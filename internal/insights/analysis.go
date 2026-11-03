package insights

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// defaultFirstWriteTokens flags threads whose first request writes this much prompt cache.
	// Claude Code starts near 30K; Claude run through the Codex harness writes 400K or more.
	defaultFirstWriteTokens = 100000
	// defaultIdleMinutes is Anthropic's default prompt cache lifetime.
	defaultIdleMinutes = 5
	// defaultRewriteTokens ignores small cache writes after an idle gap.
	defaultRewriteTokens = 20000
	maxAnalysisRows      = 500
	maxLatencySamples    = 200000
	maxThreadRequests    = 5000
)

// accountsPerProvider is 1 while each provider in a session used one account, and grows by one for
// every extra account. Switching provider (a Codex thread moving to a Claude model) is not a move.
const accountsPerProvider = `COUNT(DISTINCT CASE WHEN failed = 0 AND COALESCE(auth_id,'') <> '' THEN provider || '|' || auth_id END) -
	COUNT(DISTINCT CASE WHEN failed = 0 AND COALESCE(auth_id,'') <> '' THEN provider END) + 1`

// sessionScope selects sessions with activity in [from, to); bound twice as from, to.
const sessionScope = `session_id IN (SELECT DISTINCT session_id FROM requests WHERE ts_ms >= ? AND ts_ms < ? AND session_id <> '')`

func envInt(key string, fallback int) int {
	if value, errParse := strconv.Atoi(strings.TrimSpace(os.Getenv(key))); errParse == nil && value > 0 {
		return value
	}
	return fallback
}

// harnessFor names the agent harness that sent a session's requests. warning marks Claude models
// driven by the Codex harness, which sends its whole tool catalog on every request.
func harnessFor(clientKind, userAgent, provider string) (string, bool) {
	claude := strings.EqualFold(provider, "claude")
	switch clientKind {
	case "claude-code":
		if strings.Contains(strings.ToLower(userAgent), "agent-sdk") {
			return "Claude Agent SDK (T3)", false
		}
		return "Claude Code", false
	case "t3":
		if claude {
			return "Codex harness → Claude (T3)", true
		}
		return "Codex (T3)", false
	case "codex":
		if claude {
			return "Codex CLI → Claude", true
		}
		return "Codex CLI", false
	case "", "unknown":
		return "Unknown", false
	}
	return clientKind, false
}

