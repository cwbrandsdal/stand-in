package insights

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Totals aggregates request and token counters for a slice of requests.
type Totals struct {
	Requests         int64   `json:"requests"`
	Failed           int64   `json:"failed"`
	InputTokens      int64   `json:"input_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	ReasoningTokens  int64   `json:"reasoning_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	AvgLatencyMs     float64 `json:"avg_latency_ms"`
	AvgTTFTMs        float64 `json:"avg_ttft_ms"`
	// CostUSD is the API-equivalent cost of the priced requests.
	CostUSD float64 `json:"cost_usd"`
	// Unpriced counts requests with tokens but no known model price.
	Unpriced int64 `json:"unpriced"`
}

// SeriesPoint is one time bucket of request totals.
type SeriesPoint struct {
	T int64 `json:"t"`
	Totals
}

// GroupRow is one grouped breakdown row.
type GroupRow struct {
	Provider    string `json:"provider,omitempty"`
	Model       string `json:"model,omitempty"`
	AuthID      string `json:"auth_id,omitempty"`
	Account     string `json:"account,omitempty"`
	ClientIP    string `json:"client_ip,omitempty"`
	ClientKind  string `json:"client_kind,omitempty"`
	APIKeyLabel string `json:"api_key_label,omitempty"`
	Totals
}

// HeatCell is one weekday/hour bucket in local time.
type HeatCell struct {
	Weekday     int   `json:"weekday"`
	Hour        int   `json:"hour"`
	Requests    int64 `json:"requests"`
	TotalTokens int64 `json:"total_tokens"`
}

// Summary is the usage overview for a time range.
type Summary struct {
	From       int64         `json:"from"`
	To         int64         `json:"to"`
	BucketMs   int64         `json:"bucket_ms"`
	Totals     Totals        `json:"totals"`
	Series     []SeriesPoint `json:"series"`
	ByProvider []GroupRow    `json:"by_provider"`
	ByModel    []GroupRow    `json:"by_model"`
	ByAccount  []GroupRow    `json:"by_account"`
	ByClient   []GroupRow    `json:"by_client"`
	Heatmap    []HeatCell    `json:"heatmap"`
}

// SessionRow describes one client session for affinity analysis.
type SessionRow struct {
	SessionID        string `json:"session_id"`
	Requests         int64  `json:"requests"`
	Accounts         int64  `json:"accounts"`
	AccountNames     string `json:"account_names"`
	FirstMs          int64  `json:"first_ms"`
	LastMs           int64  `json:"last_ms"`
	InputTokens      int64  `json:"input_tokens"`
	CacheReadTokens  int64  `json:"cache_read_tokens"`
	CacheWriteTokens int64  `json:"cache_write_tokens"`
	OutputTokens     int64  `json:"output_tokens"`
	ClientKind       string `json:"client_kind"`
	ClientIP         string `json:"client_ip"`
	Model            string `json:"model"`
}

// SessionStats summarizes session affinity over a time range.
type SessionStats struct {
	Sessions       int64        `json:"sessions"`
	HoppedSessions int64        `json:"hopped_sessions"`
	Rows           []SessionRow `json:"rows"`
}

// QuotaPoint is one stored quota observation: [ts_ms, used_pct, resets_at_ms].
type QuotaPoint [3]float64

// RecentRequest is one row of the recent request log.
type RecentRequest struct {
	ID               int64  `json:"id"`
	TS               int64  `json:"ts"`
	Provider         string `json:"provider"`
	Model            string `json:"model"`
	Account          string `json:"account"`
	AuthID           string `json:"auth_id"`
	ClientIP         string `json:"client_ip"`
	ClientKind       string `json:"client_kind"`
	APIKeyLabel      string `json:"api_key_label"`
	SessionID        string `json:"session_id"`
	Stream           bool   `json:"stream"`
	Failed           bool   `json:"failed"`
	StatusCode       int64  `json:"status_code"`
	LatencyMs        int64  `json:"latency_ms"`
	TTFTMs           int64  `json:"ttft_ms"`
	InputTokens      int64  `json:"input_tokens"`
	CacheReadTokens  int64  `json:"cache_read_tokens"`
	CacheWriteTokens int64  `json:"cache_write_tokens"`
	OutputTokens     int64  `json:"output_tokens"`
	ReasoningEffort  string `json:"reasoning_effort"`
	ServiceTier      string `json:"service_tier"`
	ErrorClass       string `json:"error_class"`
	// CostUSD is nil when the model has no known price.
	CostUSD *float64 `json:"cost_usd"`
}

