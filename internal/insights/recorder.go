package insights

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	coresession "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

const (
	recorderBuffer     = 4096
	recorderBatchSize  = 256
	recorderFlushEvery = time.Second
	maxUserAgentLength = 200
)

// apiKeyLabelPattern extracts a readable label from client keys shaped like "cpa-<label>-<secret>".
var apiKeyLabelPattern = regexp.MustCompile(`^cpa-([a-z0-9][a-z0-9-]{0,30}[a-z0-9])-[A-Za-z0-9_]{16,}$`)

// recorder batches request rows and writes them on a single goroutine so the request path never blocks on disk.
type recorder struct {
	store    *Store
	rows     chan requestRow
	failures chan accessFailureRow
	dropped  atomic.Int64
	done     chan struct{}
}

// accessFailureRow is one rejected client or management key. The key itself is never stored.
type accessFailureRow struct {
	TS         time.Time
	Kind       string
	StatusCode int
	ClientIP   string
	Route      string
}

const (
	// AccessFailureClientKey is a request to the proxy API with a missing or unknown client key.
	AccessFailureClientKey = "client_key"
	// AccessFailureManagementKey is a management or insights request with a wrong key.
	AccessFailureManagementKey = "management_key"
	accessFailureBuffer        = 256
	maxRouteLength             = 120
)

func newRecorder(store *Store) *recorder {
	return &recorder{
		store:    store,
		rows:     make(chan requestRow, recorderBuffer),
		failures: make(chan accessFailureRow, accessFailureBuffer),
		done:     make(chan struct{}),
	}
}

// RecordAccessFailure notes a rejected key for the errors view. It never blocks; under a flood of
// failures the extra rows are dropped.
func RecordAccessFailure(kind string, statusCode int, clientIP, route string) {
	svc := current.Load()
	if svc == nil {
		return
	}
	if len(route) > maxRouteLength {
		route = route[:maxRouteLength]
	}
	select {
	case svc.recorder.failures <- accessFailureRow{TS: time.Now().UTC(), Kind: kind, StatusCode: statusCode, ClientIP: clientIP, Route: route}:
	default:
	}
}

func (r *recorder) enqueue(row requestRow) {
	select {
	case r.rows <- row:
	default:
		if r.dropped.Add(1)%100 == 1 {
			log.Warn("insights: request buffer full, dropping usage rows")
		}
	}
}

func (r *recorder) run(ctx context.Context) {
	defer close(r.done)
	ticker := time.NewTicker(recorderFlushEvery)
	defer ticker.Stop()
	batch := make([]requestRow, 0, recorderBatchSize)
	var failures []accessFailureRow
	flush := func() {
		if len(batch) == 0 && len(failures) == 0 {
			return
		}
		writeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if errInsert := r.store.insertRequests(writeCtx, batch); errInsert != nil {
			log.Errorf("insights: failed to persist %d usage rows: %v", len(batch), errInsert)
		}
		if errInsert := r.store.insertAccessFailures(writeCtx, failures); errInsert != nil {
			log.Errorf("insights: failed to persist %d access failures: %v", len(failures), errInsert)
		}
		cancel()
		batch = batch[:0]
		failures = failures[:0]
	}
	for {
		select {
		case row := <-r.rows:
			batch = append(batch, row)
			if len(batch) >= recorderBatchSize {
				flush()
			}
		case failure := <-r.failures:
			failures = append(failures, failure)
		case <-ticker.C:
			flush()
		case <-ctx.Done():
			for {
				select {
				case row := <-r.rows:
					batch = append(batch, row)
				case failure := <-r.failures:
					failures = append(failures, failure)
				default:
					flush()
					return
				}
			}
		}
	}
}

// usagePlugin forwards usage records to the running insights service.
type usagePlugin struct{}

func (usagePlugin) HandleUsage(ctx context.Context, record coreusage.Record) {
	svc := current.Load()
	if svc == nil || !coreusage.GenerateEnabled(record.Generate) {
		return
	}
	svc.recorder.enqueue(buildRequestRow(ctx, record, svc.accountLabel(record.AuthID)))
}