// ThreadRow summarizes one agent thread (client session).
type ThreadRow struct {
	SessionID        string  `json:"session_id"`
	FirstMs          int64   `json:"first_ms"`
	LastMs           int64   `json:"last_ms"`
	Requests         int64   `json:"requests"`
	Failed           int64   `json:"failed"`
	ClientKind       string  `json:"client_kind"`
	ClientIP         string  `json:"client_ip"`
	Provider         string  `json:"provider"`
	Models           string  `json:"models"`
	Harness          string  `json:"harness"`
	HarnessWarning   bool    `json:"harness_warning"`
	Accounts         int64   `json:"accounts"`
	InputTokens      int64   `json:"input_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CostUSD          float64 `json:"cost_usd"`
	Unpriced         int64   `json:"unpriced"`
	FirstCacheWrite  int64   `json:"first_cache_write"`
}

// threads returns sessions active in [from, to), ordered by cost or by last activity.
func (s *Store) threads(ctx context.Context, from, to time.Time, byCost bool, limit int) ([]ThreadRow, error) {
	order := "MAX(ts_ms) DESC"
	if byCost {
		order = "COALESCE(SUM(cost_usd),0) DESC, MAX(ts_ms) DESC"
	}
	rows, errQuery := s.db.QueryContext(ctx, `SELECT session_id, MIN(ts_ms), MAX(ts_ms), COUNT(*), COALESCE(SUM(failed),0),
			COALESCE(MAX(client_kind),''), COALESCE(MAX(client_ip),''), COALESCE(MAX(user_agent),''), COALESCE(MAX(provider),''),
			COALESCE(GROUP_CONCAT(DISTINCT model),''), `+accountsPerProvider+`,
			COALESCE(SUM(input_tokens),0), COALESCE(SUM(cache_read_tokens),0), COALESCE(SUM(cache_write_tokens),0),
			COALESCE(SUM(output_tokens),0), COALESCE(SUM(cost_usd),0),
			COALESCE(SUM(CASE WHEN cost_usd IS NULL AND total_tokens > 0 THEN 1 ELSE 0 END),0)
		FROM requests WHERE ts_ms >= ? AND ts_ms < ? AND session_id <> ''
		GROUP BY session_id ORDER BY `+order+` LIMIT ?`, from.UnixMilli(), to.UnixMilli(), limit)
	if errQuery != nil {
		return nil, fmt.Errorf("insights: threads: %w", errQuery)
	}
	var out []ThreadRow
	for rows.Next() {
		var row ThreadRow
		var userAgent string
		if errScan := rows.Scan(&row.SessionID, &row.FirstMs, &row.LastMs, &row.Requests, &row.Failed, &row.ClientKind,
			&row.ClientIP, &userAgent, &row.Provider, &row.Models, &row.Accounts, &row.InputTokens, &row.CacheReadTokens,
			&row.CacheWriteTokens, &row.OutputTokens, &row.CostUSD, &row.Unpriced); errScan != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("insights: scan thread: %w", errScan)
		}
		row.Harness, row.HarnessWarning = harnessFor(row.ClientKind, userAgent, row.Provider)
		out = append(out, row)
	}
	if errClose := closeRows(rows); errClose != nil {
		return nil, errClose
	}
	for i := range out {
		errScan := s.db.QueryRowContext(ctx, `SELECT cache_write_tokens FROM requests
			WHERE session_id = ? AND failed = 0 ORDER BY ts_ms, id LIMIT 1`, out[i].SessionID).Scan(&out[i].FirstCacheWrite)
		if errScan != nil && errScan != sql.ErrNoRows {
			return nil, fmt.Errorf("insights: thread first write: %w", errScan)
		}
	}
	return out, nil
}

// ThreadRequest is one request of a thread.
type ThreadRequest struct {
	TS               int64    `json:"ts"`
	Provider         string   `json:"provider"`
	Model            string   `json:"model"`
	Account          string   `json:"account"`
	StatusCode       int64    `json:"status_code"`
	Failed           bool     `json:"failed"`
	ErrorClass       string   `json:"error_class"`
	InputTokens      int64    `json:"input_tokens"`
	CacheReadTokens  int64    `json:"cache_read_tokens"`
	CacheWriteTokens int64    `json:"cache_write_tokens"`
	OutputTokens     int64    `json:"output_tokens"`
	CostUSD          *float64 `json:"cost_usd"`
	LatencyMs        int64    `json:"latency_ms"`
	TTFTMs           int64    `json:"ttft_ms"`
	ReasoningEffort  string   `json:"reasoning_effort"`
	ServiceTier      string   `json:"service_tier"`
}

// threadRequests returns every request of one session in time order.
func (s *Store) threadRequests(ctx context.Context, sessionID string) ([]ThreadRequest, error) {
	rows, errQuery := s.db.QueryContext(ctx, `SELECT ts_ms, provider, COALESCE(model,''), COALESCE(account,''),
			COALESCE(status_code,0), failed, COALESCE(error_class,''), input_tokens, cache_read_tokens, cache_write_tokens,
			output_tokens, cost_usd, COALESCE(latency_ms,0), COALESCE(ttft_ms,0), COALESCE(reasoning_effort,''),
			COALESCE(service_tier,'')
		FROM requests WHERE session_id = ? ORDER BY ts_ms, id LIMIT ?`, sessionID, maxThreadRequests)
	if errQuery != nil {
		return nil, fmt.Errorf("insights: thread requests: %w", errQuery)
	}
	var out []ThreadRequest
	for rows.Next() {
		var row ThreadRequest
		var failed int64
		var cost sql.NullFloat64
		if errScan := rows.Scan(&row.TS, &row.Provider, &row.Model, &row.Account, &row.StatusCode, &failed, &row.ErrorClass,
			&row.InputTokens, &row.CacheReadTokens, &row.CacheWriteTokens, &row.OutputTokens, &cost, &row.LatencyMs,
			&row.TTFTMs, &row.ReasoningEffort, &row.ServiceTier); errScan != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("insights: scan thread request: %w", errScan)
		}
		row.Failed = failed != 0
		if cost.Valid {
			value := cost.Float64
			row.CostUSD = &value
		}
		out = append(out, row)
	}
	return out, closeRows(rows)
}

// AccountMove is a session switching credentials of the same provider between two consecutive
// requests. A thread that switches from a Codex to a Claude model is not a move.
type AccountMove struct {
	TS               int64   `json:"ts"`
	SessionID        string  `json:"session_id"`
	Provider         string  `json:"provider"`
	FromAuthID       string  `json:"from_auth_id"`
	FromAccount      string  `json:"from_account"`
	ToAuthID         string  `json:"to_auth_id"`
	ToAccount        string  `json:"to_account"`
	Cause            string  `json:"cause"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	ExtraUSD         float64 `json:"extra_usd"`
}

