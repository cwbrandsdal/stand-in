package insights

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, errOpen := OpenStore(filepath.Join(t.TempDir(), "insights.db"))
	if errOpen != nil {
		t.Fatalf("open: %v", errOpen)
	}
	t.Cleanup(func() {
		if errClose := store.Close(); errClose != nil {
			t.Errorf("close: %v", errClose)
		}
	})
	return store
}

func near(a, b float64) bool { return math.Abs(a-b) <= 1e-9*math.Max(1, math.Abs(b)) }

func TestClassifyFailure(t *testing.T) {
	cases := []struct {
		status int
		code   string
		body   string
		want   string
	}{
		{429, "", `{"error":{"type":"rate_limit_error","message":"Fast request rejected: usage credits are required"}}`, errorClassFastMode},
		{429, "", `{"type":"error","error":{"type":"rate_limit_error","message":"This request would exceed your rate limit"}}`, errorClassRateLimit},
		{529, "", `{"type":"overloaded_error"}`, errorClassOverloaded},
		{401, "", `{"error":"token_invalidated"}`, errorClassUpstreamAuth},
		{400, "", "prompt is too long: 1200000 tokens > 1000000 maximum", errorClassContextLength},
		{400, "", "unknown provider for model gpt-5.5", errorClassModelNotFound},
		{400, "", "invalid tool schema", errorClassBadRequest},
		{499, "", "", errorClassClientCancelled},
		{200, coreusage.FailureStreamTruncatedCode, "", errorClassStreamTruncated},
		{502, "", "bad gateway", errorClassUpstreamError},
		{0, "", "", errorClassOther},
	}
	for _, tc := range cases {
		if got := classifyFailure(tc.status, tc.code, tc.body); got != tc.want {
			t.Errorf("classifyFailure(%d, %q, %q) = %q, want %q", tc.status, tc.code, tc.body, got, tc.want)
		}
	}
}

const testPrices = `{
	"claude-opus-9": {"litellm_provider": "anthropic", "input_cost_per_token": 1e-6, "output_cost_per_token": 1e-5,
		"cache_read_input_token_cost": 1e-7, "cache_creation_input_token_cost": 2e-6, "provider_specific_entry": {"fast": 2}},
	"gpt-9": {"litellm_provider": "openai", "input_cost_per_token": 2e-6, "output_cost_per_token": 8e-6,
		"cache_read_input_token_cost": 2e-7, "input_cost_per_token_priority": 4e-6, "output_cost_per_token_priority": 1.6e-5,
		"cache_read_input_token_cost_priority": 4e-7, "input_cost_per_token_above_272k_tokens": 4e-6,
		"output_cost_per_token_above_272k_tokens": 1.2e-5, "input_cost_per_token_ultrafast": 1e-5,
		"output_cost_per_token_ultrafast": 4e-5},
	"gemini-9": {"litellm_provider": "gemini", "input_cost_per_token": 1e-6, "output_cost_per_token": 1e-6},
	"openrouter/claude-opus-9": {"input_cost_per_token": 9, "output_cost_per_token": 9},
	"claude-no-output": {"litellm_provider": "anthropic", "input_cost_per_token": 1e-6},
	"sample_spec": {"input_cost_per_token": 0}
}`

func TestPriceTableCost(t *testing.T) {
	table, errParse := parsePriceTable([]byte(testPrices), "test")
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	if len(table.models) != 2 {
		t.Fatalf("models = %d, want claude-opus-9 and gpt-9 only", len(table.models))
	}
	cases := []struct {
		model, tier                string
		input, read, write, output int64
		want                       float64
	}{
		{"claude-opus-9", "", 1000, 10000, 2000, 500, 0.011},
		{"claude-opus-9", "fast", 1000, 10000, 2000, 500, 0.022},
		{"anthropic/claude-opus-9-20261001", "auto", 1000, 10000, 2000, 500, 0.011},
		{"claude-opus-9-thinking", "", 1000, 10000, 2000, 500, 0.011},
		{"gpt-9", "priority", 1000, 10000, 0, 500, 0.016},
		{"gpt-9", "fast", 1000, 10000, 0, 500, 0.016},
		{"gpt-9", "", 300000, 0, 0, 500, 1.206},
		{"gpt-9", "ultrafast", 1000, 0, 0, 500, 0.03},
		{"GPT-9", "flex", 1000, 0, 0, 500, 0.006},
	}
	for _, tc := range cases {
		got, ok := table.costUSD(tc.model, tc.tier, tc.input, tc.read, tc.write, tc.output)
		if !ok || !near(got, tc.want) {
			t.Errorf("costUSD(%s, %s) = %v, %v; want %v", tc.model, tc.tier, got, ok, tc.want)
		}
	}
	if _, ok := table.costUSD("gemini-9", "", 1, 1, 1, 1); ok {
		t.Error("priced a model from another provider")
	}
	refreshed, _ := parsePriceTable([]byte(`{"gpt-9": {"litellm_provider": "openai", "input_cost_per_token": 3e-6, "output_cost_per_token": 9e-6}}`), "refresh")
	if added := refreshed.keepMissing(table); added != 1 || refreshed.lookup("claude-opus-9") == nil || refreshed.lookup("gpt-9").rates[priceInput] != 3e-6 {
		t.Errorf("keepMissing added %d; refreshed prices = %+v", added, refreshed.models)
	}
	write, read, ok := table.cacheRates("claude-opus-9")
	if !ok || !near(write, 2e-6) || !near(read, 1e-7) {
		t.Errorf("cacheRates = %v %v %v", write, read, ok)
	}
	if embedded := currentPrices(); embedded == nil || embedded.lookup("claude-opus-5-5") == nil || embedded.lookup("gpt-6.1-sol") == nil {
		t.Error("embedded prices miss the models in use")
	}
}