const totalsColumns = `COUNT(*), COALESCE(SUM(failed),0), COALESCE(SUM(input_tokens),0), COALESCE(SUM(cache_read_tokens),0),
	COALESCE(SUM(cache_write_tokens),0), COALESCE(SUM(output_tokens),0), COALESCE(SUM(reasoning_tokens),0),
	COALESCE(SUM(total_tokens),0), COALESCE(SUM(cost_usd),0),
	COALESCE(SUM(CASE WHEN cost_usd IS NULL AND total_tokens > 0 THEN 1 ELSE 0 END),0),
	AVG(CASE WHEN latency_ms > 0 THEN latency_ms END), AVG(CASE WHEN ttft_ms > 0 THEN ttft_ms END)`

func totalsTargets(t *Totals, latency, ttft *sql.NullFloat64) []any {
	return []any{&t.Requests, &t.Failed, &t.InputTokens, &t.CacheReadTokens, &t.CacheWriteTokens,
		&t.OutputTokens, &t.ReasoningTokens, &t.TotalTokens, &t.CostUSD, &t.Unpriced, latency, ttft}
}

func finishTotals(t *Totals, latency, ttft sql.NullFloat64) {
	if latency.Valid {
		t.AvgLatencyMs = latency.Float64
	}
	if ttft.Valid {
		t.AvgTTFTMs = ttft.Float64
	}
}

// summary aggregates requests in [from, to). offset shifts buckets and the heatmap to the viewer's local time.
func (s *Store) summary(ctx context.Context, from, to time.Time, bucket, offset time.Duration) (*Summary, error) {
	fromMs, toMs := from.UnixMilli(), to.UnixMilli()
	bucketMs := bucket.Milliseconds()
	if bucketMs <= 0 {
		bucketMs = time.Hour.Milliseconds()
	}
	offsetMs := offset.Milliseconds()
	out := &Summary{From: fromMs, To: toMs, BucketMs: bucketMs}

	var latency, ttft sql.NullFloat64
	row := s.db.QueryRowContext(ctx, `SELECT `+totalsColumns+` FROM requests WHERE ts_ms >= ? AND ts_ms < ?`, fromMs, toMs)
	if errScan := row.Scan(totalsTargets(&out.Totals, &latency, &ttft)...); errScan != nil {
		return nil, fmt.Errorf("insights: totals: %w", errScan)
	}
	finishTotals(&out.Totals, latency, ttft)

	series, errSeries := s.db.QueryContext(ctx, `SELECT ((ts_ms + ?) / ?) * ? - ? AS bucket, `+totalsColumns+`
		FROM requests WHERE ts_ms >= ? AND ts_ms < ? GROUP BY bucket ORDER BY bucket`,
		offsetMs, bucketMs, bucketMs, offsetMs, fromMs, toMs)
	if errSeries != nil {
		return nil, fmt.Errorf("insights: series: %w", errSeries)
	}
	for series.Next() {
		var point SeriesPoint
		targets := append([]any{&point.T}, totalsTargets(&point.Totals, &latency, &ttft)...)
		if errScan := series.Scan(targets...); errScan != nil {
			_ = series.Close()
			return nil, fmt.Errorf("insights: scan series: %w", errScan)
		}
		finishTotals(&point.Totals, latency, ttft)
		out.Series = append(out.Series, point)
	}
	if errClose := closeRows(series); errClose != nil {
		return nil, errClose
	}

	var errGroup error
	if out.ByProvider, errGroup = s.groupBy(ctx, fromMs, toMs, []string{"provider"}, func(r *GroupRow) []any {
		return []any{&r.Provider}
	}); errGroup != nil {
		return nil, errGroup
	}
	if out.ByModel, errGroup = s.groupBy(ctx, fromMs, toMs, []string{"provider", "model"}, func(r *GroupRow) []any {
		return []any{&r.Provider, &r.Model}
	}); errGroup != nil {
		return nil, errGroup
	}
	if out.ByAccount, errGroup = s.groupByAccount(ctx, fromMs, toMs); errGroup != nil {
		return nil, errGroup
	}
	if out.ByClient, errGroup = s.groupBy(ctx, fromMs, toMs, []string{"client_ip", "client_kind", "api_key_label"}, func(r *GroupRow) []any {
		return []any{&r.ClientIP, &r.ClientKind, &r.APIKeyLabel}
	}); errGroup != nil {
		return nil, errGroup
	}

	heat, errHeat := s.db.QueryContext(ctx, `SELECT
			CAST(strftime('%w', (ts_ms + ?) / 1000, 'unixepoch') AS INTEGER) AS wd,
			CAST(strftime('%H', (ts_ms + ?) / 1000, 'unixepoch') AS INTEGER) AS hr,
			COUNT(*), COALESCE(SUM(total_tokens),0)
		FROM requests WHERE ts_ms >= ? AND ts_ms < ? GROUP BY wd, hr`, offsetMs, offsetMs, fromMs, toMs)
	if errHeat != nil {
		return nil, fmt.Errorf("insights: heatmap: %w", errHeat)
	}
	for heat.Next() {
		var cell HeatCell
		if errScan := heat.Scan(&cell.Weekday, &cell.Hour, &cell.Requests, &cell.TotalTokens); errScan != nil {
			_ = heat.Close()
			return nil, fmt.Errorf("insights: scan heatmap: %w", errScan)
		}
		out.Heatmap = append(out.Heatmap, cell)
	}
	if errClose := closeRows(heat); errClose != nil {
		return nil, errClose
	}
	return out, nil
}

