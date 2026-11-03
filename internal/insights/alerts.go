package insights

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// Alerts flag quota windows that are close to running out and patterns that waste quota. Every
// alert is stored once (deduplicated by key) and shown on the dashboard. Limit, sign-in and
// fast-mode alerts are also sent to Luna's alert webhook (see luna.go), which speaks or pushes them;
// alerts resolve themselves when their window resets or the cause goes away, and Luna is told.

const (
	alertCheckInterval  = time.Minute
	alertFirstCheck     = 20 * time.Second
	alertCheckOverlap   = 2 * time.Minute
	alertFirstLookback  = 10 * time.Minute
	maxAlertsListed     = 500
	maxAlertTextLength  = 600
	quotaPeriodSlack    = 15 * time.Minute
	minProjectionSpan   = 10 * time.Minute
	shortWindowLookback = time.Hour
	longWindowLookback  = 6 * time.Hour
	authAlertRepeat     = 6 * time.Hour
	// fastModeQuiet is how long a machine must go without fast-mode refusals before the alert resolves.
	fastModeQuiet = time.Hour
	// unknownResetLifetime resolves a limit alert for a window that reports no reset time.
	unknownResetLifetime = 24 * time.Hour

	metaAlertSettings  = "alert_settings"
	metaAlertsChecked  = "alerts_checked_ms"
	pushStatusPending  = "pending"
	pushStatusSent     = "sent"
	pushStatusFailed   = "failed"
	pushStatusOff      = "off"
	alertGroupLimits   = "limits"
	alertGroupWaste    = "waste"
	severityInfo       = "info"
	severityWarning    = "warning"
	severityCritical   = "critical"
	alertQuota         = "quota"
	alertQuotaExceeded = "quota_exhausted"
	alertProjection    = "projection"
	alertUpstreamAuth  = "upstream_auth"
	alertFirstWrite    = "first_write"
	alertFastMode      = "fast_mode"
	alertAccountMoves  = "account_moves"
)

// AlertSettings controls which alerts are raised and sent to Luna. They are stored in the meta table
// and edited on the dashboard with the management key.
type AlertSettings struct {
	// QuotaPct raises an alert when an account uses this share of a quota window; 0 turns it off.
	QuotaPct float64 `json:"quota_pct"`
	// ProjectionMinPct raises an alert when a window that has used at least this share is projected
	// to run out before it resets; 0 turns projections off.
	ProjectionMinPct float64 `json:"projection_min_pct"`
	// FastModeRefusals raises an alert when one machine and tool collect this many fast-mode
	// refusals within a clock hour; 0 turns it off.
	FastModeRefusals int64 `json:"fast_mode_refusals"`
	// WasteAlerts raises alerts for large first cache writes, fast-mode refusals and account moves.
	WasteAlerts bool `json:"waste_alerts"`
	// LunaLimits sends quota, projection and sign-in alerts to Luna.
	LunaLimits bool `json:"luna_limits"`
	// LunaWaste sends fast-mode refusal alerts to Luna. Large first cache writes and account moves stay
	// on the dashboard; Luna's brief reads them from the waste report instead.
	LunaWaste bool `json:"luna_waste"`
}

func defaultAlertSettings() AlertSettings {
	return AlertSettings{QuotaPct: 80, ProjectionMinPct: 50, FastModeRefusals: 5, WasteAlerts: true, LunaLimits: true, LunaWaste: true}
}

func (a AlertSettings) validate() error {
	if math.IsNaN(a.QuotaPct) || a.QuotaPct < 0 || a.QuotaPct > 100 {
		return fmt.Errorf("quota_pct must be between 0 and 100")
	}
	if math.IsNaN(a.ProjectionMinPct) || a.ProjectionMinPct < 0 || a.ProjectionMinPct > 100 {
		return fmt.Errorf("projection_min_pct must be between 0 and 100")
	}
	if a.FastModeRefusals < 0 || a.FastModeRefusals > 100000 {
		return fmt.Errorf("fast_mode_refusals must be between 0 and 100000")
	}
	return nil
}

