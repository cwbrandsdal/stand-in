// Package insights persists per-request usage and per-credential quota observations
// to a local SQLite database and serves an analytics dashboard on top of it.
//
// Recording is passive: request rows come from the usage plugin pipeline and quota
// windows come from the signals the proxy already observes on upstream responses.
// No additional upstream requests are made. Prompt and response content is never stored.
package insights

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

const (
	// quotaSampleInterval is how often in-memory quota snapshots are checked for new observations.
	quotaSampleInterval = 30 * time.Second
	// quotaHeartbeat stores an unchanged observation at most this often, so charts show recent coverage.
	quotaHeartbeat = 10 * time.Minute
	pruneInterval  = 6 * time.Hour
)

var (
	current     atomic.Pointer[Service]
	authManager atomic.Pointer[coreauth.Manager]
)

// Service owns the insights database and its background writers.
type Service struct {
	store     *Store
	recorder  *recorder
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	labels    atomic.Pointer[map[string]string]
	retention time.Duration
	// last is only accessed by the sampler goroutine.
	last map[string]storedSample
	// backupMu keeps a manual backup from overlapping the nightly one.
	backupMu sync.Mutex
}

type storedSample struct {
	used       float64
	resetsAt   time.Time
	observedAt time.Time
	storedAt   time.Time
}

// SetAuthManager records the runtime auth manager used for account labels and quota sampling.
func SetAuthManager(manager *coreauth.Manager) {
	if manager != nil {
		authManager.Store(manager)
	}
}

// DatabasePath resolves the absolute insights database location.
// INSIGHTS_DB_PATH wins, then WRITABLE_PATH/data, then <config dir>/data.
func DatabasePath(configFilePath string) string {
	path := strings.TrimSpace(os.Getenv("INSIGHTS_DB_PATH"))
	if path == "" {
		if writable := util.WritablePath(); writable != "" {
			path = filepath.Join(writable, "data", "insights.db")
		} else if configFilePath = strings.TrimSpace(configFilePath); configFilePath != "" {
			path = filepath.Join(filepath.Dir(configFilePath), "data", "insights.db")
		}
	}
	if path == "" {
		return ""
	}
	if absolute, errAbs := filepath.Abs(path); errAbs == nil {
		return absolute
	}
	return filepath.Clean(path)
}

// Start opens the insights database and begins recording. It returns nil when disabled or
// when the database cannot be opened; the proxy keeps running either way.
func Start(configFilePath string) *Service {
	if envBool("INSIGHTS_DISABLED") {
		log.Info("insights: disabled by INSIGHTS_DISABLED")
		return nil
	}
	path := DatabasePath(configFilePath)
	if path == "" {
		log.Warn("insights: no database path available, recording disabled")
		return nil
	}
	store, errOpen := OpenStore(path)
	if errOpen != nil {
		log.Errorf("insights: recording disabled: %v", errOpen)
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	svc := &Service{
		store:    store,
		recorder: newRecorder(store),
		cancel:   cancel,
		last:     make(map[string]storedSample),
	}
	if days, errDays := strconv.Atoi(strings.TrimSpace(os.Getenv("INSIGHTS_RETENTION_DAYS"))); errDays == nil && days > 0 {
		svc.retention = time.Duration(days) * 24 * time.Hour
	}
	empty := map[string]string{}
	svc.labels.Store(&empty)

	for _, worker := range []func(context.Context){svc.recorder.run, svc.runSampler, svc.runPriceUpdater, svc.runAlerts, svc.runBackups} {
		svc.wg.Add(1)
		go func(run func(context.Context)) {
			defer svc.wg.Done()
			run(ctx)
		}(worker)
	}

	if previous := current.Swap(svc); previous != nil {
		previous.Stop()
	}
	coreusage.RegisterNamedPlugin("insights", usagePlugin{})
	log.Infof("insights: recording usage and quota history to %s", path)
	if dir := backupDir(); dir != "" {
		log.Infof("insights: nightly backups to %s after %02d:00, keeping %d", dir, backupHour(), backupKeep())
	}
	if status := lunaStatus(); status.Configured {
		log.Infof("insights: alerts go to Luna at %s", status.Server)
	} else if status.Missing != "" {
		log.Warnf("insights: alerts are not sent to Luna: %s is not set", status.Missing)
	}
	return svc
}

// Stop flushes pending rows and closes the database.
func (s *Service) Stop() {
	if s == nil {
		return
	}
	current.CompareAndSwap(s, nil)
	s.cancel()
	s.wg.Wait()
	if errClose := s.store.Close(); errClose != nil {
		log.Errorf("insights: close database: %v", errClose)
	}
}

func (s *Service) accountLabel(authID string) string {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return ""
	}
	if labels := s.labels.Load(); labels != nil {
		if label, ok := (*labels)[authID]; ok {
			return label
		}
	}
	if manager := authManager.Load(); manager != nil {
		if auth, ok := manager.GetByID(authID); ok {
			return accountLabel(auth)
		}
	}
	return ""
}