func (s *Store) accountMoves(ctx context.Context, from, to time.Time) ([]AccountMove, error) {
	fromMs, toMs := from.UnixMilli(), to.UnixMilli()
	rows, errQuery := s.db.QueryContext(ctx, `WITH s AS (
			SELECT ts_ms, session_id, auth_id, COALESCE(account,'') AS account, provider, COALESCE(model,'') AS model,
				cache_write_tokens, failed,
				LAG(auth_id) OVER w AS prev_auth, LAG(COALESCE(account,'')) OVER w AS prev_account,
				LAG(failed) OVER w AS prev_failed, LAG(COALESCE(error_class,'')) OVER w AS prev_class
			FROM requests WHERE COALESCE(auth_id,'') <> '' AND `+sessionScope+`
			WINDOW w AS (PARTITION BY session_id, provider ORDER BY ts_ms, id))
		SELECT ts_ms, session_id, provider, model, prev_auth, prev_account, auth_id, account, prev_failed, prev_class,
			CASE WHEN failed = 0 THEN cache_write_tokens ELSE 0 END
		FROM s WHERE prev_auth IS NOT NULL AND prev_auth <> auth_id AND ts_ms >= ? AND ts_ms < ?
		ORDER BY ts_ms DESC LIMIT ?`, fromMs, toMs, fromMs, toMs, maxAnalysisRows)
	if errQuery != nil {
		return nil, fmt.Errorf("insights: account moves: %w", errQuery)
	}
	table := currentPrices()
	var out []AccountMove
	for rows.Next() {
		var move AccountMove
		var model, prevClass string
		var prevFailed int64
		if errScan := rows.Scan(&move.TS, &move.SessionID, &move.Provider, &model, &move.FromAuthID, &move.FromAccount,
			&move.ToAuthID, &move.ToAccount, &prevFailed, &prevClass, &move.CacheWriteTokens); errScan != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("insights: scan account move: %w", errScan)
		}
		move.Cause = "affinity"
		if prevFailed != 0 {
			move.Cause = "failover"
			if prevClass != "" {
				move.Cause = "failover after " + prevClass
			}
		}
		if write, read, ok := table.cacheRates(model); ok {
			move.ExtraUSD = float64(move.CacheWriteTokens) * (write - read)
		}
		out = append(out, move)
	}
	return out, closeRows(rows)
}

// FirstWrite is a thread whose first request wrote an unusually large prompt cache.
type FirstWrite struct {
	TS               int64   `json:"ts"`
	SessionID        string  `json:"session_id"`
	ClientIP         string  `json:"client_ip"`
	ClientKind       string  `json:"client_kind"`
	Harness          string  `json:"harness"`
	HarnessWarning   bool    `json:"harness_warning"`
	Model            string  `json:"model"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	CostUSD          float64 `json:"cost_usd"`
}

// IdleRewrite is a request that wrote the prompt cache again after the thread sat idle.
type IdleRewrite struct {
	TS               int64   `json:"ts"`
	SessionID        string  `json:"session_id"`
	ClientIP         string  `json:"client_ip"`
	ClientKind       string  `json:"client_kind"`
	Model            string  `json:"model"`
	GapMs            int64   `json:"gap_ms"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	ExtraUSD         float64 `json:"extra_usd"`
}

// FastModeRefusals counts fast-mode requests the provider refused, per machine and tool.
type FastModeRefusals struct {
	ClientIP   string `json:"client_ip"`
	ClientKind string `json:"client_kind"`
	Refusals   int64  `json:"refusals"`
	Sessions   int64  `json:"sessions"`
	LastMs     int64  `json:"last_ms"`
	LatencyMs  int64  `json:"latency_ms"`
}

// WasteReport lists patterns that spend quota without doing work.
type WasteReport struct {
	FirstWriteTokens int64              `json:"first_write_tokens"`
	IdleMinutes      int64              `json:"idle_minutes"`
	RewriteTokens    int64              `json:"rewrite_tokens"`
	FirstWrites      []FirstWrite       `json:"first_writes"`
	IdleRewrites     []IdleRewrite      `json:"idle_rewrites"`
	FastMode         []FastModeRefusals `json:"fast_mode"`
	Moves            []AccountMove      `json:"moves"`
}