// sendsToLuna reports whether alerts of this kind go to Luna.
func (a AlertSettings) sendsToLuna(kind string) bool {
	switch kind {
	case alertQuota, alertQuotaExceeded, alertProjection, alertUpstreamAuth:
		return a.LunaLimits
	case alertFastMode:
		return a.LunaWaste
	}
	return false
}

// alertGroup sorts alert kinds into limits and waste.
func alertGroup(kind string) string {
	switch kind {
	case alertQuota, alertQuotaExceeded, alertProjection, alertUpstreamAuth:
		return alertGroupLimits
	}
	return alertGroupWaste
}

func (s *Store) alertSettings(ctx context.Context) (AlertSettings, error) {
	settings := defaultAlertSettings()
	raw, errRead := s.metaValue(ctx, metaAlertSettings)
	if errRead != nil || raw == "" {
		return settings, errRead
	}
	if errUnmarshal := json.Unmarshal([]byte(raw), &settings); errUnmarshal != nil {
		return defaultAlertSettings(), fmt.Errorf("insights: parse alert settings: %w", errUnmarshal)
	}
	return settings, nil
}

func (s *Store) saveAlertSettings(ctx context.Context, settings AlertSettings) error {
	if errValidate := settings.validate(); errValidate != nil {
		return errValidate
	}
	raw, errMarshal := json.Marshal(settings)
	if errMarshal != nil {
		return fmt.Errorf("insights: encode alert settings: %w", errMarshal)
	}
	return s.setMetaValue(ctx, metaAlertSettings, string(raw))
}

// alertRow is an alert before it is stored.
type alertRow struct {
	TS        time.Time
	DedupeKey string
	// Fingerprint names the condition rather than one observation of it: the 80% and 100% alerts of a
	// window period share one, so Luna updates one alert instead of opening a second. Empty means the
	// dedupe key.
	Fingerprint string
	// ResolvesAt resolves the alert by time, for example when its quota window resets; zero means the
	// alert resolves by condition or not at all.
	ResolvesAt time.Time
	Kind       string
	Severity   string
	Provider   string
	AuthID     string
	WindowKey  string
	Title      string
	Body       string
}

