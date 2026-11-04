package insights

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// Luna is the owner's personal assistant. Its operations sentinel takes alerts on a webhook
// (POST <core>/v1/alerts/<sourceId> with a source token, generic JSON) and speaks critical ones at the
// next pause in a call, batches warnings, pushes to the iPhone when no call is live, and folds open
// ones into its drive brief. Luna dedupes by fingerprint and escalates severity, so the hub sends one
// fingerprint per condition and a "resolved" message when the condition ends.

const (
	// lunaRetryFor is how long an alert or resolution keeps being retried while Luna is unreachable.
	lunaRetryFor        = 24 * time.Hour
	maxLunaSendsPerTick = 10
	// lunaRequestTimeout bounds one webhook call on the local network. This is not an upstream model
	// connection: a bounded wait keeps a stuck Luna from stalling the alert loop.
	lunaRequestTimeout   = 15 * time.Second
	lunaFingerprintScope = "cliproxy:"
)

// lunaTarget returns Luna's alert webhook URL (INSIGHTS_LUNA_ALERTS_URL) and source token
// (INSIGHTS_LUNA_TOKEN, passed from SecretStore by Start-Hub.ps1); ok is false unless both are set.
func lunaTarget() (target lunaEndpoint, ok bool) {
	target = lunaEndpoint{URL: strings.TrimSpace(os.Getenv("INSIGHTS_LUNA_ALERTS_URL")), Token: strings.TrimSpace(os.Getenv("INSIGHTS_LUNA_TOKEN"))}
	return target, target.URL != "" && target.Token != ""
}

type lunaEndpoint struct {
	URL   string
	Token string
}

// LunaStatus describes the Luna connection for the dashboard without revealing the token.
type LunaStatus struct {
	Configured bool   `json:"configured"`
	Server     string `json:"server,omitempty"`
	// Missing names what is not set when only half of the connection is configured.
	Missing string `json:"missing,omitempty"`
}

func lunaStatus() LunaStatus {
	target, ok := lunaTarget()
	status := LunaStatus{Configured: ok, Server: serverOf(target.URL)}
	switch {
	case target.URL == "" && target.Token != "":
		status.Missing = "INSIGHTS_LUNA_ALERTS_URL"
	case target.URL != "" && target.Token == "":
		status.Missing = "INSIGHTS_LUNA_TOKEN"
	}
	return status
}

func serverOf(raw string) string {
	parsed, errParse := url.Parse(raw)
	if errParse != nil || parsed.Host == "" {
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host
}

// lunaAlert is the generic alert JSON Luna's webhook accepts.
type lunaAlert struct {
	Title       string `json:"title"`
	Severity    string `json:"severity,omitempty"`
	Detail      string `json:"detail,omitempty"`
	URL         string `json:"url,omitempty"`
	Status      string `json:"status"`
	Fingerprint string `json:"fingerprint"`
}

// postToLuna sends one alert. The error never contains the token.
func postToLuna(ctx context.Context, target lunaEndpoint, alert lunaAlert) error {
	parsed, errParse := url.Parse(target.URL)
	if errParse != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return fmt.Errorf("INSIGHTS_LUNA_ALERTS_URL must be an http(s) URL")
	}
	if dashboard := strings.TrimSpace(os.Getenv("INSIGHTS_DASHBOARD_URL")); dashboard != "" && alert.URL == "" {
		alert.URL = dashboard
	}
	payload, errMarshal := json.Marshal(alert)
	if errMarshal != nil {
		return errMarshal
	}
	ctx, cancel := context.WithTimeout(ctx, lunaRequestTimeout)
	defer cancel()
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, parsed.String(), bytes.NewReader(payload))
	if errRequest != nil {
		return fmt.Errorf("build Luna request: %w", unwrapURLError(errRequest))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+target.Token)
	resp, errDo := http.DefaultClient.Do(req)
	if errDo != nil {
		return fmt.Errorf("reach Luna: %w", unwrapURLError(errDo))
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if errClose := resp.Body.Close(); errClose != nil {
		log.Debugf("insights: close Luna response: %v", errClose)
	}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode <= 299:
		return nil
	case resp.StatusCode == http.StatusUnauthorized:
		return fmt.Errorf("Luna rejected the source token (HTTP 401)")
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("Luna does not know this alert source (HTTP 404); check INSIGHTS_LUNA_ALERTS_URL")
	}
	return fmt.Errorf("Luna answered HTTP %d", resp.StatusCode)
}

func unwrapURLError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		return urlErr.Err
	}
	return err
}

type pendingLuna struct {
	id          int64
	fingerprint string
	severity    string
	title       string
	body        string
	attempts    int
}