func (s *Store) waste(ctx context.Context, from, to time.Time) (*WasteReport, error) {
	out := &WasteReport{
		FirstWriteTokens: int64(envInt("INSIGHTS_WASTE_FIRST_WRITE", defaultFirstWriteTokens)),
		IdleMinutes:      int64(envInt("INSIGHTS_WASTE_IDLE_MINUTES", defaultIdleMinutes)),
		RewriteTokens:    int64(envInt("INSIGHTS_WASTE_REWRITE_TOKENS", defaultRewriteTokens)),
	}
	var errPart error
	if out.FirstWrites, errPart = s.firstWrites(ctx, from, to, out.FirstWriteTokens); errPart != nil {
		return nil, errPart
	}
	if out.IdleRewrites, errPart = s.idleRewrites(ctx, from, to, time.Duration(out.IdleMinutes)*time.Minute, out.RewriteTokens); errPart != nil {
		return nil, errPart
	}
	if out.FastMode, errPart = s.fastModeRefusals(ctx, from, to); errPart != nil {
		return nil, errPart
	}
	if out.Moves, errPart = s.accountMoves(ctx, from, to); errPart != nil {
		return nil, errPart
	}
	return out, nil
}

// firstWrites lists sessions whose first successful request in [from, to) wrote at least threshold
// cache tokens.
func (s *Store) firstWrites(ctx context.Context, from, to time.Time, threshold int64) ([]FirstWrite, error) {
	fromMs, toMs := from.UnixMilli(), to.UnixMilli()
	rows, errQuery := s.db.QueryContext(ctx, `SELECT r.ts_ms, r.session_id, COALESCE(r.client_ip,''), COALESCE(r.client_kind,''),
			COALESCE(r.user_agent,''), r.provider, COALESCE(r.model,''), r.cache_write_tokens
		FROM requests r JOIN (
			SELECT session_id, MIN(ts_ms) AS first_ts FROM requests WHERE failed = 0 AND `+sessionScope+`
			GROUP BY session_id HAVING MIN(ts_ms) >= ? AND MIN(ts_ms) < ?) f
			ON r.session_id = f.session_id AND r.ts_ms = f.first_ts AND r.failed = 0
		WHERE r.cache_write_tokens >= ? ORDER BY r.ts_ms DESC LIMIT ?`, fromMs, toMs, fromMs, toMs, threshold, maxAnalysisRows)
	if errQuery != nil {
		return nil, fmt.Errorf("insights: first writes: %w", errQuery)
	}
	table := currentPrices()
	var out []FirstWrite
	seen := make(map[string]bool)
	for rows.Next() {
		var item FirstWrite
		var userAgent, provider string
		if errScan := rows.Scan(&item.TS, &item.SessionID, &item.ClientIP, &item.ClientKind, &userAgent, &provider,
			&item.Model, &item.CacheWriteTokens); errScan != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("insights: scan first write: %w", errScan)
		}
		if seen[item.SessionID] {
			continue
		}
		seen[item.SessionID] = true
		item.Harness, item.HarnessWarning = harnessFor(item.ClientKind, userAgent, provider)
		if write, _, ok := table.cacheRates(item.Model); ok {
			item.CostUSD = float64(item.CacheWriteTokens) * write
		}
		out = append(out, item)
	}
	return out, closeRows(rows)
}

// idleRewrites lists requests in [from, to) that followed a gap of at least idle in their session and
// wrote at least minTokens cache tokens, more than they read.
func (s *Store) idleRewrites(ctx context.Context, from, to time.Time, idle time.Duration, minTokens int64) ([]IdleRewrite, error) {
	fromMs, toMs := from.UnixMilli(), to.UnixMilli()
	rows, errQuery := s.db.QueryContext(ctx, `WITH s AS (
			SELECT ts_ms, session_id, COALESCE(client_ip,'') AS client_ip, COALESCE(client_kind,'') AS client_kind,
				COALESCE(model,'') AS model, cache_write_tokens, cache_read_tokens,
				LAG(ts_ms) OVER (PARTITION BY session_id ORDER BY ts_ms, id) AS prev_ts
			FROM requests WHERE failed = 0 AND `+sessionScope+`)
		SELECT ts_ms, session_id, client_ip, client_kind, model, ts_ms - prev_ts, cache_write_tokens, cache_read_tokens
		FROM s WHERE prev_ts IS NOT NULL AND ts_ms - prev_ts >= ? AND cache_write_tokens >= ?
			AND cache_write_tokens > cache_read_tokens AND ts_ms >= ? AND ts_ms < ?
		ORDER BY ts_ms DESC LIMIT ?`, fromMs, toMs, idle.Milliseconds(), minTokens, fromMs, toMs, maxAnalysisRows)
	if errQuery != nil {
		return nil, fmt.Errorf("insights: idle rewrites: %w", errQuery)
	}
	table := currentPrices()
	var out []IdleRewrite
	for rows.Next() {
		var item IdleRewrite
		if errScan := rows.Scan(&item.TS, &item.SessionID, &item.ClientIP, &item.ClientKind, &item.Model, &item.GapMs,
			&item.CacheWriteTokens, &item.CacheReadTokens); errScan != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("insights: scan idle rewrite: %w", errScan)
		}
		if write, read, ok := table.cacheRates(item.Model); ok {
			item.ExtraUSD = float64(item.CacheWriteTokens) * (write - read)
		}
		out = append(out, item)
	}
	return out, closeRows(rows)
}