// Alert is one stored alert.
type Alert struct {
	ID        int64  `json:"id"`
	TS        int64  `json:"ts"`
	Kind      string `json:"kind"`
	Group     string `json:"group"`
	Severity  string `json:"severity"`
	Provider  string `json:"provider"`
	AuthID    string `json:"auth_id"`
	WindowKey string `json:"window_key"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	// PushStatus is the delivery to Luna: pending, sent, failed or off.
	PushStatus string `json:"push_status"`
	PushError  string `json:"push_error,omitempty"`
	AckedMs    int64  `json:"acked_ms,omitempty"`
	ResolvedMs int64  `json:"resolved_ms,omitempty"`
}

// insertAlert stores an alert unless one with the same dedupe key exists; it reports whether it was new.
// A new alert also postpones the time-based resolution of open alerts with the same fingerprint, so a
// condition that keeps recurring is not resolved under a newer alert.
func (s *Store) insertAlert(ctx context.Context, row alertRow, pushStatus string) (bool, error) {
	fingerprint := firstNonEmpty(row.Fingerprint, row.DedupeKey)
	var resolves any
	if !row.ResolvesAt.IsZero() {
		resolves = row.ResolvesAt.UnixMilli()
	}
	result, errExec := s.db.ExecContext(ctx, `INSERT INTO alerts
		(ts_ms, dedupe_key, fingerprint, resolves_ms, kind, severity, provider, auth_id, window_key, title, body, push_status)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(dedupe_key) DO NOTHING`,
		row.TS.UnixMilli(), row.DedupeKey, fingerprint, resolves, row.Kind, row.Severity, row.Provider, row.AuthID, row.WindowKey,
		truncateText(row.Title, maxAlertTextLength), truncateText(row.Body, maxAlertTextLength), pushStatus)
	if errExec != nil {
		return false, fmt.Errorf("insights: insert alert: %w", errExec)
	}
	affected, errAffected := result.RowsAffected()
	if errAffected != nil {
		return false, fmt.Errorf("insights: insert alert: %w", errAffected)
	}
	if affected > 0 && resolves != nil {
		if _, errExec = s.db.ExecContext(ctx, `UPDATE alerts SET resolves_ms = MAX(resolves_ms, ?)
			WHERE fingerprint = ? AND resolved_ms IS NULL AND resolves_ms IS NOT NULL`, resolves, fingerprint); errExec != nil {
			return true, fmt.Errorf("insights: extend alert resolution: %w", errExec)
		}
	}
	return affected > 0, nil
}

// alerts lists stored alerts, newest first, optionally only those with an id below beforeID.
func (s *Store) alerts(ctx context.Context, beforeID int64, limit int) ([]Alert, error) {
	if beforeID <= 0 {
		beforeID = math.MaxInt64
	}
	rows, errQuery := s.db.QueryContext(ctx, `SELECT id, ts_ms, kind, severity, COALESCE(provider,''), COALESCE(auth_id,''),
			COALESCE(window_key,''), title, body, push_status, push_error, COALESCE(acked_ms,0), COALESCE(resolved_ms,0)
		FROM alerts WHERE id < ? ORDER BY id DESC LIMIT ?`, beforeID, limit)
	if errQuery != nil {
		return nil, fmt.Errorf("insights: alerts: %w", errQuery)
	}
	var out []Alert
	for rows.Next() {
		var alert Alert
		if errScan := rows.Scan(&alert.ID, &alert.TS, &alert.Kind, &alert.Severity, &alert.Provider, &alert.AuthID,
			&alert.WindowKey, &alert.Title, &alert.Body, &alert.PushStatus, &alert.PushError, &alert.AckedMs, &alert.ResolvedMs); errScan != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("insights: scan alert: %w", errScan)
		}
		alert.Group = alertGroup(alert.Kind)
		out = append(out, alert)
	}
	return out, closeRows(rows)
}

// openAlertCount counts alerts from the last week that are neither seen nor resolved.
func (s *Store) openAlertCount(ctx context.Context, now time.Time) (int64, error) {
	var count int64
	if errScan := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM alerts WHERE acked_ms IS NULL AND resolved_ms IS NULL AND ts_ms >= ?`,
		now.Add(-7*24*time.Hour).UnixMilli()).Scan(&count); errScan != nil {
		return 0, fmt.Errorf("insights: count open alerts: %w", errScan)
	}
	return count, nil
}

// ackAlerts marks one alert, or every open alert when id is 0, as seen.
func (s *Store) ackAlerts(ctx context.Context, id int64, now time.Time) (int64, error) {
	query, args := `UPDATE alerts SET acked_ms = ? WHERE acked_ms IS NULL`, []any{now.UnixMilli()}
	if id > 0 {
		query += ` AND id = ?`
		args = append(args, id)
	}
	result, errExec := s.db.ExecContext(ctx, query, args...)
	if errExec != nil {
		return 0, fmt.Errorf("insights: acknowledge alerts: %w", errExec)
	}
	affected, errAffected := result.RowsAffected()
	if errAffected != nil {
		return 0, fmt.Errorf("insights: acknowledge alerts: %w", errAffected)
	}
	return affected, nil
}

type quotaPoint struct {
	TS       time.Time
	UsedPct  float64
	ResetsAt time.Time
}

// quotaUsedAt returns the latest stored observation of a window at or before at.
func (s *Store) quotaUsedAt(ctx context.Context, authID, windowKey string, at time.Time) (quotaPoint, bool, error) {
	var ts, resets int64
	var point quotaPoint
	errScan := s.db.QueryRowContext(ctx, `SELECT ts_ms, used_pct, resets_at_ms FROM quota_samples
		WHERE auth_id = ? AND window_key = ? AND ts_ms <= ? ORDER BY ts_ms DESC LIMIT 1`,
		authID, windowKey, at.UnixMilli()).Scan(&ts, &point.UsedPct, &resets)
	if errors.Is(errScan, sql.ErrNoRows) {
		return point, false, nil
	}
	if errScan != nil {
		return point, false, fmt.Errorf("insights: quota history: %w", errScan)
	}
	point.TS = time.UnixMilli(ts).UTC()
	if resets > 0 {
		point.ResetsAt = time.UnixMilli(resets).UTC()
	}
	return point, true, nil
}