func TestMigrateClassifiesOldFailures(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, errOpen := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if errOpen != nil {
		t.Fatalf("open: %v", errOpen)
	}
	base := time.Date(2026, 10, 29, 22, 0, 0, 0, time.UTC).UnixMilli()
	statements := []string{
		`CREATE TABLE requests (id INTEGER PRIMARY KEY, ts_ms INTEGER NOT NULL, provider TEXT NOT NULL, model TEXT, auth_id TEXT,
			session_id TEXT, failed INTEGER NOT NULL DEFAULT 0, status_code INTEGER, total_tokens INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE accounts (auth_id TEXT PRIMARY KEY, provider TEXT NOT NULL, label TEXT, email TEXT, plan TEXT,
			first_seen_ms INTEGER NOT NULL, last_seen_ms INTEGER NOT NULL)`,
	}
	for _, statement := range statements {
		if _, errExec := db.Exec(statement); errExec != nil {
			t.Fatalf("create old schema: %v", errExec)
		}
	}
	rows := []struct {
		ts      int64
		prov    string
		session string
		failed  int
		status  int
	}{
		{base, "claude", "s1", 1, 429}, // retried successfully 3s later: fast mode
		{base + 3000, "claude", "s1", 0, 200},
		{base, "claude", "s2", 1, 429}, // never succeeded: rate limit
		{base, "codex", "s3", 1, 401},
		{base, "codex", "s3", 1, 499},
		{base, "claude", "s3", 1, 529},
		{base, "codex", "s3", 1, 502},
		{base, "codex", "s3", 1, 400},
		{base + 60000, "claude", "s4", 1, 429}, // success arrives after 20s: rate limit
		{base + 80000, "claude", "s4", 0, 200},
	}
	for _, row := range rows {
		if _, errExec := db.Exec(`INSERT INTO requests (ts_ms, provider, session_id, failed, status_code) VALUES (?,?,?,?,?)`,
			row.ts, row.prov, row.session, row.failed, row.status); errExec != nil {
			t.Fatalf("insert: %v", errExec)
		}
	}
	if errClose := db.Close(); errClose != nil {
		t.Fatalf("close: %v", errClose)
	}

	for attempt := 0; attempt < 2; attempt++ {
		store, errStore := OpenStore(path)
		if errStore != nil {
			t.Fatalf("open store (attempt %d): %v", attempt, errStore)
		}
		var version int
		_ = store.db.QueryRow(`PRAGMA user_version`).Scan(&version)
		if version != schemaVersion {
			t.Fatalf("user_version = %d", version)
		}
		got := map[string]int{}
		classRows, errQuery := store.db.Query(`SELECT error_class, COUNT(*) FROM requests WHERE failed = 1 GROUP BY 1`)
		if errQuery != nil {
			t.Fatalf("query: %v", errQuery)
		}
		for classRows.Next() {
			var class string
			var count int
			_ = classRows.Scan(&class, &count)
			got[class] = count
		}
		_ = classRows.Close()
		want := map[string]int{"fast_mode": 1, "rate_limit": 2, "upstream_auth": 1, "client_cancelled": 1, "overloaded": 1, "upstream_error": 1, "bad_request": 1}
		for class, count := range want {
			if got[class] != count {
				t.Fatalf("attempt %d: classes = %v, want %v", attempt, got, want)
			}
		}
		if _, errExec := store.db.Exec(`UPDATE accounts SET monthly_usd = 1`); errExec != nil {
			t.Fatalf("monthly_usd missing: %v", errExec)
		}
		if errClose := store.Close(); errClose != nil {
			t.Fatalf("close store: %v", errClose)
		}
	}
}

// seedAnalysis records three threads:
//   - A: Claude through the Codex harness, a 400K first write, then a cache rewrite after a 10 minute gap;
//   - B: Claude Code with a fast-mode refusal, then a move from account c1 to c2;
//   - C: a Codex thread that switches to a Claude model (a provider switch, not a move);
//
// plus a sign-in failure on c2.
func seedAnalysis(t *testing.T, store *Store, base time.Time) {
	t.Helper()
	priced := func(row requestRow) requestRow {
		row.TotalTokens = row.InputTokens + row.CacheReadTokens + row.CacheWriteTokens + row.OutputTokens
		if cost, ok := requestCost(row); ok && row.TotalTokens > 0 {
			row.CostUSD = sql.NullFloat64{Float64: cost, Valid: true}
		}
		if row.StatusCode == 0 {
			row.StatusCode = 200
		}
		return row
	}
	t3 := "t3-code/1.0 codex"
	rows := []requestRow{
		{TS: base, Provider: "claude", Model: "claude-opus-5-5", AuthID: "c1", Account: "a@example.com", SessionID: "A",
			ClientIP: "192.168.1.170", ClientKind: "t3", UserAgent: t3, InputTokens: 10, CacheWriteTokens: 400000, OutputTokens: 100, LatencyMs: 4000, TTFTMs: 900},
		{TS: base.Add(time.Minute), Provider: "claude", Model: "claude-opus-5-5", AuthID: "c1", Account: "a@example.com", SessionID: "A",
			ClientIP: "192.168.1.170", ClientKind: "t3", UserAgent: t3, InputTokens: 10, CacheReadTokens: 400000, CacheWriteTokens: 1000, OutputTokens: 100, LatencyMs: 2000, TTFTMs: 700},
		{TS: base.Add(11 * time.Minute), Provider: "claude", Model: "claude-opus-5-5", AuthID: "c1", Account: "a@example.com", SessionID: "A",
			ClientIP: "192.168.1.170", ClientKind: "t3", UserAgent: t3, InputTokens: 10, CacheWriteTokens: 420000, OutputTokens: 100, LatencyMs: 6000, TTFTMs: 1500},

		{TS: base.Add(time.Hour), Provider: "claude", Model: "claude-opus-5-5", AuthID: "c1", Account: "a@example.com", SessionID: "B",
			ClientIP: "192.168.1.171", ClientKind: "claude-code", Failed: true, StatusCode: 429, ErrorClass: errorClassFastMode, LatencyMs: 300},
		{TS: base.Add(time.Hour + 2*time.Second), Provider: "claude", Model: "claude-opus-5-5", AuthID: "c1", Account: "a@example.com", SessionID: "B",
			ClientIP: "192.168.1.171", ClientKind: "claude-code", InputTokens: 5, CacheWriteTokens: 30000, OutputTokens: 50, LatencyMs: 3000, TTFTMs: 800},
		{TS: base.Add(time.Hour + time.Minute), Provider: "claude", Model: "claude-opus-5-5", AuthID: "c2", Account: "b@example.com", SessionID: "B",
			ClientIP: "192.168.1.171", ClientKind: "claude-code", InputTokens: 5, CacheWriteTokens: 25000, OutputTokens: 50, LatencyMs: 3500, TTFTMs: 850},

		{TS: base.Add(2 * time.Hour), Provider: "codex", Model: "gpt-6.1-sol", AuthID: "x1", Account: "a@example.com", SessionID: "C",
			ClientIP: "127.0.0.1", ClientKind: "t3", UserAgent: t3, ServiceTier: "priority", InputTokens: 5000, CacheReadTokens: 20000, OutputTokens: 300, LatencyMs: 5000, TTFTMs: 2000},
		{TS: base.Add(2*time.Hour + 30*time.Second), Provider: "claude", Model: "claude-opus-5-5", AuthID: "c1", Account: "a@example.com", SessionID: "C",
			ClientIP: "127.0.0.1", ClientKind: "t3", UserAgent: t3, InputTokens: 5, CacheWriteTokens: 50000, OutputTokens: 30, LatencyMs: 2500, TTFTMs: 600},

		{TS: base.Add(3 * time.Hour), Provider: "claude", Model: "claude-opus-5-5", AuthID: "c2", Account: "b@example.com", SessionID: "D",
			ClientIP: "192.168.1.171", ClientKind: "claude-code", Failed: true, StatusCode: 401, ErrorClass: errorClassUpstreamAuth},
	}
	for i := range rows {
		rows[i] = priced(rows[i])
	}
	if errInsert := store.insertRequests(context.Background(), rows); errInsert != nil {
		t.Fatalf("insert: %v", errInsert)
	}
	if errUpsert := store.upsertAccounts(context.Background(), []accountRow{
		{AuthID: "c1", Provider: "claude", Label: "a@example.com", SeenAt: base},
		{AuthID: "c2", Provider: "claude", Label: "b@example.com", SeenAt: base},
		{AuthID: "x1", Provider: "codex", Label: "a@example.com", Plan: "pro", SeenAt: base},
	}); errUpsert != nil {
		t.Fatalf("upsert accounts: %v", errUpsert)
	}
}