// fastModeRefusals counts fast-mode refusals in [from, to) per machine and tool.
func (s *Store) fastModeRefusals(ctx context.Context, from, to time.Time) ([]FastModeRefusals, error) {
	fromMs, toMs := from.UnixMilli(), to.UnixMilli()
	rows, errQuery := s.db.QueryContext(ctx, `SELECT COALESCE(client_ip,''), COALESCE(client_kind,''), COUNT(*),
			COUNT(DISTINCT session_id), MAX(ts_ms), COALESCE(SUM(latency_ms),0)
		FROM requests WHERE error_class = ? AND ts_ms >= ? AND ts_ms < ?
		GROUP BY 1, 2 ORDER BY 3 DESC`, errorClassFastMode, fromMs, toMs)
	if errQuery != nil {
		return nil, fmt.Errorf("insights: fast mode refusals: %w", errQuery)
	}
	var out []FastModeRefusals
	for rows.Next() {
		var item FastModeRefusals
		if errScan := rows.Scan(&item.ClientIP, &item.ClientKind, &item.Refusals, &item.Sessions, &item.LastMs, &item.LatencyMs); errScan != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("insights: scan fast mode refusals: %w", errScan)
		}
		out = append(out, item)
	}
	return out, closeRows(rows)
}

// AccountUsage is one credential's share of traffic and its value against the plan price.
type AccountUsage struct {
	AuthID        string   `json:"auth_id"`
	Provider      string   `json:"provider"`
	Account       string   `json:"account"`
	Plan          string   `json:"plan"`
	MonthlyUSD    *float64 `json:"monthly_usd"`
	MonthlySource string   `json:"monthly_source"`
	Totals
	RateLimited int64 `json:"rate_limited"`
	// Cost30dUSD is the API-equivalent cost over the last 30 days; CoverageDays is how much of that
	// window has recorded data, so a new account's monthly pace can be estimated.
	Cost30dUSD   float64 `json:"cost_30d_usd"`
	CoverageDays float64 `json:"coverage_days"`
}

// AccountSeriesPoint is one account's usage in one time bucket.
type AccountSeriesPoint struct {
	T           int64   `json:"t"`
	AuthID      string  `json:"auth_id"`
	Requests    int64   `json:"requests"`
	TotalTokens int64   `json:"total_tokens"`
	CostUSD     float64 `json:"cost_usd"`
}

// AccountsReport describes how traffic spreads over each provider's accounts.
type AccountsReport struct {
	BucketMs int64                `json:"bucket_ms"`
	Accounts []AccountUsage       `json:"accounts"`
	Series   []AccountSeriesPoint `json:"series"`
	Moves    []AccountMove        `json:"moves"`
}

// defaultPlanUSD is the list price of known ChatGPT plans, per month. Claude credentials do not
// report their plan, so their price has to be set on the dashboard.
var defaultPlanUSD = map[string]float64{
	"plus":    20,
	"pro":     200,
	"prolite": 100,
	"promax":  500,
}