// projection estimates when a window reaches 100% at its recent burn rate.
type projection struct {
	PerHour  float64
	HitAt    time.Time
	Lookback time.Duration
}

// projectionLookback is how far back the burn rate is measured: an hour for short windows such as
// the 5-hour window, six hours for weekly windows.
func projectionLookback(windowMinutes int64) time.Duration {
	if length := time.Duration(windowMinutes) * time.Minute; length > 0 && length <= longWindowLookback {
		return shortWindowLookback
	}
	return longWindowLookback
}

// projectWindow measures the burn since the window's value at at-lookback (prev), or since the window
// started when prev belongs to an earlier window or is missing. ok is false when the window cannot
// be projected or does not run out before it resets.
func projectWindow(w QuotaWindow, prev quotaPoint, havePrev bool, at time.Time) (projection, bool) {
	if w.ResetsAt.IsZero() || w.WindowMinutes <= 0 || w.UsedPct >= 100 || !w.ResetsAt.After(at) {
		return projection{}, false
	}
	length := time.Duration(w.WindowMinutes) * time.Minute
	start := w.ResetsAt.Add(-length)
	// Without a stored value from this window period, measure the average burn since it started.
	baseTime, baseUsed := start, 0.0
	if havePrev && !prev.ResetsAt.IsZero() && absDuration(prev.ResetsAt.Sub(w.ResetsAt)) <= quotaPeriodSlack {
		// prev is the latest sample at or before at-lookback, so it is the value at that moment.
		baseTime, baseUsed = at.Add(-projectionLookback(w.WindowMinutes)), prev.UsedPct
	}
	if baseTime.Before(start) {
		baseTime, baseUsed = start, 0
	}
	span := at.Sub(baseTime)
	if span < minProjectionSpan {
		return projection{}, false
	}
	perHour := (w.UsedPct - baseUsed) / span.Hours()
	if perHour <= 0 {
		return projection{}, false
	}
	hitAt := at.Add(time.Duration((100 - w.UsedPct) / perHour * float64(time.Hour)))
	if !hitAt.Before(w.ResetsAt) {
		return projection{}, false
	}
	return projection{PerHour: perHour, HitAt: hitAt, Lookback: at.Sub(baseTime)}, true
}