func TestAnalysisReports(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	base := time.Date(2026, 10, 30, 8, 0, 0, 0, time.UTC)
	seedAnalysis(t, store, base)
	from, to := base.Add(-time.Hour), base.Add(4*time.Hour)

	threads, errThreads := store.threads(ctx, from, to, true, 10)
	if errThreads != nil {
		t.Fatalf("threads: %v", errThreads)
	}
	if len(threads) != 4 || threads[0].SessionID != "A" || !threads[0].HarnessWarning || threads[0].FirstCacheWrite != 400000 {
		t.Fatalf("threads = %+v", threads)
	}
	accounts := map[string]int64{}
	for _, thread := range threads {
		accounts[thread.SessionID] = thread.Accounts
	}
	if accounts["B"] != 2 || accounts["C"] != 1 {
		t.Fatalf("accounts per thread = %v, want B moved and C a provider switch", accounts)
	}
	recent, errRecent := store.threads(ctx, from, to, false, 10)
	if errRecent != nil || recent[0].SessionID != "D" {
		t.Fatalf("recent threads = %+v err=%v", recent, errRecent)
	}
	detail, errDetail := store.threadRequests(ctx, "A")
	if errDetail != nil || len(detail) != 3 || detail[0].CostUSD == nil || detail[2].CacheWriteTokens != 420000 {
		t.Fatalf("thread A = %+v err=%v", detail, errDetail)
	}

	waste, errWaste := store.waste(ctx, from, to)
	if errWaste != nil {
		t.Fatalf("waste: %v", errWaste)
	}
	if len(waste.FirstWrites) != 1 || waste.FirstWrites[0].SessionID != "A" || !waste.FirstWrites[0].HarnessWarning || waste.FirstWrites[0].CostUSD <= 0 {
		t.Fatalf("first writes = %+v", waste.FirstWrites)
	}
	if len(waste.IdleRewrites) != 1 || waste.IdleRewrites[0].GapMs != (10*time.Minute).Milliseconds() || waste.IdleRewrites[0].ExtraUSD <= 0 {
		t.Fatalf("idle rewrites = %+v", waste.IdleRewrites)
	}
	if len(waste.FastMode) != 1 || waste.FastMode[0].Refusals != 1 || waste.FastMode[0].ClientIP != "192.168.1.171" {
		t.Fatalf("fast mode = %+v", waste.FastMode)
	}
	if len(waste.Moves) != 1 || waste.Moves[0].SessionID != "B" || waste.Moves[0].FromAuthID != "c1" || waste.Moves[0].ToAuthID != "c2" ||
		waste.Moves[0].Cause != "affinity" || waste.Moves[0].CacheWriteTokens != 25000 {
		t.Fatalf("moves = %+v", waste.Moves)
	}

	price := 100.0
	if found, errSet := store.setMonthlyPrice(ctx, "c1", &price); errSet != nil || !found {
		t.Fatalf("set price: %v %v", found, errSet)
	}
	if found, _ := store.setMonthlyPrice(ctx, "missing", &price); found {
		t.Fatal("set a price for an unknown account")
	}
	report, errReport := store.accountsReport(ctx, from, to, time.Hour, 0, base.Add(4*time.Hour))
	if errReport != nil {
		t.Fatalf("accounts: %v", errReport)
	}
	byID := map[string]AccountUsage{}
	for _, account := range report.Accounts {
		byID[account.AuthID] = account
	}
	if c1 := byID["c1"]; c1.MonthlyUSD == nil || *c1.MonthlyUSD != 100 || c1.MonthlySource != "set" || c1.Cost30dUSD <= 0 || c1.CoverageDays <= 0 {
		t.Fatalf("c1 = %+v", c1)
	}
	if x1 := byID["x1"]; x1.MonthlyUSD == nil || *x1.MonthlyUSD != 200 || x1.MonthlySource != "plan" {
		t.Fatalf("x1 = %+v", x1)
	}
	if c2 := byID["c2"]; c2.MonthlyUSD != nil || c2.Requests != 2 {
		t.Fatalf("c2 = %+v", c2)
	}
	if len(report.Series) == 0 || len(report.Moves) != 1 {
		t.Fatalf("series=%d moves=%d", len(report.Series), len(report.Moves))
	}

	health, errHealth := store.health(ctx, from, to, time.Hour, 0)
	if errHealth != nil {
		t.Fatalf("health: %v", errHealth)
	}
	classes := map[string]int64{}
	for _, group := range health.Errors {
		classes[group.ErrorClass] += group.Count
	}
	if classes[errorClassFastMode] != 1 || classes[errorClassUpstreamAuth] != 1 || len(health.Series) != 2 {
		t.Fatalf("errors = %+v series = %+v", health.Errors, health.Series)
	}
	if len(health.Latency) != 2 || health.Latency[0].Model != "claude-opus-5-5" || health.Latency[0].Requests != 6 || health.Latency[0].LatencyP50 != 3000 {
		t.Fatalf("latency = %+v", health.Latency)
	}
}

func TestPercentile(t *testing.T) {
	values := []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	for p, want := range map[float64]int64{50: 5, 90: 9, 99: 10, 0: 1} {
		if got := percentile(values, p); got != want {
			t.Errorf("p%v = %d, want %d", p, got, want)
		}
	}
	if percentile(nil, 50) != 0 {
		t.Error("empty percentile")
	}
}