func (s *Store) accountsReport(ctx context.Context, from, to time.Time, bucket, offset time.Duration, now time.Time) (*AccountsReport, error) {
	fromMs, toMs := from.UnixMilli(), to.UnixMilli()
	bucketMs, offsetMs := bucket.Milliseconds(), offset.Milliseconds()
	out := &AccountsReport{BucketMs: bucketMs}

	usage := make(map[string]*AccountUsage)
	var order []string
	rows, errQuery := s.db.QueryContext(ctx, `SELECT COALESCE(auth_id,''), MAX(provider), COALESCE(MAX(account),''), `+totalsColumns+`,
			COALESCE(SUM(CASE WHEN error_class = 'rate_limit' THEN 1 ELSE 0 END),0)
		FROM requests WHERE ts_ms >= ? AND ts_ms < ? AND COALESCE(auth_id,'') <> '' GROUP BY auth_id`, fromMs, toMs)
	if errQuery != nil {
		return nil, fmt.Errorf("insights: account usage: %w", errQuery)
	}
	var latency, ttft sql.NullFloat64
	for rows.Next() {
		row := &AccountUsage{}
		targets := append([]any{&row.AuthID, &row.Provider, &row.Account}, totalsTargets(&row.Totals, &latency, &ttft)...)
		if errScan := rows.Scan(append(targets, &row.RateLimited)...); errScan != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("insights: scan account usage: %w", errScan)
		}
		finishTotals(&row.Totals, latency, ttft)
		usage[row.AuthID] = row
		order = append(order, row.AuthID)
	}
	if errClose := closeRows(rows); errClose != nil {
		return nil, errClose
	}

	known, errKnown := s.db.QueryContext(ctx, `SELECT auth_id, provider, COALESCE(label,''), COALESCE(plan,''), monthly_usd FROM accounts`)
	if errKnown != nil {
		return nil, fmt.Errorf("insights: accounts: %w", errKnown)
	}
	for known.Next() {
		var authID, provider, label, plan string
		var monthly sql.NullFloat64
		if errScan := known.Scan(&authID, &provider, &label, &plan, &monthly); errScan != nil {
			_ = known.Close()
			return nil, fmt.Errorf("insights: scan account: %w", errScan)
		}
		row := usage[authID]
		if row == nil {
			row = &AccountUsage{AuthID: authID, Provider: provider, Account: label}
			usage[authID] = row
			order = append(order, authID)
		}
		if row.Account == "" {
			row.Account = label
		}
		row.Plan = plan
		if monthly.Valid {
			value := monthly.Float64
			row.MonthlyUSD, row.MonthlySource = &value, "set"
		} else if value, ok := defaultPlanUSD[strings.ToLower(plan)]; ok {
			row.MonthlyUSD, row.MonthlySource = &value, "plan"
		}
	}
	if errClose := closeRows(known); errClose != nil {
		return nil, errClose
	}

	monthStart := now.Add(-30 * 24 * time.Hour).UnixMilli()
	rows, errQuery = s.db.QueryContext(ctx, `SELECT auth_id, COALESCE(SUM(CASE WHEN ts_ms >= ? THEN cost_usd END),0), MIN(ts_ms)
		FROM requests WHERE COALESCE(auth_id,'') <> '' GROUP BY auth_id`, monthStart)
	if errQuery != nil {
		return nil, fmt.Errorf("insights: account monthly cost: %w", errQuery)
	}
	for rows.Next() {
		var authID string
		var cost float64
		var firstMs int64
		if errScan := rows.Scan(&authID, &cost, &firstMs); errScan != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("insights: scan account monthly cost: %w", errScan)
		}
		if row := usage[authID]; row != nil {
			row.Cost30dUSD = cost
			row.CoverageDays = math.Min(30, float64(now.UnixMilli()-max(firstMs, monthStart))/float64(24*time.Hour/time.Millisecond))
		}
	}
	if errClose := closeRows(rows); errClose != nil {
		return nil, errClose
	}
	for _, authID := range order {
		out.Accounts = append(out.Accounts, *usage[authID])
	}
	sort.SliceStable(out.Accounts, func(i, j int) bool {
		if out.Accounts[i].Provider != out.Accounts[j].Provider {
			return out.Accounts[i].Provider < out.Accounts[j].Provider
		}
		return out.Accounts[i].TotalTokens > out.Accounts[j].TotalTokens
	})

	rows, errQuery = s.db.QueryContext(ctx, `SELECT ((ts_ms + ?) / ?) * ? - ? AS bucket, auth_id, COUNT(*),
			COALESCE(SUM(total_tokens),0), COALESCE(SUM(cost_usd),0)
		FROM requests WHERE ts_ms >= ? AND ts_ms < ? AND COALESCE(auth_id,'') <> '' AND failed = 0
		GROUP BY bucket, auth_id ORDER BY bucket`, offsetMs, bucketMs, bucketMs, offsetMs, fromMs, toMs)
	if errQuery != nil {
		return nil, fmt.Errorf("insights: account series: %w", errQuery)
	}
	for rows.Next() {
		var point AccountSeriesPoint
		if errScan := rows.Scan(&point.T, &point.AuthID, &point.Requests, &point.TotalTokens, &point.CostUSD); errScan != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("insights: scan account series: %w", errScan)
		}
		out.Series = append(out.Series, point)
	}
	if errClose := closeRows(rows); errClose != nil {
		return nil, errClose
	}

	moves, errMoves := s.accountMoves(ctx, from, to)
	if errMoves != nil {
		return nil, errMoves
	}
	out.Moves = moves
	return out, nil
}