func buildRequestRow(ctx context.Context, record coreusage.Record, account string) requestRow {
	ts := record.RequestedAt
	if ts.IsZero() {
		ts = time.Now()
	}
	meta := internallogging.GetClientRequestMetadata(ctx)
	clientIP := strings.TrimSpace(meta.ResolvedClientIP)
	if clientIP == "" {
		clientIP = strings.TrimSpace(meta.ClientIP)
	}
	userAgent := strings.TrimSpace(meta.UserAgent)
	if len(userAgent) > maxUserAgentLength {
		userAgent = userAgent[:maxUserAgentLength]
	}

	sessionID := strings.TrimSpace(record.SessionID)
	parentSessionID := strings.TrimSpace(record.ParentSessionID)
	if sessionID == "" {
		sessionID = strings.TrimSpace(meta.SessionID)
		parentSessionID = strings.TrimSpace(meta.ParentSessionID)
	}
	sessionID = coresession.NormalizeToCanonicalUUID(sessionID)
	parentSessionID = coresession.NormalizeToCanonicalUUID(parentSessionID)
	if parentSessionID == sessionID {
		parentSessionID = ""
	}

	status := internallogging.GetResponseStatus(ctx)
	failed := record.Failed || status >= 400
	statusCode := record.Fail.StatusCode
	if statusCode <= 0 {
		statusCode = status
	}
	if statusCode <= 0 {
		statusCode = 200
		if failed {
			statusCode = 500
		}
	}

	detail := coreusage.EnsureTokenBreakdownForProvider(record.Detail, record.Provider, record.ExecutorType)
	breakdown := detail.TokenBreakdown
	total := breakdown.TotalTokens
	if total == 0 {
		total = detail.TotalTokens
	}

	reasoningEffort := strings.TrimSpace(record.ReasoningEffort)
	if reasoningEffort == "" {
		reasoningEffort = coreusage.ReasoningEffortFromContext(ctx)
	}
	serviceTier := strings.TrimSpace(record.EffectiveServiceTier)
	if serviceTier == "" {
		serviceTier = strings.TrimSpace(record.ServiceTier)
	}
	fingerprint, label := apiKeyIdentity(record.APIKey)
	errorClass := ""
	if failed {
		errorClass = classifyFailure(statusCode, strings.TrimSpace(record.Fail.Code), record.Fail.Body)
	}

	row := requestRow{
		TS:               ts.UTC(),
		RequestID:        strings.TrimSpace(record.RequestID),
		TraceID:          strings.TrimSpace(record.TraceID),
		Provider:         firstNonEmpty(strings.TrimSpace(record.Provider), "unknown"),
		Model:            strings.TrimSpace(record.Model),
		Alias:            strings.TrimSpace(record.Alias),
		ResponseModel:    strings.TrimSpace(record.ResponseModel),
		AuthID:           strings.TrimSpace(record.AuthID),
		AuthIndex:        strings.TrimSpace(record.AuthIndex),
		AuthType:         strings.TrimSpace(record.AuthType),
		Account:          account,
		APIKeyFP:         fingerprint,
		APIKeyLabel:      label,
		ClientIP:         clientIP,
		UserAgent:        userAgent,
		ClientKind:       clientKind(userAgent),
		SessionID:        sessionID,
		ParentSessionID:  parentSessionID,
		Stream:           record.Stream || coreusage.StreamFromContext(ctx),
		Failed:           failed,
		StatusCode:       statusCode,
		FailCode:         strings.TrimSpace(record.Fail.Code),
		LatencyMs:        record.Latency.Milliseconds(),
		TTFTMs:           record.TTFT.Milliseconds(),
		InputTokens:      breakdown.Input.UncachedTokens,
		CacheReadTokens:  breakdown.Input.CacheReadTokens,
		CacheWriteTokens: breakdown.Input.CacheWriteTokens,
		OutputTokens:     breakdown.Output.TotalTokens,
		ReasoningTokens:  breakdown.Output.ReasoningTokens,
		TotalTokens:      total,
		ReasoningEffort:  reasoningEffort,
		ServiceTier:      serviceTier,
		ErrorClass:       errorClass,
	}
	if row.InputTokens+row.CacheReadTokens+row.CacheWriteTokens+row.OutputTokens > 0 {
		if cost, ok := requestCost(row); ok {
			row.CostUSD = sql.NullFloat64{Float64: cost, Valid: true}
		}
	}
	return row
}

// apiKeyIdentity returns a short non-reversible fingerprint and an optional readable label.
// The raw client key is never persisted.
func apiKeyIdentity(apiKey string) (string, string) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return "", ""
	}
	sum := sha256.Sum256([]byte(apiKey))
	fingerprint := hex.EncodeToString(sum[:4])
	if match := apiKeyLabelPattern.FindStringSubmatch(apiKey); match != nil {
		return fingerprint, match[1]
	}
	return fingerprint, ""
}

// clientKind classifies the calling tool from its User-Agent.
func clientKind(userAgent string) string {
	lower := strings.ToLower(userAgent)
	switch {
	case lower == "":
		return "unknown"
	case strings.Contains(lower, "claude-cli"), strings.Contains(lower, "claude-code"), strings.Contains(lower, "claude_code"):
		return "claude-code"
	case strings.Contains(lower, "codex"):
		return "codex"
	case strings.Contains(lower, "t3"):
		return "t3"
	case strings.Contains(lower, "opencode"):
		return "opencode"
	case strings.HasPrefix(lower, "curl/"):
		return "curl"
	}
	name, _, _ := strings.Cut(lower, "/")
	name, _, _ = strings.Cut(name, " ")
	if len(name) > 32 {
		name = name[:32]
	}
	return name
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
