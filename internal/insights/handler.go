package insights

import (
	_ "embed"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

//go:embed dashboard.html
var dashboardHTML []byte

const (
	defaultRange   = 7 * 24 * time.Hour
	maxRange       = 400 * 24 * time.Hour
	minBucket      = 5 * time.Minute
	maxBuckets     = 2000
	defaultLimit   = 100
	maxLimit       = 1000
	maxTZOffsetMin = 14 * 60
)

// RegisterRoutes attaches the insights JSON endpoints to an authenticated management route group.
// The routes only read data, so the group may also admit the viewer key (see ViewerKeyMatches).
// Routes that change settings or read raw data refuse the viewer key (see requireManagementKey).
func RegisterRoutes(group gin.IRoutes) {
	group.GET("/insights/status", handleStatus)
	group.GET("/insights/summary", handleSummary)
	group.GET("/insights/quota", handleQuota)
	group.GET("/insights/sessions", handleSessions)
	group.GET("/insights/requests", handleRequests)
	group.GET("/insights/threads", handleThreads)
	group.GET("/insights/thread", handleThread)
	group.GET("/insights/waste", handleWaste)
	group.GET("/insights/accounts", handleAccounts)
	group.GET("/insights/health", handleHealth)
	group.GET("/insights/alerts", handleAlerts)
	group.GET("/insights/overview", handleOverview)

	group.PUT("/insights/accounts/price", requireManagementKey, handleAccountPrice)
	group.PUT("/insights/alerts/settings", requireManagementKey, handleAlertSettings)
	group.POST("/insights/alerts/ack", requireManagementKey, handleAlertAck)
	group.POST("/insights/alerts/test", requireManagementKey, handleAlertTest)
	group.POST("/insights/backup", requireManagementKey, handleBackup)
	group.GET("/insights/sql/schema", requireManagementKey, handleSQLSchema)
	group.POST("/insights/sql", requireManagementKey, handleSQL)
}

// requireManagementKey refuses the read-only viewer key.
func requireManagementKey(c *gin.Context) {
	if c.GetBool(ViewerAccessContextKey) {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "this action needs the management key"})
		return
	}
	c.Next()
}

// ServeDashboard serves the embedded dashboard. The page itself holds no data; every data call
// requires the management key or the viewer key.
func ServeDashboard(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Referrer-Policy", "no-referrer")
	c.Data(http.StatusOK, "text/html; charset=utf-8", dashboardHTML)
}

func running(c *gin.Context) *Service {
	svc := current.Load()
	if svc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "insights recording is not running"})
		return nil
	}
	return svc
}

func handleStatus(c *gin.Context) {
	svc := running(c)
	if svc == nil {
		return
	}
	var requests, samples int64
	_ = svc.store.db.QueryRowContext(c.Request.Context(), `SELECT COUNT(*) FROM requests`).Scan(&requests)
	_ = svc.store.db.QueryRowContext(c.Request.Context(), `SELECT COUNT(*) FROM quota_samples`).Scan(&samples)
	var size int64
	if info, errStat := os.Stat(svc.store.Path()); errStat == nil {
		size = info.Size()
	}
	backup := svc.backupStatus(c.Request.Context())
	table := currentPrices()
	priceInfo := gin.H{"models": 0, "source": "none"}
	if table != nil {
		priceInfo = gin.H{"models": len(table.models), "source": priceSourceLabel(table.source), "loaded_ms": table.loaded.UnixMilli()}
	}
	status := gin.H{
		"access":         "management",
		"database_bytes": size,
		"requests":       requests,
		"quota_samples":  samples,
		"dropped_rows":   svc.recorder.dropped.Load(),
		"retention_days": int(svc.retention / (24 * time.Hour)),
		"host_labels":    hostLabels(os.Getenv("INSIGHTS_HOST_LABELS")),
		"prices":         priceInfo,
		"luna":           lunaStatus(),
	}
	// The viewer key is meant for phones and remote access, so it does not learn the hub's file layout.
	if c.GetBool(ViewerAccessContextKey) {
		status["access"] = "viewer"
		backup.Dir, backup.LastFile = "", ""
	} else {
		status["database_path"] = svc.store.Path()
	}
	status["backup"] = backup
	c.JSON(http.StatusOK, status)
}

// hostLabels parses "ip=name,ip=name" so the dashboard can name client machines.
func hostLabels(raw string) map[string]string {
	labels := make(map[string]string)
	for _, entry := range strings.Split(raw, ",") {
		ip, name, ok := strings.Cut(entry, "=")
		ip, name = strings.TrimSpace(ip), strings.TrimSpace(name)
		if ok && ip != "" && name != "" {
			labels[ip] = name
		}
	}
	return labels
}