// setMonthlyPrice stores an account's plan price; nil clears it back to the plan default.
func (s *Store) setMonthlyPrice(ctx context.Context, authID string, usd *float64) (bool, error) {
	var value any
	if usd != nil {
		value = *usd
	}
	result, errExec := s.db.ExecContext(ctx, `UPDATE accounts SET monthly_usd = ? WHERE auth_id = ?`, value, authID)
	if errExec != nil {
		return false, fmt.Errorf("insights: set plan price: %w", errExec)
	}
	affected, errAffected := result.RowsAffected()
	if errAffected != nil {
		return false, fmt.Errorf("insights: set plan price: %w", errAffected)
	}
	return affected > 0, nil
}

// ErrorGroup counts failed requests by cause, machine and tool.
type ErrorGroup struct {
	Provider   string `json:"provider"`
	ErrorClass string `json:"error_class"`
	ClientIP   string `json:"client_ip"`
	ClientKind string `json:"client_kind"`
	StatusCode int64  `json:"status_code"`
	Count      int64  `json:"count"`
	LastMs     int64  `json:"last_ms"`
}

// ErrorPoint is the number of failures of one class in one time bucket.
type ErrorPoint struct {
	T          int64  `json:"t"`
	ErrorClass string `json:"error_class"`
	Count      int64  `json:"count"`
}

// AccessFailureGroup counts rejected keys per kind and source address.
type AccessFailureGroup struct {
	Kind     string `json:"kind"`
	ClientIP string `json:"client_ip"`
	Count    int64  `json:"count"`
	LastMs   int64  `json:"last_ms"`
	Route    string `json:"route"`
}

// LatencyRow holds latency and time-to-first-token percentiles for one model.
type LatencyRow struct {
	Provider    string `json:"provider"`
	Model       string `json:"model"`
	Requests    int64  `json:"requests"`
	LatencyP50  int64  `json:"latency_p50"`
	LatencyP90  int64  `json:"latency_p90"`
	LatencyP99  int64  `json:"latency_p99"`
	TTFTSamples int64  `json:"ttft_samples"`
	TTFTP50     int64  `json:"ttft_p50"`
	TTFTP90     int64  `json:"ttft_p90"`
	TTFTP99     int64  `json:"ttft_p99"`
}

// HealthReport groups failures by cause and summarizes response times.
type HealthReport struct {
	BucketMs       int64                `json:"bucket_ms"`
	Errors         []ErrorGroup         `json:"errors"`
	Series         []ErrorPoint         `json:"series"`
	AccessFailures []AccessFailureGroup `json:"access_failures"`
	Latency        []LatencyRow         `json:"latency"`
}