// quotaAlerts evaluates one account's live windows. history returns the stored value at a time. named
// adds the account to titles, for providers with more than one account.
func quotaAlerts(settings AlertSettings, provider, authID, account string, named bool, windows []QuotaWindow, now time.Time,
	history func(windowKey string, at time.Time) (quotaPoint, bool)) []alertRow {
	var out []alertRow
	name := providerTitle(provider)
	suffix := ""
	if named && account != "" {
		suffix = " (" + account + ")"
	}
	for _, w := range windows {
		at := w.ObservedAt
		if at.IsZero() {
			at = now
		}
		if !w.ResetsAt.IsZero() && !w.ResetsAt.After(now) {
			continue // the window has reset since this observation
		}
		if w.WindowMinutes > 0 && now.Sub(at) > time.Duration(w.WindowMinutes)*time.Minute {
			continue
		}
		period := quotaPeriodKey(w, at)
		label := strings.ToLower(firstNonEmpty(w.Label, w.Key))
		resets := resetText(w.ResetsAt, now)
		resolvesAt := w.ResetsAt
		if resolvesAt.IsZero() {
			resolvesAt = at.Add(unknownResetLifetime)
		}
		// One fingerprint per window period: on pace, 80% and 100% are the same alert getting worse.
		base := alertRow{TS: now, Provider: provider, AuthID: authID, WindowKey: w.Key, ResolvesAt: resolvesAt,
			Fingerprint: strings.Join([]string{"limit", authID, w.Key, period}, "|")}
		var proj projection
		projected := false
		if settings.ProjectionMinPct > 0 && w.UsedPct >= settings.ProjectionMinPct && history != nil {
			prev, ok := history(w.Key, at.Add(-projectionLookback(w.WindowMinutes)))
			proj, projected = projectWindow(w, prev, ok, at)
		}
		used := fmt.Sprintf("Account %s has used %s of its %s window. %s", account, fmtPct(w.UsedPct), label, resets)
		switch {
		case w.UsedPct >= 100:
			row := base
			row.Kind, row.Severity = alertQuotaExceeded, severityCritical
			row.DedupeKey = strings.Join([]string{alertQuotaExceeded, authID, w.Key, period}, "|")
			row.Title = fmt.Sprintf("%s %s limit reached%s", name, label, suffix)
			row.Body = used
			out = append(out, row)
		case settings.QuotaPct > 0 && w.UsedPct >= settings.QuotaPct:
			row := base
			row.Kind, row.Severity = alertQuota, severityWarning
			row.DedupeKey = strings.Join([]string{alertQuota, authID, w.Key, period, strconv.FormatFloat(settings.QuotaPct, 'f', -1, 64)}, "|")
			row.Title = fmt.Sprintf("%s %s limit at %s%s", name, label, fmtPct(w.UsedPct), suffix)
			row.Body = used
			if projected {
				row.Body += " " + projectionText(proj, w.ResetsAt, now)
			}
			out = append(out, row)
		case projected:
			row := base
			row.Kind, row.Severity = alertProjection, severityWarning
			row.DedupeKey = strings.Join([]string{alertProjection, authID, w.Key, period}, "|")
			row.Title = fmt.Sprintf("%s %s limit on pace to run out%s", name, label, suffix)
			row.Body = fmt.Sprintf("Account %s is at %s of its %s window. %s", account, fmtPct(w.UsedPct), label, projectionText(proj, w.ResetsAt, now))
			out = append(out, row)
		}
	}
	return out
}

func projectionText(p projection, resetsAt, now time.Time) string {
	return fmt.Sprintf("At the burn rate of the last %s (%.1f%%/h) it reaches 100%% in about %s, %s before the window resets.",
		shortDuration(p.Lookback), p.PerHour, shortDuration(p.HitAt.Sub(now)), shortDuration(resetsAt.Sub(p.HitAt)))
}

func resetText(resetsAt, now time.Time) string {
	if resetsAt.IsZero() {
		return "No reset time reported."
	}
	return fmt.Sprintf("Resets %s (in %s).", resetsAt.In(time.Local).Format("Mon 2 Jan 15:04"), shortDuration(resetsAt.Sub(now)))
}

// quotaPeriodKey identifies one window period. Codex reports some resets relative to the observation,
// so the reset time is rounded to the hour.
func quotaPeriodKey(w QuotaWindow, at time.Time) string {
	if w.ResetsAt.IsZero() {
		return "d" + at.In(time.Local).Format("20060102")
	}
	return "r" + strconv.FormatInt(w.ResetsAt.Add(30*time.Minute).Truncate(time.Hour).Unix(), 10)
}

// checkAlerts raises new alerts and sends pending pushes.
func (s *Service) checkAlerts(ctx context.Context, now time.Time) {
	settings, errSettings := s.store.alertSettings(ctx)
	if errSettings != nil {
		log.Warnf("insights: %v", errSettings)
	}
	rows := s.liveQuotaAlerts(ctx, settings, now)
	from := now.Add(-alertFirstLookback)
	if raw, errMeta := s.store.metaValue(ctx, metaAlertsChecked); errMeta == nil && raw != "" {
		if checked, errParse := strconv.ParseInt(raw, 10, 64); errParse == nil {
			from = time.UnixMilli(checked).UTC().Add(-alertCheckOverlap)
		}
	}
	if earliest := now.Add(-24 * time.Hour); from.Before(earliest) {
		from = earliest
	}
	requestRows, errRequests := s.store.requestAlerts(ctx, settings, from, now)
	if errRequests != nil {
		log.Warnf("insights: %v", errRequests)
	} else if errMeta := s.store.setMetaValue(ctx, metaAlertsChecked, strconv.FormatInt(now.UnixMilli(), 10)); errMeta != nil {
		log.Warnf("insights: %v", errMeta)
	}
	rows = append(rows, requestRows...)
	_, configured := lunaTarget()
	for _, row := range rows {
		status := pushStatusOff
		if configured && settings.sendsToLuna(row.Kind) {
			status = pushStatusPending
		}
		inserted, errInsert := s.store.insertAlert(ctx, row, status)
		if errInsert != nil {
			log.Warnf("insights: %v", errInsert)
			continue
		}
		if inserted {
			log.Infof("insights: alert %s: %s", row.Kind, row.Title)
		}
	}
	if errResolve := s.store.resolveAlerts(ctx, now); errResolve != nil {
		log.Warnf("insights: %v", errResolve)
	}
	s.deliverToLuna(ctx, now)
}