func handleSummary(c *gin.Context) {
	svc := running(c)
	if svc == nil {
		return
	}
	from, to, ok := parseRange(c)
	if !ok {
		return
	}
	bucket, offset := parseBucket(c, from, to)
	summary, errSummary := svc.store.summary(c.Request.Context(), from, to, bucket, offset)
	if errSummary != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errSummary.Error()})
		return
	}
	c.JSON(http.StatusOK, summary)
}

func handleSessions(c *gin.Context) {
	svc := running(c)
	if svc == nil {
		return
	}
	from, to, ok := parseRange(c)
	if !ok {
		return
	}
	stats, errSessions := svc.store.sessions(c.Request.Context(), from, to, clampLimit(queryInt(c, "limit", defaultLimit)))
	if errSessions != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errSessions.Error()})
		return
	}
	c.JSON(http.StatusOK, stats)
}

func handleRequests(c *gin.Context) {
	svc := running(c)
	if svc == nil {
		return
	}
	rows, errRows := svc.store.recentRequests(c.Request.Context(), int64(queryInt(c, "before", 0)), clampLimit(queryInt(c, "limit", defaultLimit)))
	if errRows != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errRows.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"requests": rows})
}

type windowView struct {
	Key           string  `json:"key"`
	Label         string  `json:"label"`
	UsedPct       float64 `json:"used_pct"`
	ResetsAt      int64   `json:"resets_at"`
	WindowMinutes int64   `json:"window_minutes,omitempty"`
	Status        string  `json:"status,omitempty"`
	ObservedAt    int64   `json:"observed_at"`
}

type blockedModel struct {
	Model string `json:"model"`
	Until int64  `json:"until"`
}

type accountView struct {
	AuthID        string         `json:"auth_id"`
	Provider      string         `json:"provider"`
	Label         string         `json:"label"`
	Email         string         `json:"email,omitempty"`
	Plan          string         `json:"plan,omitempty"`
	Present       bool           `json:"present"`
	Disabled      bool           `json:"disabled"`
	Unavailable   bool           `json:"unavailable"`
	Status        string         `json:"status,omitempty"`
	StatusMessage string         `json:"status_message,omitempty"`
	NextRetryAt   int64          `json:"next_retry_at,omitempty"`
	LastSeenAt    int64          `json:"last_seen_at,omitempty"`
	Windows       []windowView   `json:"windows"`
	BlockedModels []blockedModel `json:"blocked_models,omitempty"`
}

func handleQuota(c *gin.Context) {
	svc := running(c)
	if svc == nil {
		return
	}
	from, to, ok := parseRange(c)
	if !ok {
		return
	}
	history, errHistory := svc.store.quotaHistory(c.Request.Context(), from, to)
	if errHistory != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errHistory.Error()})
		return
	}
	known, errKnown := svc.store.knownAccounts(c.Request.Context())
	if errKnown != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errKnown.Error()})
		return
	}

	now := time.Now().UTC()
	views := make(map[string]*accountView)
	if manager := authManager.Load(); manager != nil {
		for _, auth := range manager.List() {
			if auth == nil || !coreauth.ProviderSupportsQuotaObservation(auth.Provider) {
				continue
			}
			views[auth.ID] = liveAccountView(auth, now)
		}
	}
	for _, row := range known {
		if _, live := views[row.AuthID]; live || !coreauth.ProviderSupportsQuotaObservation(row.Provider) {
			if view := views[row.AuthID]; view != nil {
				view.LastSeenAt = row.SeenAt.UnixMilli()
			}
			continue
		}
		if _, hasHistory := history[row.AuthID]; !hasHistory {
			continue
		}
		views[row.AuthID] = &accountView{
			AuthID:     row.AuthID,
			Provider:   row.Provider,
			Label:      row.Label,
			Email:      row.Email,
			Plan:       row.Plan,
			LastSeenAt: row.SeenAt.UnixMilli(),
			Windows:    []windowView{},
		}
	}

	accounts := make([]*accountView, 0, len(views))
	labels := make(map[string]string)
	for _, view := range views {
		accounts = append(accounts, view)
		for _, window := range view.Windows {
			labels[view.Provider+"|"+window.Key] = window.Label
		}
		for windowKey := range history[view.AuthID] {
			if _, exists := labels[view.Provider+"|"+windowKey]; !exists {
				labels[view.Provider+"|"+windowKey] = windowLabel(view.Provider, windowKey)
			}
		}
	}
	sort.Slice(accounts, func(i, j int) bool {
		if accounts[i].Provider != accounts[j].Provider {
			return accounts[i].Provider < accounts[j].Provider
		}
		return accounts[i].Label < accounts[j].Label
	})
	c.JSON(http.StatusOK, gin.H{
		"now":           now.UnixMilli(),
		"from":          from.UnixMilli(),
		"to":            to.UnixMilli(),
		"accounts":      accounts,
		"history":       history,
		"window_labels": labels,
	})
}