func TestRequestAlertsAndDedupe(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	base := time.Date(2026, 10, 30, 8, 0, 0, 0, time.UTC)
	seedAnalysis(t, store, base)
	t.Setenv("INSIGHTS_HOST_LABELS", "192.168.1.170=ProArt,192.168.1.171=Deathstar")

	rows, errAlerts := store.requestAlerts(ctx, defaultAlertSettings(), base.Add(-time.Minute), base.Add(4*time.Hour))
	if errAlerts != nil {
		t.Fatalf("alerts: %v", errAlerts)
	}
	kinds := map[string]alertRow{}
	for _, row := range rows {
		kinds[row.Kind] = row
	}
	if auth := kinds[alertUpstreamAuth]; auth.Severity != severityCritical || !strings.Contains(auth.Body, "b@example.com") {
		t.Fatalf("sign-in alert = %+v", auth)
	}
	if first := kinds[alertFirstWrite]; first.Severity != severityWarning || !strings.Contains(first.Body, "ProArt") || first.DedupeKey != alertFirstWrite+"|A" {
		t.Fatalf("first write alert = %+v", first)
	}
	if move := kinds[alertAccountMoves]; move.Provider != "claude" || !strings.Contains(move.Body, "1 session moved") {
		t.Fatalf("move alert = %+v", move)
	}
	if _, has := kinds[alertFastMode]; has {
		t.Fatal("one refusal is below the default threshold of 5")
	}

	settings := defaultAlertSettings()
	settings.FastModeRefusals = 1
	settings.WasteAlerts = true
	hourRows, errHour := store.requestAlerts(ctx, settings, base.Add(time.Hour), base.Add(time.Hour+5*time.Minute))
	if errHour != nil {
		t.Fatalf("alerts: %v", errHour)
	}
	found := false
	for _, row := range hourRows {
		if row.Kind == alertFastMode && strings.Contains(row.Title, "Deathstar") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no fast-mode alert in %+v", hourRows)
	}

	settings.WasteAlerts = false
	quiet, _ := store.requestAlerts(ctx, settings, base.Add(-time.Minute), base.Add(4*time.Hour))
	if len(quiet) != 1 || quiet[0].Kind != alertUpstreamAuth {
		t.Fatalf("with waste alerts off = %+v", quiet)
	}

	inserted, errInsert := store.insertAlert(ctx, rows[0], pushStatusPending)
	again, errAgain := store.insertAlert(ctx, rows[0], pushStatusPending)
	if errInsert != nil || errAgain != nil || !inserted || again {
		t.Fatalf("dedupe: inserted=%v again=%v errs=%v %v", inserted, again, errInsert, errAgain)
	}
	listed, _ := store.alerts(ctx, 0, 10)
	if len(listed) != 1 || listed[0].Group == "" || listed[0].AckedMs != 0 {
		t.Fatalf("alerts = %+v", listed)
	}
	if count, errAck := store.ackAlerts(ctx, 0, base); errAck != nil || count != 1 {
		t.Fatalf("ack all = %d %v", count, errAck)
	}
}

func TestQuotaAlerts(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	settings := defaultAlertSettings()
	weekly := func(used float64, resetsIn time.Duration) QuotaWindow {
		return QuotaWindow{Key: "7d", Label: "Weekly", UsedPct: used, ResetsAt: now.Add(resetsIn), WindowMinutes: 10080, ObservedAt: now}
	}
	noHistory := func(string, time.Time) (quotaPoint, bool) { return quotaPoint{}, false }

	rows := quotaAlerts(settings, "claude", "c1", "a@example.com", false, []QuotaWindow{weekly(85, 48*time.Hour)}, now, noHistory)
	if len(rows) != 1 || rows[0].Kind != alertQuota || rows[0].Severity != severityWarning || !strings.Contains(rows[0].Body, "a@example.com") {
		t.Fatalf("threshold = %+v", rows)
	}
	// A Codex-style reset a few seconds later keeps the same period and dedupe key.
	again := quotaAlerts(settings, "claude", "c1", "a@example.com", false, []QuotaWindow{weekly(86, 48*time.Hour+7*time.Second)}, now, noHistory)
	if again[0].DedupeKey != rows[0].DedupeKey {
		t.Fatalf("dedupe key changed: %s vs %s", again[0].DedupeKey, rows[0].DedupeKey)
	}
	if rows[0].Title != "Claude weekly limit at 85%" || !rows[0].ResolvesAt.Equal(now.Add(48*time.Hour)) {
		t.Fatalf("title %q resolves %v", rows[0].Title, rows[0].ResolvesAt)
	}
	// 80% and 100% of one window period are one condition for Luna, which escalates the alert it has.
	fullWeek := quotaAlerts(settings, "claude", "c1", "a@example.com", true, []QuotaWindow{weekly(100, 48*time.Hour)}, now, noHistory)
	if len(fullWeek) != 1 || fullWeek[0].Fingerprint != rows[0].Fingerprint || fullWeek[0].DedupeKey == rows[0].DedupeKey ||
		fullWeek[0].Title != "Claude weekly limit reached (a@example.com)" {
		t.Fatalf("exhausted week = %+v, warning fingerprint %s", fullWeek, rows[0].Fingerprint)
	}

	full := QuotaWindow{Key: "5h", Label: "5-hour", UsedPct: 100, ResetsAt: now.Add(time.Hour), WindowMinutes: 300, ObservedAt: now}
	rows = quotaAlerts(settings, "claude", "c1", "a", false, []QuotaWindow{full}, now, noHistory)
	if len(rows) != 1 || rows[0].Kind != alertQuotaExceeded || rows[0].Severity != severityCritical {
		t.Fatalf("exhausted = %+v", rows)
	}

	fastBurn := func(string, time.Time) (quotaPoint, bool) {
		return quotaPoint{TS: now.Add(-7 * time.Hour), UsedPct: 30, ResetsAt: now.Add(72 * time.Hour)}, true
	}
	rows = quotaAlerts(settings, "claude", "c1", "a", false, []QuotaWindow{weekly(60, 72*time.Hour)}, now, fastBurn)
	if len(rows) != 1 || rows[0].Kind != alertProjection || !strings.Contains(rows[0].Body, "5.0%/h") {
		t.Fatalf("projection = %+v", rows)
	}
	slowBurn := func(string, time.Time) (quotaPoint, bool) {
		return quotaPoint{TS: now.Add(-7 * time.Hour), UsedPct: 59, ResetsAt: now.Add(72 * time.Hour)}, true
	}
	if rows = quotaAlerts(settings, "claude", "c1", "a", false, []QuotaWindow{weekly(60, 72*time.Hour)}, now, slowBurn); len(rows) != 0 {
		t.Fatalf("slow burn raised %+v", rows)
	}
	if rows = quotaAlerts(settings, "claude", "c1", "a", false, []QuotaWindow{weekly(40, 72*time.Hour)}, now, fastBurn); len(rows) != 0 {
		t.Fatalf("below projection minimum raised %+v", rows)
	}
	if rows = quotaAlerts(settings, "claude", "c1", "a", false, []QuotaWindow{weekly(95, -time.Minute)}, now, noHistory); len(rows) != 0 {
		t.Fatalf("a window that already reset raised %+v", rows)
	}

	// Without history from this period, the burn is averaged since the window started (1h ago).
	young := QuotaWindow{Key: "5h", UsedPct: 60, ResetsAt: now.Add(4 * time.Hour), WindowMinutes: 300, ObservedAt: now}
	earlier := quotaPoint{TS: now.Add(-6 * time.Hour), UsedPct: 90, ResetsAt: now.Add(-time.Hour)}
	projected, ok := projectWindow(young, earlier, true, now)
	if !ok || !near(projected.PerHour, 60) || projected.Lookback != time.Hour {
		t.Fatalf("young window projection = %+v %v", projected, ok)
	}

	off := settings
	off.QuotaPct, off.ProjectionMinPct = 0, 0
	if rows = quotaAlerts(off, "claude", "c1", "a", false, []QuotaWindow{weekly(90, 48*time.Hour)}, now, fastBurn); len(rows) != 0 {
		t.Fatalf("disabled checks raised %+v", rows)
	}
}

func TestAlertSettingsRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	settings, errRead := store.alertSettings(ctx)
	if errRead != nil || settings != defaultAlertSettings() {
		t.Fatalf("defaults = %+v %v", settings, errRead)
	}
	settings.QuotaPct, settings.LunaWaste = 90, false
	if errSave := store.saveAlertSettings(ctx, settings); errSave != nil {
		t.Fatalf("save: %v", errSave)
	}
	if got, _ := store.alertSettings(ctx); got != settings {
		t.Fatalf("read back %+v", got)
	}
	settings.QuotaPct = 150
	if errSave := store.saveAlertSettings(ctx, settings); errSave == nil {
		t.Fatal("accepted quota_pct 150")
	}
}

// lunaServer records the alerts a fake Luna webhook receives; status decides its answer.
type lunaServer struct {
	server   *httptest.Server
	status   int
	received []lunaAlert
	auth     []string
}

func newLunaServer(t *testing.T) *lunaServer {
	t.Helper()
	fake := &lunaServer{status: http.StatusAccepted}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var alert lunaAlert
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &alert)
		fake.received = append(fake.received, alert)
		fake.auth = append(fake.auth, r.Header.Get("Authorization"))
		w.WriteHeader(fake.status)
	}))
	t.Cleanup(fake.server.Close)
	t.Setenv("INSIGHTS_LUNA_ALERTS_URL", fake.server.URL+"/v1/alerts/src-1")
	t.Setenv("INSIGHTS_LUNA_TOKEN", "source-token-value")
	return fake
}

func TestPostToLunaSendsGenericAlertAndHidesToken(t *testing.T) {
	fake := newLunaServer(t)
	t.Setenv("INSIGHTS_DASHBOARD_URL", "http://192.168.1.134:8317/insights")
	target, ok := lunaTarget()
	if !ok {
		t.Fatal("target not configured")
	}
	if errSend := postToLuna(context.Background(), target, lunaAlert{Title: "Claude weekly limit reached", Severity: severityCritical, Status: "firing", Fingerprint: "cliproxy:limit|c1"}); errSend != nil {
		t.Fatalf("send: %v", errSend)
	}
	if len(fake.received) != 1 || fake.auth[0] != "Bearer source-token-value" || fake.received[0].URL != "http://192.168.1.134:8317/insights" ||
		fake.received[0].Severity != "critical" || fake.received[0].Status != "firing" {
		t.Fatalf("received %+v auth %v", fake.received, fake.auth)
	}
	for _, status := range []int{http.StatusUnauthorized, http.StatusNotFound, http.StatusTooManyRequests} {
		fake.status = status
		errSend := postToLuna(context.Background(), target, lunaAlert{Title: "x", Status: "firing", Fingerprint: "f"})
		if errSend == nil || strings.Contains(errSend.Error(), "source-token-value") {
			t.Fatalf("status %d: %v", status, errSend)
		}
	}
	fake.server.Close()
	if errSend := postToLuna(context.Background(), target, lunaAlert{Title: "x", Status: "firing", Fingerprint: "f"}); errSend == nil || strings.Contains(errSend.Error(), "source-token-value") {
		t.Fatalf("network error = %v", errSend)
	}
	if errSend := postToLuna(context.Background(), lunaEndpoint{URL: "ftp://example.com/x", Token: "t"}, lunaAlert{}); errSend == nil {
		t.Fatal("accepted a non-http URL")
	}
	t.Setenv("INSIGHTS_LUNA_TOKEN", "")
	if status := lunaStatus(); status.Configured || status.Missing != "INSIGHTS_LUNA_TOKEN" {
		t.Fatalf("half configured = %+v", status)
	}
}