func (s *Store) health(ctx context.Context, from, to time.Time, bucket, offset time.Duration) (*HealthReport, error) {
	fromMs, toMs := from.UnixMilli(), to.UnixMilli()
	bucketMs, offsetMs := bucket.Milliseconds(), offset.Milliseconds()
	out := &HealthReport{BucketMs: bucketMs}

	rows, errQuery := s.db.QueryContext(ctx, `SELECT provider, COALESCE(error_class,'other'), COALESCE(client_ip,''),
			COALESCE(client_kind,''), COALESCE(MAX(status_code),0), COUNT(*), MAX(ts_ms)
		FROM requests WHERE failed = 1 AND ts_ms >= ? AND ts_ms < ?
		GROUP BY 1, 2, 3, 4 ORDER BY 6 DESC LIMIT ?`, fromMs, toMs, maxAnalysisRows)
	if errQuery != nil {
		return nil, fmt.Errorf("insights: errors: %w", errQuery)
	}
	for rows.Next() {
		var row ErrorGroup
		if errScan := rows.Scan(&row.Provider, &row.ErrorClass, &row.ClientIP, &row.ClientKind, &row.StatusCode, &row.Count, &row.LastMs); errScan != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("insights: scan errors: %w", errScan)
		}
		out.Errors = append(out.Errors, row)
	}
	if errClose := closeRows(rows); errClose != nil {
		return nil, errClose
	}

	rows, errQuery = s.db.QueryContext(ctx, `SELECT ((ts_ms + ?) / ?) * ? - ? AS bucket, COALESCE(error_class,'other'), COUNT(*)
		FROM requests WHERE failed = 1 AND ts_ms >= ? AND ts_ms < ? GROUP BY bucket, 2 ORDER BY bucket`,
		offsetMs, bucketMs, bucketMs, offsetMs, fromMs, toMs)
	if errQuery != nil {
		return nil, fmt.Errorf("insights: error series: %w", errQuery)
	}
	for rows.Next() {
		var point ErrorPoint
		if errScan := rows.Scan(&point.T, &point.ErrorClass, &point.Count); errScan != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("insights: scan error series: %w", errScan)
		}
		out.Series = append(out.Series, point)
	}
	if errClose := closeRows(rows); errClose != nil {
		return nil, errClose
	}

	rows, errQuery = s.db.QueryContext(ctx, `SELECT kind, COALESCE(client_ip,''), COUNT(*), MAX(ts_ms), COALESCE(MAX(route),'')
		FROM access_failures WHERE ts_ms >= ? AND ts_ms < ? GROUP BY 1, 2 ORDER BY 3 DESC LIMIT ?`, fromMs, toMs, maxAnalysisRows)
	if errQuery != nil {
		return nil, fmt.Errorf("insights: access failures: %w", errQuery)
	}
	for rows.Next() {
		var row AccessFailureGroup
		if errScan := rows.Scan(&row.Kind, &row.ClientIP, &row.Count, &row.LastMs, &row.Route); errScan != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("insights: scan access failures: %w", errScan)
		}
		out.AccessFailures = append(out.AccessFailures, row)
	}
	if errClose := closeRows(rows); errClose != nil {
		return nil, errClose
	}

	latency, errLatency := s.latencyPercentiles(ctx, fromMs, toMs)
	if errLatency != nil {
		return nil, errLatency
	}
	out.Latency = latency
	return out, nil
}

func (s *Store) latencyPercentiles(ctx context.Context, fromMs, toMs int64) ([]LatencyRow, error) {
	rows, errQuery := s.db.QueryContext(ctx, `SELECT provider, COALESCE(model,''), COALESCE(latency_ms,0), COALESCE(ttft_ms,0)
		FROM requests WHERE failed = 0 AND ts_ms >= ? AND ts_ms < ? ORDER BY ts_ms DESC LIMIT ?`, fromMs, toMs, maxLatencySamples)
	if errQuery != nil {
		return nil, fmt.Errorf("insights: latency: %w", errQuery)
	}
	type samples struct {
		provider, model string
		latency, ttft   []int64
	}
	byModel := make(map[string]*samples)
	for rows.Next() {
		var provider, model string
		var latency, ttft int64
		if errScan := rows.Scan(&provider, &model, &latency, &ttft); errScan != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("insights: scan latency: %w", errScan)
		}
		key := provider + "|" + model
		entry := byModel[key]
		if entry == nil {
			entry = &samples{provider: provider, model: model}
			byModel[key] = entry
		}
		if latency > 0 {
			entry.latency = append(entry.latency, latency)
		}
		if ttft > 0 {
			entry.ttft = append(entry.ttft, ttft)
		}
	}
	if errClose := closeRows(rows); errClose != nil {
		return nil, errClose
	}
	out := make([]LatencyRow, 0, len(byModel))
	for _, entry := range byModel {
		if len(entry.latency) == 0 {
			continue
		}
		sort.Slice(entry.latency, func(i, j int) bool { return entry.latency[i] < entry.latency[j] })
		sort.Slice(entry.ttft, func(i, j int) bool { return entry.ttft[i] < entry.ttft[j] })
		out = append(out, LatencyRow{
			Provider:    entry.provider,
			Model:       entry.model,
			Requests:    int64(len(entry.latency)),
			LatencyP50:  percentile(entry.latency, 50),
			LatencyP90:  percentile(entry.latency, 90),
			LatencyP99:  percentile(entry.latency, 99),
			TTFTSamples: int64(len(entry.ttft)),
			TTFTP50:     percentile(entry.ttft, 50),
			TTFTP90:     percentile(entry.ttft, 90),
			TTFTP99:     percentile(entry.ttft, 99),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Requests > out[j].Requests })
	return out, nil
}

// percentile returns the nearest-rank percentile of sorted values, or 0 when there are none.
func percentile(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(p/100*float64(len(sorted)))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	return sorted[rank]
}