// resolveAlerts closes open alerts whose cause has gone: the quota window reset (or the fast-mode quiet
// period passed), or the account served a request again after a sign-in failure. Resolutions of
// alerts Luna already has are queued for delivery; alerts still waiting for delivery are dropped.
func (s *Store) resolveAlerts(ctx context.Context, now time.Time) error {
	const resolve = `UPDATE alerts SET resolved_ms = ?,
			resolve_status = CASE WHEN push_status = 'sent' THEN 'pending' ELSE '' END,
			push_error = CASE WHEN push_status = 'pending' THEN 'resolved before delivery' ELSE push_error END,
			push_status = CASE WHEN push_status = 'pending' THEN 'off' ELSE push_status END
		WHERE resolved_ms IS NULL AND `
	nowMs := now.UnixMilli()
	if _, errExec := s.db.ExecContext(ctx, resolve+`resolves_ms IS NOT NULL AND resolves_ms <= ?`, nowMs, nowMs); errExec != nil {
		return fmt.Errorf("insights: resolve alerts: %w", errExec)
	}
	if _, errExec := s.db.ExecContext(ctx, resolve+`kind = ? AND COALESCE(auth_id,'') <> '' AND EXISTS (
			SELECT 1 FROM requests r WHERE r.auth_id = alerts.auth_id AND r.failed = 0 AND r.ts_ms > alerts.ts_ms)`,
		nowMs, alertUpstreamAuth); errExec != nil {
		return fmt.Errorf("insights: resolve sign-in alerts: %w", errExec)
	}
	return nil
}

func (s *Service) liveQuotaAlerts(ctx context.Context, settings AlertSettings, now time.Time) []alertRow {
	manager := authManager.Load()
	if manager == nil || (settings.QuotaPct <= 0 && settings.ProjectionMinPct <= 0) {
		return nil
	}
	auths := manager.List()
	perProvider := make(map[string]int)
	for _, auth := range auths {
		if auth != nil && auth.ID != "" && !auth.Disabled {
			perProvider[auth.Provider]++
		}
	}
	var out []alertRow
	for _, auth := range auths {
		if auth == nil || auth.ID == "" || auth.Disabled || !coreauth.ProviderSupportsQuotaObservation(auth.Provider) {
			continue
		}
		authID := auth.ID
		history := func(windowKey string, at time.Time) (quotaPoint, bool) {
			point, ok, errHistory := s.store.quotaUsedAt(ctx, authID, windowKey, at)
			if errHistory != nil {
				log.Warnf("insights: %v", errHistory)
				return quotaPoint{}, false
			}
			return point, ok
		}
		out = append(out, quotaAlerts(settings, auth.Provider, authID, accountLabel(auth), perProvider[auth.Provider] > 1, windowsForAuth(auth), now, history)...)
	}
	return out
}