func TestLunaDeliveryResolutionAndRetry(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	svc := &Service{store: store}
	fake := newLunaServer(t)
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	reset := now.Add(2 * time.Hour)
	warning := alertRow{TS: now, DedupeKey: "quota|c1|5h|r1|80", Fingerprint: "limit|c1|5h|r1", ResolvesAt: reset, Kind: alertQuota,
		Severity: severityWarning, AuthID: "c1", Title: "Claude 5-hour limit at 85%", Body: "b"}
	if _, errInsert := store.insertAlert(ctx, warning, pushStatusPending); errInsert != nil {
		t.Fatalf("insert: %v", errInsert)
	}

	// Luna is down: the alert stays queued and is retried on the next check.
	fake.status = http.StatusServiceUnavailable
	svc.deliverToLuna(ctx, now)
	fake.status = http.StatusAccepted
	svc.deliverToLuna(ctx, now.Add(time.Minute))
	if len(fake.received) != 2 || fake.received[1].Fingerprint != "cliproxy:limit|c1|5h|r1" || fake.received[1].Severity != "warning" {
		t.Fatalf("received %+v", fake.received)
	}
	alerts, _ := store.alerts(ctx, 0, 10)
	if alerts[0].PushStatus != pushStatusSent || alerts[0].PushError != "" {
		t.Fatalf("after retry %+v", alerts[0])
	}

	// The window fills up: same fingerprint, so Luna escalates its alert.
	critical := warning
	critical.DedupeKey, critical.Kind, critical.Severity, critical.Title = "quota_exhausted|c1|5h|r1", alertQuotaExceeded, severityCritical, "Claude 5-hour limit reached"
	if _, errInsert := store.insertAlert(ctx, critical, pushStatusPending); errInsert != nil {
		t.Fatalf("insert: %v", errInsert)
	}
	svc.deliverToLuna(ctx, now.Add(2*time.Minute))
	if last := fake.received[len(fake.received)-1]; last.Fingerprint != "cliproxy:limit|c1|5h|r1" || last.Severity != "critical" {
		t.Fatalf("escalation %+v", last)
	}

	// Before the reset nothing resolves; after it one resolution goes out for both rows.
	if errResolve := store.resolveAlerts(ctx, reset.Add(-time.Minute)); errResolve != nil {
		t.Fatalf("resolve: %v", errResolve)
	}
	if open, _ := store.openAlertCount(ctx, reset.Add(-time.Minute)); open != 2 {
		t.Fatalf("open before reset = %d", open)
	}
	if errResolve := store.resolveAlerts(ctx, reset); errResolve != nil {
		t.Fatalf("resolve: %v", errResolve)
	}
	before := len(fake.received)
	svc.deliverToLuna(ctx, reset)
	svc.deliverToLuna(ctx, reset.Add(time.Minute))
	if len(fake.received) != before+1 || fake.received[before].Status != "resolved" || fake.received[before].Fingerprint != "cliproxy:limit|c1|5h|r1" {
		t.Fatalf("resolutions %+v", fake.received[before:])
	}
	if open, _ := store.openAlertCount(ctx, reset); open != 0 {
		t.Fatalf("open after reset = %d", open)
	}

	// An alert that resolves before it was delivered is never sent, and Luna is not told it resolved.
	quick := alertRow{TS: now, DedupeKey: "fast|1", Fingerprint: "fastmode|ip|tool", ResolvesAt: now.Add(time.Minute), Kind: alertFastMode, Severity: severityWarning, Title: "t", Body: "b"}
	if _, errInsert := store.insertAlert(ctx, quick, pushStatusPending); errInsert != nil {
		t.Fatalf("insert: %v", errInsert)
	}
	_ = store.resolveAlerts(ctx, now.Add(time.Hour))
	before = len(fake.received)
	svc.deliverToLuna(ctx, now.Add(time.Hour))
	if len(fake.received) != before {
		t.Fatalf("sent a resolved alert: %+v", fake.received[before:])
	}

	// A newer alert with the same fingerprint postpones the older one's resolution.
	first := alertRow{TS: now, DedupeKey: "fast|h1", Fingerprint: "fastmode|ip2|tool", ResolvesAt: now.Add(time.Hour), Kind: alertFastMode, Severity: severityWarning, Title: "t", Body: "b"}
	second := first
	second.DedupeKey, second.ResolvesAt = "fast|h2", now.Add(2*time.Hour)
	_, _ = store.insertAlert(ctx, first, pushStatusOff)
	_, _ = store.insertAlert(ctx, second, pushStatusOff)
	_ = store.resolveAlerts(ctx, now.Add(90*time.Minute))
	var unresolved int
	_ = store.db.QueryRow(`SELECT COUNT(*) FROM alerts WHERE fingerprint = 'fastmode|ip2|tool' AND resolved_ms IS NULL`).Scan(&unresolved)
	if unresolved != 2 {
		t.Fatalf("unresolved = %d, want both still open", unresolved)
	}

	// Gives up after a day of failures.
	fake.status = http.StatusInternalServerError
	late := alertRow{TS: now, DedupeKey: "signin|x", Fingerprint: "signin|claude|c9", Kind: alertUpstreamAuth, Severity: severityCritical, AuthID: "c9", Title: "t", Body: "b"}
	_, _ = store.insertAlert(ctx, late, pushStatusPending)
	svc.deliverToLuna(ctx, now.Add(lunaRetryFor+time.Minute))
	var status string
	_ = store.db.QueryRow(`SELECT push_status FROM alerts WHERE dedupe_key = 'signin|x'`).Scan(&status)
	if status != pushStatusFailed {
		t.Fatalf("expired status = %s", status)
	}

	// Not configured: queued alerts are marked off rather than retried.
	t.Setenv("INSIGHTS_LUNA_TOKEN", "")
	off := alertRow{TS: now.Add(lunaRetryFor), DedupeKey: "q|off", Kind: alertQuota, Severity: severityWarning, Title: "t", Body: "b"}
	_, _ = store.insertAlert(ctx, off, pushStatusPending)
	svc.deliverToLuna(ctx, now.Add(lunaRetryFor+2*time.Minute))
	_ = store.db.QueryRow(`SELECT push_status FROM alerts WHERE dedupe_key = 'q|off'`).Scan(&status)
	if status != pushStatusOff {
		t.Fatalf("unconfigured status = %s", status)
	}
}

func TestSignInAlertResolvesAfterSuccess(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	row := alertRow{TS: now, DedupeKey: "upstream_auth|claude|c2|1", Fingerprint: "signin|claude|c2", Kind: alertUpstreamAuth, Severity: severityCritical, AuthID: "c2", Title: "t", Body: "b"}
	if _, errInsert := store.insertAlert(ctx, row, pushStatusSent); errInsert != nil {
		t.Fatalf("insert: %v", errInsert)
	}
	if errInsert := store.insertRequests(ctx, []requestRow{{TS: now.Add(-time.Minute), Provider: "claude", AuthID: "c2"}}); errInsert != nil {
		t.Fatalf("insert request: %v", errInsert)
	}
	_ = store.resolveAlerts(ctx, now.Add(time.Minute))
	if open, _ := store.openAlertCount(ctx, now); open != 1 {
		t.Fatalf("a success before the alert resolved it")
	}
	_ = store.insertRequests(ctx, []requestRow{{TS: now.Add(2 * time.Minute), Provider: "claude", AuthID: "c2", Failed: true, StatusCode: 401}})
	_ = store.resolveAlerts(ctx, now.Add(3*time.Minute))
	if open, _ := store.openAlertCount(ctx, now); open != 1 {
		t.Fatalf("a failure resolved it")
	}
	_ = store.insertRequests(ctx, []requestRow{{TS: now.Add(4 * time.Minute), Provider: "claude", AuthID: "c2"}})
	_ = store.resolveAlerts(ctx, now.Add(5*time.Minute))
	var resolveStatus string
	_ = store.db.QueryRow(`SELECT resolve_status FROM alerts`).Scan(&resolveStatus)
	if open, _ := store.openAlertCount(ctx, now); open != 0 || resolveStatus != pushStatusPending {
		t.Fatalf("open=%d resolve_status=%s", open, resolveStatus)
	}
}