// deliverToLuna sends new alerts, then resolutions of alerts Luna already has. Failures are retried
// every check for lunaRetryFor; only the first failure of each alert is logged.
func (s *Service) deliverToLuna(ctx context.Context, now time.Time) {
	target, configured := lunaTarget()
	cutoff := now.Add(-lunaRetryFor).UnixMilli()
	if _, errExec := s.store.db.ExecContext(ctx, `UPDATE alerts SET push_status = ? WHERE push_status = ? AND ts_ms < ?`,
		pushStatusFailed, pushStatusPending, cutoff); errExec != nil {
		log.Warnf("insights: expire Luna deliveries: %v", errExec)
	}
	if _, errExec := s.store.db.ExecContext(ctx, `UPDATE alerts SET resolve_status = ? WHERE resolve_status = ? AND resolved_ms < ?`,
		pushStatusFailed, pushStatusPending, cutoff); errExec != nil {
		log.Warnf("insights: expire Luna resolutions: %v", errExec)
	}

	firing, errFiring := s.store.pendingLuna(ctx, `SELECT id, COALESCE(fingerprint, dedupe_key), severity, title, body, push_attempts
		FROM alerts WHERE push_status = ? ORDER BY id LIMIT ?`, pushStatusPending, maxLunaSendsPerTick)
	if errFiring != nil {
		log.Warnf("insights: %v", errFiring)
		return
	}
	for _, item := range firing {
		status, message := pushStatusSent, ""
		if !configured {
			status, message = pushStatusOff, "Luna is not configured"
		} else if errSend := postToLuna(ctx, target, lunaAlert{
			Title: item.title, Severity: item.severity, Detail: item.body, Status: "firing", Fingerprint: lunaFingerprintScope + item.fingerprint,
		}); errSend != nil {
			if ctx.Err() != nil {
				return
			}
			status, message = pushStatusPending, errSend.Error()
			if item.attempts == 0 {
				log.Warnf("insights: send alert %d to Luna (retrying for %s): %s", item.id, shortDuration(lunaRetryFor), message)
			}
		}
		if _, errExec := s.store.db.ExecContext(ctx, `UPDATE alerts SET push_status = ?, push_error = ?, push_attempts = push_attempts + 1 WHERE id = ?`,
			status, message, item.id); errExec != nil {
			log.Warnf("insights: record Luna delivery: %v", errExec)
		}
	}

	// One resolution per fingerprint. With a single MAX() aggregate SQLite takes title from that row, the
	// newest; Luna keeps the title it already has anyway.
	resolved, errResolved := s.store.pendingLuna(ctx, `SELECT MAX(id), fingerprint, '', title, '', 0
		FROM alerts WHERE resolve_status = ? GROUP BY fingerprint ORDER BY MAX(id) LIMIT ?`, pushStatusPending, maxLunaSendsPerTick)
	if errResolved != nil {
		log.Warnf("insights: %v", errResolved)
		return
	}
	for _, item := range resolved {
		status := pushStatusSent
		if !configured {
			status = pushStatusOff
		} else if errSend := postToLuna(ctx, target, lunaAlert{
			Title: item.title, Status: "resolved", Fingerprint: lunaFingerprintScope + item.fingerprint,
		}); errSend != nil {
			if ctx.Err() != nil {
				return
			}
			continue // retried next check until lunaRetryFor passes
		}
		if _, errExec := s.store.db.ExecContext(ctx, `UPDATE alerts SET resolve_status = ? WHERE fingerprint = ? AND resolve_status = ?`,
			status, item.fingerprint, pushStatusPending); errExec != nil {
			log.Warnf("insights: record Luna resolution: %v", errExec)
		}
	}
}

func (s *Store) pendingLuna(ctx context.Context, query, status string, limit int) ([]pendingLuna, error) {
	rows, errQuery := s.db.QueryContext(ctx, query, status, limit)
	if errQuery != nil {
		return nil, fmt.Errorf("insights: pending Luna deliveries: %w", errQuery)
	}
	var out []pendingLuna
	for rows.Next() {
		var item pendingLuna
		if errScan := rows.Scan(&item.id, &item.fingerprint, &item.severity, &item.title, &item.body, &item.attempts); errScan != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("insights: scan Luna delivery: %w", errScan)
		}
		out = append(out, item)
	}
	return out, closeRows(rows)
}

// sendLunaTest sends an info alert, which Luna lists on its Work screen without speaking or pushing it.
func sendLunaTest(ctx context.Context, now time.Time) error {
	target, configured := lunaTarget()
	if !configured {
		return fmt.Errorf("Luna is not configured: set INSIGHTS_LUNA_ALERTS_URL in .env and the CLIProxyAPI hub alert token (see ops/windows/README.md)")
	}
	return postToLuna(ctx, target, lunaAlert{
		Title:       "Test alert from the CLIProxyAPI hub",
		Severity:    severityInfo,
		Detail:      "The insights dashboard reached Luna. Acknowledge this under Alerts on the Work screen.",
		Status:      "firing",
		Fingerprint: lunaFingerprintScope + "test|" + strconv.FormatInt(now.UnixMilli(), 10),
	})
}