func liveAccountView(auth *coreauth.Auth, now time.Time) *accountView {
	view := &accountView{
		AuthID:        auth.ID,
		Provider:      auth.Provider,
		Label:         accountLabel(auth),
		Email:         metadataString(auth.Metadata, "email"),
		Plan:          accountPlan(auth),
		Present:       true,
		Disabled:      auth.Disabled,
		Unavailable:   auth.Unavailable,
		Status:        string(auth.Status),
		StatusMessage: auth.StatusMessage,
		Windows:       []windowView{},
	}
	nextRetry := auth.NextRetryAfter
	if auth.Quota.Exceeded && auth.Quota.NextRecoverAt.After(nextRetry) {
		nextRetry = auth.Quota.NextRecoverAt
	}
	if nextRetry.After(now) {
		view.NextRetryAt = nextRetry.UnixMilli()
	}
	for _, window := range windowsForAuth(auth) {
		view.Windows = append(view.Windows, windowView{
			Key:           window.Key,
			Label:         window.Label,
			UsedPct:       window.UsedPct,
			ResetsAt:      unixMilliOrZero(window.ResetsAt),
			WindowMinutes: window.WindowMinutes,
			Status:        window.Status,
			ObservedAt:    unixMilliOrZero(window.ObservedAt),
		})
	}
	for model, state := range auth.ModelStates {
		if state == nil || !state.Unavailable || !state.NextRetryAfter.After(now) {
			continue
		}
		view.BlockedModels = append(view.BlockedModels, blockedModel{Model: model, Until: state.NextRetryAfter.UnixMilli()})
	}
	sort.Slice(view.BlockedModels, func(i, j int) bool { return view.BlockedModels[i].Model < view.BlockedModels[j].Model })
	return view
}

// windowLabel names a window key that only exists in history.
func windowLabel(provider, key string) string {
	if strings.EqualFold(provider, "claude") {
		return claudeWindowLabel(key)
	}
	if namespace, rest, ok := strings.Cut(key, ":"); ok {
		return namespace + " " + strings.ToLower(durationLabel(rest))
	}
	return durationLabel(key)
}

func parseRange(c *gin.Context) (time.Time, time.Time, bool) {
	now := time.Now().UTC()
	to := now
	if raw := queryInt(c, "to", 0); raw > 0 {
		to = time.UnixMilli(int64(raw)).UTC()
	}
	from := to.Add(-defaultRange)
	if raw := queryInt(c, "from", 0); raw > 0 {
		from = time.UnixMilli(int64(raw)).UTC()
	}
	if !from.Before(to) || to.Sub(from) > maxRange {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid range: from must be before to and span at most 400 days"})
		return time.Time{}, time.Time{}, false
	}
	return from, to, true
}

// parseBucket reads bucket_ms and tz_offset_min, bounding the number of buckets in [from, to).
func parseBucket(c *gin.Context, from, to time.Time) (time.Duration, time.Duration) {
	span := to.Sub(from)
	bucket := time.Duration(queryInt(c, "bucket_ms", 0)) * time.Millisecond
	if bucket <= 0 {
		bucket = autoBucket(span)
	}
	if bucket < minBucket {
		bucket = minBucket
	}
	if span/bucket > maxBuckets {
		bucket = span / maxBuckets
	}
	offsetMin := queryInt(c, "tz_offset_min", 0)
	if offsetMin > maxTZOffsetMin || offsetMin < -maxTZOffsetMin {
		offsetMin = 0
	}
	return bucket, time.Duration(offsetMin) * time.Minute
}

func autoBucket(span time.Duration) time.Duration {
	switch {
	case span <= 36*time.Hour:
		return time.Hour
	case span <= 8*24*time.Hour:
		return 6 * time.Hour
	default:
		return 24 * time.Hour
	}
}

func queryInt(c *gin.Context, key string, fallback int) int {
	raw := strings.TrimSpace(c.Query(key))
	if raw == "" {
		return fallback
	}
	value, errParse := strconv.ParseInt(raw, 10, 64)
	if errParse != nil {
		return fallback
	}
	return int(value)
}

func clampLimit(limit int) int {
	if limit <= 0 {
		return defaultLimit
	}
	if limit > maxLimit {
		return maxLimit
	}
	return limit
}

func unixMilliOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}