func TestOverview(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	t.Setenv("INSIGHTS_HOST_LABELS", "192.168.1.170=ProArt,192.168.1.171=Deathstar,127.0.0.1=R2-D2")
	now := time.Now().UTC().Truncate(time.Second)
	base := localDayStart(now).Add(time.Minute)
	if now.Sub(base) < 4*time.Hour {
		// Keep every seeded request inside today and before now.
		base = now.Add(-4 * time.Hour)
		if base.Before(localDayStart(now)) {
			t.Skip("too close to local midnight to seed a whole day")
		}
	}
	seedAnalysis(t, store, base)

	manager := coreauth.NewManager(nil, nil, nil)
	SetAuthManager(manager)
	observed := now.Add(-time.Minute)
	resets := now.Add(10 * time.Hour)
	auth := &coreauth.Auth{
		ID: "c1", Provider: "claude", Metadata: map[string]any{"email": "a@example.com"},
		Quota: coreauth.QuotaState{ObservedAt: observed, Signals: map[string]string{
			"Anthropic-Ratelimit-Unified-7d-Utilization": "0.70",
			"Anthropic-Ratelimit-Unified-7d-Reset":       strconv.FormatInt(resets.Unix(), 10),
		}},
	}
	if _, errRegister := manager.Register(ctx, auth); errRegister != nil {
		t.Fatalf("register: %v", errRegister)
	}
	// Six hours ago the window was at 40%: 5%/h reaches 100% in 6h, before the reset in 10h.
	if errInsert := store.insertQuotaSamples(ctx, []quotaRow{{TS: observed.Add(-7 * time.Hour), AuthID: "c1", Provider: "claude", WindowKey: "7d", UsedPct: 40, ResetsAt: resets, WindowMinutes: 10080}}); errInsert != nil {
		t.Fatalf("insert quota: %v", errInsert)
	}
	price := 100.0
	if _, errSet := store.setMonthlyPrice(ctx, "c1", &price); errSet != nil {
		t.Fatalf("price: %v", errSet)
	}
	svc := &Service{store: store}
	overview, errOverview := svc.overview(ctx, now)
	if errOverview != nil {
		t.Fatalf("overview: %v", errOverview)
	}
	if len(overview.Accounts) != 1 || overview.Accounts[0].Label != "a@example.com" || overview.Accounts[0].State != "available" ||
		overview.Accounts[0].MonthlyUSD == nil || *overview.Accounts[0].MonthlyUSD != 100 {
		t.Fatalf("accounts = %+v", overview.Accounts)
	}
	window := overview.Accounts[0].Windows[0]
	if window.UsedPct != 70 || window.ResetsAt != resets.UnixMilli() || window.RunsOutAt == 0 || !near(window.BurnPerHour, 5) {
		t.Fatalf("window = %+v", window)
	}
	providers := map[string]OverviewProvider{}
	for _, p := range overview.Providers {
		providers[p.Provider] = p
	}
	// c2 has traffic but is not on the hub and has no price, so Claude's plan total is unknown.
	if claude := providers["claude"]; claude.WeekCostUSD <= 0 || claude.MonthPaceUSD <= 0 || claude.PlanUSD != nil || claude.Accounts != 1 {
		t.Fatalf("claude = %+v", claude)
	}
	if overview.Today.Requests != 9 || overview.Today.Failed != 2 || overview.Today.CostUSD <= 0 {
		t.Fatalf("today = %+v", overview.Today)
	}
	if len(overview.TopThreads) != 3 || overview.TopThreads[0].SessionID != "A" || overview.TopThreads[0].Machine != "ProArt" || !overview.TopThreads[0].HarnessWarning {
		t.Fatalf("threads = %+v", overview.TopThreads)
	}
	if w := overview.Waste; w.FirstWrites != 1 || w.IdleRewrites != 1 || w.FastModeRefusals != 1 || w.Moves != 1 || w.FirstWriteUSD <= 0 {
		t.Fatalf("waste = %+v", w)
	}
}

func TestQuotaListsRetiredWindows(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	store := openTestStore(t)
	svc := &Service{store: store}
	previous := current.Swap(svc)
	defer current.Store(previous)
	manager := coreauth.NewManager(nil, nil, nil)
	SetAuthManager(manager)

	now := time.Now().UTC().Truncate(time.Second)
	gone := now.Add(-3 * 24 * time.Hour)
	weekly := now.Add(2 * 24 * time.Hour)
	auth := &coreauth.Auth{ID: "c1", Provider: "claude", Quota: coreauth.QuotaState{ObservedAt: now.Add(-time.Minute), Signals: map[string]string{
		"Anthropic-Ratelimit-Unified-7d-Utilization": "0.74",
		"Anthropic-Ratelimit-Unified-7d-Reset":       strconv.FormatInt(weekly.Unix(), 10),
	}}}
	if _, errRegister := manager.Register(ctx, auth); errRegister != nil {
		t.Fatalf("register: %v", errRegister)
	}
	if errInsert := store.insertQuotaSamples(ctx, []quotaRow{
		{TS: gone.Add(-time.Hour), AuthID: "c1", Provider: "claude", WindowKey: "5h", UsedPct: 30, ResetsAt: gone.Add(time.Hour), WindowMinutes: 300},
		{TS: gone, AuthID: "c1", Provider: "claude", WindowKey: "5h", UsedPct: 35, ResetsAt: gone.Add(time.Hour), WindowMinutes: 300},
		{TS: now.Add(-time.Minute), AuthID: "c1", Provider: "claude", WindowKey: "7d", UsedPct: 74, ResetsAt: weekly, WindowMinutes: 10080},
	}); errInsert != nil {
		t.Fatalf("insert quota: %v", errInsert)
	}

	engine := gin.New()
	RegisterRoutes(engine.Group("/v8/management"))
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v8/management/insights/quota", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("quota = %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Accounts []accountView `json:"accounts"`
	}
	if errDecode := json.Unmarshal(rec.Body.Bytes(), &body); errDecode != nil {
		t.Fatalf("decode: %v", errDecode)
	}
	if len(body.Accounts) != 1 {
		t.Fatalf("accounts = %+v", body.Accounts)
	}
	account := body.Accounts[0]
	if len(account.Windows) != 1 || account.Windows[0].Key != "7d" {
		t.Fatalf("windows = %+v", account.Windows)
	}
	if len(account.RetiredWindows) != 1 || account.RetiredWindows[0].Key != "5h" || account.RetiredWindows[0].Label != "5-hour" ||
		account.RetiredWindows[0].LastSeen != gone.UnixMilli() {
		t.Fatalf("retired = %+v", account.RetiredWindows)
	}
}

func TestBackupAndPrune(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	if errInsert := store.insertRequests(ctx, []requestRow{{TS: time.Now(), Provider: "claude", TotalTokens: 1}}); errInsert != nil {
		t.Fatalf("insert: %v", errInsert)
	}
	dir := filepath.Join(t.TempDir(), "backups")
	now := time.Date(2026, 10, 31, 3, 30, 0, 0, time.Local)
	first, errFirst := store.backupTo(ctx, dir, now)
	second, errSecond := store.backupTo(ctx, dir, now.Add(time.Minute))
	if errFirst != nil || errSecond != nil || first == second {
		t.Fatalf("backups %q %q: %v %v", first, second, errFirst, errSecond)
	}
	if filepath.Base(first) != "insights-20261031.db" {
		t.Fatalf("first backup = %s", first)
	}
	copyDB, errOpen := sql.Open("sqlite", "file:"+filepath.ToSlash(first)+"?mode=ro")
	if errOpen != nil {
		t.Fatalf("open backup: %v", errOpen)
	}
	var count int
	errCount := copyDB.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&count)
	_ = copyDB.Close()
	if errCount != nil || count != 1 {
		t.Fatalf("backup rows = %d %v", count, errCount)
	}
	if errWrite := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("keep"), 0o600); errWrite != nil {
		t.Fatalf("write: %v", errWrite)
	}
	if errPrune := pruneBackups(dir, 1); errPrune != nil {
		t.Fatalf("prune: %v", errPrune)
	}
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if len(names) != 2 || names[0] != filepath.Base(second) || names[1] != "notes.txt" {
		t.Fatalf("after prune = %v", names)
	}
}

func TestBackupDue(t *testing.T) {
	day := func(d, h, m int) time.Time { return time.Date(2026, 10, d, h, m, 0, 0, time.Local) }
	cases := []struct {
		last, now time.Time
		want      bool
	}{
		{time.Time{}, day(31, 15, 0), true},
		{day(31, 3, 30), day(31, 10, 0), false},
		{day(31, 3, 30), day(32, 2, 59), false},
		{day(31, 3, 30), day(32, 3, 1), true},
		{day(30, 4, 0), day(31, 2, 0), false},
	}
	for _, tc := range cases {
		if got := backupDue(tc.last, tc.now, 3); got != tc.want {
			t.Errorf("backupDue(%v, %v) = %v, want %v", tc.last, tc.now, got, tc.want)
		}
	}
}