// groupBy aggregates by the given columns; keys returns the string fields receiving those columns in order.
// columns are trusted constants, never user input.
func (s *Store) groupBy(ctx context.Context, fromMs, toMs int64, columns []string, keys func(*GroupRow) []any) ([]GroupRow, error) {
	selected := make([]string, len(columns))
	for i, column := range columns {
		selected[i] = "COALESCE(" + column + ",'')"
	}
	rows, errQuery := s.db.QueryContext(ctx, `SELECT `+strings.Join(selected, ", ")+`, `+totalsColumns+`
		FROM requests WHERE ts_ms >= ? AND ts_ms < ? GROUP BY `+strings.Join(columns, ", ")+`
		ORDER BY COALESCE(SUM(total_tokens),0) DESC LIMIT 200`, fromMs, toMs)
	if errQuery != nil {
		return nil, fmt.Errorf("insights: group by %v: %w", columns, errQuery)
	}
	var out []GroupRow
	var latency, ttft sql.NullFloat64
	for rows.Next() {
		var row GroupRow
		targets := append(keys(&row), totalsTargets(&row.Totals, &latency, &ttft)...)
		if errScan := rows.Scan(targets...); errScan != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("insights: scan group %v: %w", columns, errScan)
		}
		finishTotals(&row.Totals, latency, ttft)
		out = append(out, row)
	}
	return out, closeRows(rows)
}

func (s *Store) groupByAccount(ctx context.Context, fromMs, toMs int64) ([]GroupRow, error) {
	rows, errQuery := s.db.QueryContext(ctx, `SELECT COALESCE(auth_id,''), MAX(provider), COALESCE(MAX(account),''), `+totalsColumns+`
		FROM requests WHERE ts_ms >= ? AND ts_ms < ? GROUP BY auth_id ORDER BY COALESCE(SUM(total_tokens),0) DESC LIMIT 200`,
		fromMs, toMs)
	if errQuery != nil {
		return nil, fmt.Errorf("insights: group by account: %w", errQuery)
	}
	var out []GroupRow
	var latency, ttft sql.NullFloat64
	for rows.Next() {
		var row GroupRow
		targets := append([]any{&row.AuthID, &row.Provider, &row.Account}, totalsTargets(&row.Totals, &latency, &ttft)...)
		if errScan := rows.Scan(targets...); errScan != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("insights: scan account group: %w", errScan)
		}
		finishTotals(&row.Totals, latency, ttft)
		out = append(out, row)
	}
	return out, closeRows(rows)
}