func (s *Service) runSampler(ctx context.Context) {
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	lastPrune := time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		now := time.Now().UTC()
		s.sampleQuotas(ctx, now)
		if s.retention > 0 && now.Sub(lastPrune) >= pruneInterval {
			if errPrune := s.store.pruneBefore(ctx, now.Add(-s.retention)); errPrune != nil {
				log.Warnf("insights: %v", errPrune)
			}
			lastPrune = now
		}
		timer.Reset(quotaSampleInterval)
	}
}

// sampleQuotas persists new quota observations and refreshes account identities.
func (s *Service) sampleQuotas(ctx context.Context, now time.Time) {
	manager := authManager.Load()
	if manager == nil {
		return
	}
	auths := manager.List()
	labels := make(map[string]string, len(auths))
	accounts := make([]accountRow, 0, len(auths))
	var samples []quotaRow
	for _, auth := range auths {
		if auth == nil || auth.ID == "" {
			continue
		}
		label := accountLabel(auth)
		labels[auth.ID] = label
		accounts = append(accounts, accountRow{
			AuthID:   auth.ID,
			Provider: auth.Provider,
			Label:    label,
			Email:    metadataString(auth.Metadata, "email"),
			Plan:     accountPlan(auth),
			SeenAt:   now,
		})
		for _, window := range windowsForAuth(auth) {
			key := auth.ID + "|" + window.Key
			prev, seen := s.last[key]
			if seen && !window.ObservedAt.After(prev.observedAt) {
				continue
			}
			unchanged := seen && prev.used == window.UsedPct && prev.resetsAt.Equal(window.ResetsAt)
			if unchanged && window.ObservedAt.Sub(prev.storedAt) < quotaHeartbeat {
				prev.observedAt = window.ObservedAt
				s.last[key] = prev
				continue
			}
			samples = append(samples, quotaRow{
				TS:            window.ObservedAt,
				AuthID:        auth.ID,
				Provider:      auth.Provider,
				WindowKey:     window.Key,
				UsedPct:       window.UsedPct,
				ResetsAt:      window.ResetsAt,
				WindowMinutes: window.WindowMinutes,
				Status:        window.Status,
			})
			s.last[key] = storedSample{
				used:       window.UsedPct,
				resetsAt:   window.ResetsAt,
				observedAt: window.ObservedAt,
				storedAt:   window.ObservedAt,
			}
		}
	}
	s.labels.Store(&labels)
	if errInsert := s.store.insertQuotaSamples(ctx, samples); errInsert != nil {
		log.Warnf("insights: %v", errInsert)
	}
	if errUpsert := s.store.upsertAccounts(ctx, accounts); errUpsert != nil {
		log.Warnf("insights: %v", errUpsert)
	}
}

func accountLabel(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if email := metadataString(auth.Metadata, "email"); email != "" {
		return email
	}
	if label := strings.TrimSpace(auth.Label); label != "" {
		return label
	}
	if auth.FileName != "" {
		return strings.TrimSuffix(filepath.Base(auth.FileName), filepath.Ext(auth.FileName))
	}
	return auth.ID
}

func accountPlan(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if plan := metadataString(auth.Metadata, "plan_type"); plan != "" {
		return plan
	}
	for key, value := range auth.Quota.Signals {
		if strings.EqualFold(key, "X-Codex-Plan-Type") {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func metadataString(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	if value, ok := metadata[key].(string); ok {
		return strings.TrimSpace(value)
	}
	return ""
}

func envBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