// requestAlerts raises sign-in failure and waste alerts for requests recorded in [from, to).
func (s *Store) requestAlerts(ctx context.Context, settings AlertSettings, from, to time.Time) ([]alertRow, error) {
	var out []alertRow
	labels := hostLabels(os.Getenv("INSIGHTS_HOST_LABELS"))
	host := func(ip string) string {
		if name := labels[ip]; name != "" {
			return name
		}
		if ip == "" {
			return "an unknown machine"
		}
		return ip
	}

	rows, errQuery := s.db.QueryContext(ctx, `SELECT provider, COALESCE(auth_id,''), COALESCE(MAX(account),''), COUNT(*), MAX(ts_ms)
		FROM requests WHERE error_class = ? AND ts_ms >= ? AND ts_ms < ? GROUP BY provider, auth_id`,
		errorClassUpstreamAuth, from.UnixMilli(), to.UnixMilli())
	if errQuery != nil {
		return nil, fmt.Errorf("insights: sign-in failures: %w", errQuery)
	}
	for rows.Next() {
		var provider, authID, account string
		var count, lastMs int64
		if errScan := rows.Scan(&provider, &authID, &account, &count, &lastMs); errScan != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("insights: scan sign-in failures: %w", errScan)
		}
		last := time.UnixMilli(lastMs).UTC()
		who := firstNonEmpty(account, authID, "an unidentified account")
		out = append(out, alertRow{
			TS: to, Kind: alertUpstreamAuth, Severity: severityCritical, Provider: provider, AuthID: authID,
			DedupeKey:   strings.Join([]string{alertUpstreamAuth, provider, authID, strconv.FormatInt(last.Truncate(authAlertRepeat).Unix(), 10)}, "|"),
			Fingerprint: strings.Join([]string{"signin", provider, authID}, "|"),
			Title:       providerTitle(provider) + " sign-in failing",
			Body: fmt.Sprintf("%d %s to %s failed with an authentication error, the last at %s. Sign in again for this account on the hub's management page.",
				count, plural(count, "request", "requests"), who, last.In(time.Local).Format("15:04")),
		})
	}
	if errClose := closeRows(rows); errClose != nil {
		return nil, errClose
	}
	if !settings.WasteAlerts {
		return out, nil
	}

	threshold := int64(envInt("INSIGHTS_WASTE_FIRST_WRITE", defaultFirstWriteTokens))
	firstWrites, errFirst := s.firstWrites(ctx, from, to, threshold)
	if errFirst != nil {
		return nil, errFirst
	}
	for _, item := range firstWrites {
		severity := severityInfo
		if item.HarnessWarning {
			severity = severityWarning
		}
		body := fmt.Sprintf("A new %s thread on %s wrote %s tokens to the prompt cache on its first request", item.Harness, host(item.ClientIP), compactCount(item.CacheWriteTokens))
		if item.CostUSD > 0 {
			body += fmt.Sprintf(" (about $%.2f at API prices)", item.CostUSD)
		}
		body += "."
		if item.HarnessWarning {
			body += " The Codex harness sends its whole tool catalog with Claude models; Claude Code starts near 30K."
		}
		out = append(out, alertRow{
			TS: to, Kind: alertFirstWrite, Severity: severity, DedupeKey: alertFirstWrite + "|" + item.SessionID,
			Title: fmt.Sprintf("Large first cache write: %s tokens", compactCount(item.CacheWriteTokens)),
			Body:  body,
		})
	}

	if settings.FastModeRefusals > 0 {
		hour := to.Truncate(time.Hour)
		refusals, errFast := s.fastModeRefusals(ctx, hour, to)
		if errFast != nil {
			return nil, errFast
		}
		for _, item := range refusals {
			if item.Refusals < settings.FastModeRefusals {
				continue
			}
			out = append(out, alertRow{
				TS: to, Kind: alertFastMode, Severity: severityWarning, Provider: "claude",
				DedupeKey:   strings.Join([]string{alertFastMode, item.ClientIP, item.ClientKind, strconv.FormatInt(hour.Unix(), 10)}, "|"),
				Fingerprint: strings.Join([]string{"fastmode", item.ClientIP, item.ClientKind}, "|"),
				ResolvesAt:  hour.Add(time.Hour + fastModeQuiet),
				Title:       fmt.Sprintf("Fast mode refused %d %s on %s", item.Refusals, plural(item.Refusals, "time", "times"), host(item.ClientIP)),
				Body: fmt.Sprintf("Claude refused %d fast-mode %s from %s on %s since %s; each refusal costs a round trip before the retry at standard speed. Fast mode needs usage credits on the account, so turn it off in that tool.",
					item.Refusals, plural(item.Refusals, "request", "requests"), item.ClientKind, host(item.ClientIP), hour.In(time.Local).Format("15:04")),
			})
		}
	}

	moves, errMoves := s.accountMoves(ctx, from, to)
	if errMoves != nil {
		return nil, errMoves
	}
	type moveGroup struct {
		provider string
		hour     time.Time
		sessions map[string]bool
		causes   map[string]bool
		tokens   int64
		extra    float64
	}
	groups := make(map[string]*moveGroup)
	var order []string
	for _, move := range moves {
		hour := time.UnixMilli(move.TS).UTC().Truncate(time.Hour)
		key := move.Provider + "|" + strconv.FormatInt(hour.Unix(), 10)
		group := groups[key]
		if group == nil {
			group = &moveGroup{provider: move.Provider, hour: hour, sessions: map[string]bool{}, causes: map[string]bool{}}
			groups[key] = group
			order = append(order, key)
		}
		group.sessions[move.SessionID] = true
		group.causes[move.Cause] = true
		group.tokens += move.CacheWriteTokens
		group.extra += move.ExtraUSD
	}
	for _, key := range order {
		group := groups[key]
		causes := make([]string, 0, len(group.causes))
		for cause := range group.causes {
			causes = append(causes, cause)
		}
		sort.Strings(causes)
		count := int64(len(group.sessions))
		body := fmt.Sprintf("%d %s moved to another account (%s) and wrote %s tokens of prompt cache again",
			count, plural(count, "session", "sessions"), strings.Join(causes, ", "), compactCount(group.tokens))
		if group.extra > 0 {
			body += fmt.Sprintf(", about $%.2f more than reading it", group.extra)
		}
		body += "."
		out = append(out, alertRow{
			TS: to, Kind: alertAccountMoves, Severity: severityInfo, Provider: group.provider,
			DedupeKey: alertAccountMoves + "|" + key,
			Title:     fmt.Sprintf("%s %s moved between accounts", providerTitle(group.provider), plural(count, "session", "sessions")),
			Body:      body,
		})
	}
	return out, nil
}