// sessions returns session affinity statistics for [from, to).
func (s *Store) sessions(ctx context.Context, from, to time.Time, limit int) (*SessionStats, error) {
	fromMs, toMs := from.UnixMilli(), to.UnixMilli()
	out := &SessionStats{}
	if errScan := s.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(CASE WHEN n > 1 THEN 1 ELSE 0 END),0) FROM (
			SELECT `+accountsPerProvider+` AS n FROM requests
			WHERE ts_ms >= ? AND ts_ms < ? AND session_id <> '' AND failed = 0 GROUP BY session_id)`,
		fromMs, toMs).Scan(&out.Sessions, &out.HoppedSessions); errScan != nil {
		return nil, fmt.Errorf("insights: session stats: %w", errScan)
	}
	rows, errQuery := s.db.QueryContext(ctx, `SELECT session_id, COUNT(*),
			`+accountsPerProvider+`, COALESCE(GROUP_CONCAT(DISTINCT account),''),
			MIN(ts_ms), MAX(ts_ms), COALESCE(SUM(input_tokens),0), COALESCE(SUM(cache_read_tokens),0),
			COALESCE(SUM(cache_write_tokens),0), COALESCE(SUM(output_tokens),0),
			COALESCE(MAX(client_kind),''), COALESCE(MAX(client_ip),''), COALESCE(MAX(model),'')
		FROM requests WHERE ts_ms >= ? AND ts_ms < ? AND session_id <> ''
		GROUP BY session_id ORDER BY MAX(ts_ms) DESC LIMIT ?`, fromMs, toMs, limit)
	if errQuery != nil {
		return nil, fmt.Errorf("insights: sessions: %w", errQuery)
	}
	for rows.Next() {
		var row SessionRow
		if errScan := rows.Scan(&row.SessionID, &row.Requests, &row.Accounts, &row.AccountNames, &row.FirstMs, &row.LastMs,
			&row.InputTokens, &row.CacheReadTokens, &row.CacheWriteTokens, &row.OutputTokens,
			&row.ClientKind, &row.ClientIP, &row.Model); errScan != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("insights: scan session: %w", errScan)
		}
		out.Rows = append(out.Rows, row)
	}
	return out, closeRows(rows)
}

// quotaHistory returns samples in [from, to) plus the latest sample before from for each auth/window,
// keyed by auth ID and window key.
func (s *Store) quotaHistory(ctx context.Context, from, to time.Time) (map[string]map[string][]QuotaPoint, error) {
	fromMs, toMs := from.UnixMilli(), to.UnixMilli()
	out := make(map[string]map[string][]QuotaPoint)
	rows, errQuery := s.db.QueryContext(ctx, `SELECT auth_id, window_key, ts_ms, used_pct, resets_at_ms FROM (
			SELECT q.auth_id, q.window_key, q.ts_ms, q.used_pct, q.resets_at_ms FROM quota_samples q
			JOIN (SELECT auth_id, window_key, MAX(ts_ms) AS m FROM quota_samples WHERE ts_ms < ? GROUP BY auth_id, window_key) p
				ON q.auth_id = p.auth_id AND q.window_key = p.window_key AND q.ts_ms = p.m
			UNION ALL
			SELECT auth_id, window_key, ts_ms, used_pct, resets_at_ms FROM quota_samples WHERE ts_ms >= ? AND ts_ms < ?
		) ORDER BY auth_id, window_key, ts_ms`, fromMs, fromMs, toMs)
	if errQuery != nil {
		return nil, fmt.Errorf("insights: quota history: %w", errQuery)
	}
	for rows.Next() {
		var authID, windowKey string
		var ts, resetsAt int64
		var used float64
		if errScan := rows.Scan(&authID, &windowKey, &ts, &used, &resetsAt); errScan != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("insights: scan quota history: %w", errScan)
		}
		byWindow := out[authID]
		if byWindow == nil {
			byWindow = make(map[string][]QuotaPoint)
			out[authID] = byWindow
		}
		byWindow[windowKey] = append(byWindow[windowKey], QuotaPoint{float64(ts), used, float64(resetsAt)})
	}
	return out, closeRows(rows)
}

// windowSeen is the latest stored observation of one quota window.
type windowSeen struct {
	At       time.Time
	ResetsAt time.Time
}

// windowsLastSeen returns the latest stored observation of every window, by account and window key.
func (s *Store) windowsLastSeen(ctx context.Context) (map[string]map[string]windowSeen, error) {
	rows, errQuery := s.db.QueryContext(ctx, `SELECT q.auth_id, q.window_key, q.ts_ms, MAX(q.resets_at_ms) FROM quota_samples q
		JOIN (SELECT auth_id, window_key, MAX(ts_ms) AS m FROM quota_samples GROUP BY auth_id, window_key) p
			ON q.auth_id = p.auth_id AND q.window_key = p.window_key AND q.ts_ms = p.m
		GROUP BY q.auth_id, q.window_key, q.ts_ms`)
	if errQuery != nil {
		return nil, fmt.Errorf("insights: windows last seen: %w", errQuery)
	}
	out := make(map[string]map[string]windowSeen)
	for rows.Next() {
		var authID, windowKey string
		var ts, resetsAt int64
		if errScan := rows.Scan(&authID, &windowKey, &ts, &resetsAt); errScan != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("insights: scan windows last seen: %w", errScan)
		}
		if out[authID] == nil {
			out[authID] = make(map[string]windowSeen)
		}
		seen := windowSeen{At: time.UnixMilli(ts).UTC()}
		if resetsAt > 0 {
			seen.ResetsAt = time.UnixMilli(resetsAt).UTC()
		}
		out[authID][windowKey] = seen
	}
	return out, closeRows(rows)
}

// knownAccounts returns every account the store has seen.
func (s *Store) knownAccounts(ctx context.Context) ([]accountRow, error) {
	rows, errQuery := s.db.QueryContext(ctx, `SELECT auth_id, provider, COALESCE(label,''), COALESCE(email,''), COALESCE(plan,''), last_seen_ms FROM accounts`)
	if errQuery != nil {
		return nil, fmt.Errorf("insights: accounts: %w", errQuery)
	}
	var out []accountRow
	for rows.Next() {
		var row accountRow
		var seen int64
		if errScan := rows.Scan(&row.AuthID, &row.Provider, &row.Label, &row.Email, &row.Plan, &seen); errScan != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("insights: scan account: %w", errScan)
		}
		row.SeenAt = time.UnixMilli(seen).UTC()
		out = append(out, row)
	}
	return out, closeRows(rows)
}

// recentRequests returns the newest requests by start time, optionally only those started before beforeMs.
func (s *Store) recentRequests(ctx context.Context, beforeMs int64, limit int) ([]RecentRequest, error) {
	if beforeMs <= 0 {
		beforeMs = 1<<62 - 1
	}
	rows, errQuery := s.db.QueryContext(ctx, `SELECT id, ts_ms, provider, COALESCE(model,''), COALESCE(account,''), COALESCE(auth_id,''),
			COALESCE(client_ip,''), COALESCE(client_kind,''), COALESCE(api_key_label,''), COALESCE(session_id,''),
			stream, failed, COALESCE(status_code,0), COALESCE(latency_ms,0), COALESCE(ttft_ms,0), input_tokens,
			cache_read_tokens, cache_write_tokens, output_tokens, COALESCE(reasoning_effort,''),
			COALESCE(service_tier,''), COALESCE(error_class,''), cost_usd
		FROM requests WHERE ts_ms < ? ORDER BY ts_ms DESC, id DESC LIMIT ?`, beforeMs, limit)
	if errQuery != nil {
		return nil, fmt.Errorf("insights: recent requests: %w", errQuery)
	}
	var out []RecentRequest
	for rows.Next() {
		var row RecentRequest
		var stream, failed int64
		var cost sql.NullFloat64
		if errScan := rows.Scan(&row.ID, &row.TS, &row.Provider, &row.Model, &row.Account, &row.AuthID, &row.ClientIP,
			&row.ClientKind, &row.APIKeyLabel, &row.SessionID, &stream, &failed, &row.StatusCode, &row.LatencyMs,
			&row.TTFTMs, &row.InputTokens, &row.CacheReadTokens, &row.CacheWriteTokens, &row.OutputTokens,
			&row.ReasoningEffort, &row.ServiceTier, &row.ErrorClass, &cost); errScan != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("insights: scan recent request: %w", errScan)
		}
		row.Stream, row.Failed = stream != 0, failed != 0
		if cost.Valid {
			value := cost.Float64
			row.CostUSD = &value
		}
		out = append(out, row)
	}
	return out, closeRows(rows)
}

func closeRows(rows *sql.Rows) error {
	errIter := rows.Err()
	errClose := rows.Close()
	if errIter != nil {
		return fmt.Errorf("insights: iterate rows: %w", errIter)
	}
	if errClose != nil {
		return fmt.Errorf("insights: close rows: %w", errClose)
	}
	return nil
}