func TestCheckReadOnlySQL(t *testing.T) {
	accepted := map[string]string{
		"SELECT 1;":               "SELECT 1",
		"  select 1 ;; ":          "select 1",
		"SELECT ';' AS semicolon": "SELECT ';' AS semicolon",
		"-- totals; per day\nWITH x AS (SELECT 1) SELECT * FROM x": "-- totals; per day\nWITH x AS (SELECT 1) SELECT * FROM x",
		"/* ; */ VALUES (1)": "/* ; */ VALUES (1)",
	}
	for query, want := range accepted {
		if got, errCheck := checkReadOnlySQL(query); errCheck != nil || got != want {
			t.Errorf("checkReadOnlySQL(%q) = %q, %v", query, got, errCheck)
		}
	}
	for _, query := range []string{"", ";", "SELECT 1; DROP TABLE requests", "DELETE FROM requests", "PRAGMA user_version",
		"ATTACH 'x.db' AS x", "SELECT 'unterminated", "/* open", "UPDATE meta SET value = 1"} {
		if _, errCheck := checkReadOnlySQL(query); errCheck == nil {
			t.Errorf("checkReadOnlySQL(%q) accepted", query)
		}
	}
}

func TestRunSQLIsReadOnlyAndCapped(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	if errInsert := store.insertRequests(ctx, []requestRow{{TS: time.Now(), Provider: "claude", Model: "m", TotalTokens: 5}}); errInsert != nil {
		t.Fatalf("insert: %v", errInsert)
	}
	result, errRun := store.runSQL(ctx, "SELECT provider, total_tokens, NULL AS empty_value FROM requests")
	if errRun != nil || len(result.Rows) != 1 || result.Columns[0] != "provider" || result.Rows[0][0] != "claude" || result.Rows[0][1] != int64(5) || result.Rows[0][2] != nil {
		t.Fatalf("result = %+v err=%v", result, errRun)
	}
	if _, errWrite := store.runSQL(ctx, "WITH x AS (SELECT 1) INSERT INTO meta (key, value) SELECT 'k', 'v' FROM x"); errWrite == nil {
		t.Fatal("a WITH ... INSERT statement ran")
	}
	if value, _ := store.metaValue(ctx, "k"); value != "" {
		t.Fatal("the console wrote to the database")
	}
	many, errMany := store.runSQL(ctx, "WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c WHERE x < 1500) SELECT x FROM c")
	if errMany != nil || len(many.Rows) != sqlMaxRows || !many.Truncated {
		t.Fatalf("rows=%d truncated=%v err=%v", len(many.Rows), many != nil && many.Truncated, errMany)
	}
	schema, errSchema := store.schemaInfo(ctx)
	if errSchema != nil || len(schema) < 5 {
		t.Fatalf("schema = %+v err=%v", schema, errSchema)
	}
}

func TestViewerKeyCannotChangeSettingsOrRunSQL(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := openTestStore(t)
	svc := &Service{store: store, recorder: newRecorder(store)}
	previous := current.Swap(svc)
	defer current.Store(previous)
	t.Setenv("INSIGHTS_BACKUP_DIR", filepath.Join(t.TempDir(), "backups"))
	t.Setenv("INSIGHTS_LUNA_ALERTS_URL", "")
	t.Setenv("INSIGHTS_LUNA_TOKEN", "")

	route := func(viewer bool) *gin.Engine {
		engine := gin.New()
		group := engine.Group("/v8/management")
		group.Use(func(c *gin.Context) { c.Set(ViewerAccessContextKey, viewer) })
		RegisterRoutes(group)
		return engine
	}
	do := func(engine *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/v8/management/insights/"+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		engine.ServeHTTP(rec, req)
		return rec
	}

	viewer := route(true)
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPut, "accounts/price", `{"auth_id":"c1","monthly_usd":200}`},
		{http.MethodPut, "alerts/settings", `{"quota_pct":50}`},
		{http.MethodPost, "alerts/ack", `{"all":true}`},
		{http.MethodPost, "alerts/test", `{}`},
		{http.MethodPost, "backup", `{}`},
		{http.MethodGet, "sql/schema", ""},
		{http.MethodPost, "sql", `{"query":"SELECT 1"}`},
	} {
		if rec := do(viewer, tc.method, tc.path, tc.body); rec.Code != http.StatusForbidden {
			t.Errorf("viewer %s %s = %d, want 403", tc.method, tc.path, rec.Code)
		}
	}
	for _, path := range []string{"threads", "waste", "accounts", "health", "alerts", "overview", "thread?session=A"} {
		if rec := do(viewer, http.MethodGet, path, ""); rec.Code != http.StatusOK {
			t.Errorf("viewer GET %s = %d %s", path, rec.Code, rec.Body.String())
		}
	}
	rec := do(viewer, http.MethodGet, "status", "")
	if strings.Contains(rec.Body.String(), "backups") {
		t.Errorf("viewer status reveals the backup directory: %s", rec.Body.String())
	}

	manager := route(false)
	if rec := do(manager, http.MethodPost, "sql", `{"query":"SELECT 1 AS one"}`); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"one"`) {
		t.Errorf("manager sql = %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(manager, http.MethodPost, "sql", `{"query":"DELETE FROM requests"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("manager delete = %d", rec.Code)
	}
	if rec := do(manager, http.MethodPut, "alerts/settings", `{"quota_pct":150}`); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid settings = %d", rec.Code)
	}
	if rec := do(manager, http.MethodPut, "alerts/settings", `{"quota_pct":70,"projection_min_pct":40,"fast_mode_refusals":3,"waste_alerts":true,"luna_limits":true}`); rec.Code != http.StatusOK {
		t.Errorf("valid settings = %d %s", rec.Code, rec.Body.String())
	}
	if settings, _ := store.alertSettings(context.Background()); settings.QuotaPct != 70 || settings.FastModeRefusals != 3 {
		t.Errorf("settings not saved: %+v", settings)
	}
	if rec := do(manager, http.MethodPut, "accounts/price", `{"auth_id":"missing","monthly_usd":20}`); rec.Code != http.StatusNotFound {
		t.Errorf("unknown account = %d", rec.Code)
	}
	if rec := do(manager, http.MethodPut, "accounts/price", `{"auth_id":"c1","monthly_usd":-1}`); rec.Code != http.StatusBadRequest {
		t.Errorf("negative price = %d", rec.Code)
	}
	if rec := do(manager, http.MethodPost, "backup", `{}`); rec.Code != http.StatusOK {
		t.Errorf("backup = %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(manager, http.MethodPost, "alerts/test", `{}`); rec.Code != http.StatusBadRequest {
		t.Errorf("test push without a target = %d", rec.Code)
	}
}