func (s *Service) runAlerts(ctx context.Context) {
	timer := time.NewTimer(alertFirstCheck)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		s.checkAlerts(ctx, time.Now().UTC())
		timer.Reset(alertCheckInterval)
	}
}

func truncateText(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

func providerTitle(provider string) string {
	switch strings.ToLower(provider) {
	case "claude":
		return "Claude"
	case "codex":
		return "Codex"
	case "":
		return "Unknown provider"
	}
	return strings.ToUpper(provider[:1]) + provider[1:]
}

func plural(n int64, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func fmtPct(v float64) string { return strconv.FormatFloat(math.Round(v), 'f', 0, 64) + "%" }

func compactCount(n int64) string {
	value := float64(n)
	switch {
	case n >= 1_000_000:
		return strconv.FormatFloat(value/1e6, 'f', 1, 64) + "M"
	case n >= 10_000:
		return strconv.FormatFloat(value/1e3, 'f', 0, 64) + "K"
	case n >= 1_000:
		return strconv.FormatFloat(value/1e3, 'f', 1, 64) + "K"
	}
	return strconv.FormatInt(n, 10)
}

// shortDuration renders a duration as "2d 4h", "3h 10m" or "25m".
func shortDuration(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	minutes := int64(d.Round(time.Minute) / time.Minute)
	if minutes < 60 {
		return strconv.FormatInt(minutes, 10) + "m"
	}
	hours, minutes := minutes/60, minutes%60
	if hours < 24 {
		if minutes == 0 {
			return strconv.FormatInt(hours, 10) + "h"
		}
		return strconv.FormatInt(hours, 10) + "h " + strconv.FormatInt(minutes, 10) + "m"
	}
	days, hours := hours/24, hours%24
	if hours == 0 {
		return strconv.FormatInt(days, 10) + "d"
	}
	return strconv.FormatInt(days, 10) + "d " + strconv.FormatInt(hours, 10) + "h"
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
