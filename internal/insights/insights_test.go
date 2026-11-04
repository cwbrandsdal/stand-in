package insights

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestParseClaudeWindows(t *testing.T) {
	observed := time.Unix(1787290000, 0).UTC()
	windows := parseQuotaWindows("claude", map[string]string{
		"Anthropic-Ratelimit-Unified-5h-Utilization":      "0.23",
		"Anthropic-Ratelimit-Unified-5h-Reset":            "1787296800",
		"Anthropic-Ratelimit-Unified-5h-Status":           "allowed",
		"Anthropic-Ratelimit-Unified-7d-Utilization":      "0.69",
		"Anthropic-Ratelimit-Unified-7d-Reset":            "1787695200",
		"Anthropic-Ratelimit-Unified-7d_oi-Utilization":   "1.02",
		"Anthropic-Ratelimit-Unified-7d_oi-Status":        "rejected",
		"Anthropic-Ratelimit-Unified-Status":              "allowed",
		"Anthropic-Ratelimit-Unified-Reset":               "1787296800",
		"Anthropic-Ratelimit-Unified-Fallback-Percentage": "0.5",
	}, observed)
	if len(windows) != 3 {
		t.Fatalf("windows = %d, want 3: %+v", len(windows), windows)
	}
	want := []struct {
		key, label string
		used       float64
		reset      int64
		minutes    int64
	}{
		{"5h", "5-hour", 23, 1787296800, 300},
		{"7d", "Weekly", 69, 1787695200, 10080},
		{"7d_oi", "Weekly Fable", 102, 0, 10080},
	}
	for i, w := range want {
		got := windows[i]
		if got.Key != w.key || got.Label != w.label || got.UsedPct != w.used || got.WindowMinutes != w.minutes {
			t.Fatalf("window %d = %+v, want %+v", i, got, w)
		}
		if w.reset == 0 && !got.ResetsAt.IsZero() || w.reset != 0 && got.ResetsAt.Unix() != w.reset {
			t.Fatalf("window %s reset = %v, want %d", got.Key, got.ResetsAt, w.reset)
		}
		if !got.ObservedAt.Equal(observed) {
			t.Fatalf("window %s observed = %v", got.Key, got.ObservedAt)
		}
	}
	if windows[2].Status != "rejected" {
		t.Fatalf("7d_oi status = %q", windows[2].Status)
	}
}

func TestParseCodexWindows(t *testing.T) {
	observed := time.Unix(1787000000, 0).UTC()
	windows := parseQuotaWindows("codex", map[string]string{
		"X-Codex-Primary-Used-Percent":                 "81",
		"X-Codex-Primary-Window-Minutes":               "10080",
		"X-Codex-Primary-Reset-At":                     "1787588999",
		"X-Codex-Secondary-Used-Percent":               "12",
		"X-Codex-Secondary-Window-Minutes":             "300",
		"X-Codex-Secondary-Reset-After-Seconds":        "600",
		"X-Codex-Bengalfox-Primary-Used-Percent":       "40",
		"X-Codex-Bengalfox-Primary-Window-Minutes":     "10080",
		"X-Codex-Bengalfox-Limit-Name":                 "GPT-6-Codex-Spark",
		"X-Codex-Primary-Over-Secondary-Limit-Percent": "0",
		"X-Codex-Plan-Type":                            "pro",
	}, observed)
	byKey := make(map[string]QuotaWindow)
	for _, w := range windows {
		byKey[w.Key] = w
	}
	if len(byKey) != 3 {
		t.Fatalf("windows = %+v, want 3", windows)
	}
	if w := byKey["7d"]; w.UsedPct != 81 || w.ResetsAt.Unix() != 1787588999 || w.Label != "Weekly" {
		t.Fatalf("weekly = %+v", w)
	}
	if w := byKey["5h"]; w.UsedPct != 12 || !w.ResetsAt.Equal(observed.Add(600*time.Second)) {
		t.Fatalf("5h = %+v", w)
	}
	if w := byKey["bengalfox:7d"]; w.UsedPct != 40 || w.Label != "GPT-6-Codex-Spark weekly" {
		t.Fatalf("namespaced = %+v", w)
	}
	if windows[0].Key != "5h" || windows[1].Key != "7d" {
		t.Fatalf("order = %s, %s", windows[0].Key, windows[1].Key)
	}
}

func TestParseCodexWindowsSkipsPlaceholderSlot(t *testing.T) {
	windows := parseQuotaWindows("codex", map[string]string{
		"X-Codex-Primary-Used-Percent":          "1",
		"X-Codex-Primary-Window-Minutes":        "10080",
		"X-Codex-Secondary-Used-Percent":        "0",
		"X-Codex-Secondary-Reset-After-Seconds": "0",
	}, time.Now())
	if len(windows) != 1 || windows[0].Key != "7d" {
		t.Fatalf("windows = %+v, want only 7d", windows)
	}
}

func TestParseQuotaWindowsIgnoresOtherProviders(t *testing.T) {
	if got := parseQuotaWindows("gemini", map[string]string{"X-Codex-Primary-Used-Percent": "5"}, time.Now()); got != nil {
		t.Fatalf("got %+v, want nil", got)
	}
}

func TestWindowsForAuthPrefersNewestObservation(t *testing.T) {
	older := time.Unix(1000, 0).UTC()
	newer := time.Unix(2000, 0).UTC()
	auth := &coreauth.Auth{
		ID:       "a",
		Provider: "claude",
		Quota: coreauth.QuotaState{ObservedAt: older, Signals: map[string]string{
			"Anthropic-Ratelimit-Unified-7d-Utilization": "0.10",
		}},
		ModelStates: map[string]*coreauth.ModelState{
			"claude-fable-5-1": {Quota: coreauth.QuotaState{ObservedAt: newer, Signals: map[string]string{
				"Anthropic-Ratelimit-Unified-7d-Utilization":    "0.30",
				"Anthropic-Ratelimit-Unified-7d_oi-Utilization": "0.50",
			}}},
		},
	}
	windows := windowsForAuth(auth)
	if len(windows) != 2 || windows[0].Key != "7d" || windows[0].UsedPct != 30 || windows[1].UsedPct != 50 {
		t.Fatalf("windows = %+v", windows)
	}
}

func TestWindowsForAuthDropsRetiredWindows(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	stale := now.Add(-7 * time.Hour)
	auth := &coreauth.Auth{
		ID:       "a",
		Provider: "claude",
		Quota: coreauth.QuotaState{ObservedAt: now, Signals: map[string]string{
			"Anthropic-Ratelimit-Unified-7d-Utilization": "0.40",
			"Anthropic-Ratelimit-Unified-7d-Reset":       strconv.FormatInt(now.Add(48*time.Hour).Unix(), 10),
		}},
		ModelStates: map[string]*coreauth.ModelState{
			// Reported before the account lost its 5-hour window; that window has reset since.
			"claude-opus-5-5": {Quota: coreauth.QuotaState{ObservedAt: stale, Signals: map[string]string{
				"Anthropic-Ratelimit-Unified-5h-Utilization": "0.80",
				"Anthropic-Ratelimit-Unified-5h-Reset":       strconv.FormatInt(stale.Add(2*time.Hour).Unix(), 10),
			}}},
			// A model-specific window seen as long ago, whose period is still running, stays.
			"claude-fable-5-1": {Quota: coreauth.QuotaState{ObservedAt: stale, Signals: map[string]string{
				"Anthropic-Ratelimit-Unified-7d_oi-Utilization": "0.20",
				"Anthropic-Ratelimit-Unified-7d_oi-Reset":       strconv.FormatInt(now.Add(48*time.Hour).Unix(), 10),
			}}},
		},
	}
	windows := windowsForAuth(auth)
	if len(windows) != 2 || windows[0].Key != "7d" || windows[1].Key != "7d_oi" {
		t.Fatalf("windows = %+v, want 7d and 7d_oi", windows)
	}
	if windowRetired(stale, time.Time{}, stale.Add(retiredWindowGap)) {
		t.Fatal("a window is retired only after more than the gap")
	}
}

func TestAPIKeyIdentity(t *testing.T) {
	fp, label := apiKeyIdentity("cpa-proart-Ab3dEf6hIj9kLm2nOp5q")
	if label != "proart" || len(fp) != 8 {
		t.Fatalf("fp=%q label=%q", fp, label)
	}
	fp2, label2 := apiKeyIdentity("sk-something-else")
	if label2 != "" || fp2 == "" || fp2 == fp {
		t.Fatalf("fp=%q label=%q", fp2, label2)
	}
	if fp3, label3 := apiKeyIdentity(" "); fp3 != "" || label3 != "" {
		t.Fatalf("empty key produced %q/%q", fp3, label3)
	}
}

func TestClientKind(t *testing.T) {
	cases := map[string]string{
		"claude-cli/2.1.270 (external, sdk-ts)":             "claude-code",
		"codex_cli_rs/0.154.0 (Windows 10.0.26200; x86_64)": "codex",
		"curl/8.9.1":         "curl",
		"":                   "unknown",
		"SomeTool/1.0 extra": "sometool",
	}
	for ua, want := range cases {
		if got := clientKind(ua); got != want {
			t.Fatalf("clientKind(%q) = %q, want %q", ua, got, want)
		}
	}
}

func TestStoreSummarySessionsAndQuotaHistory(t *testing.T) {
	ctx := context.Background()
	store, errOpen := OpenStore(filepath.Join(t.TempDir(), "nested", "insights.db"))
	if errOpen != nil {
		t.Fatalf("open: %v", errOpen)
	}
	defer func() {
		if errClose := store.Close(); errClose != nil {
			t.Errorf("close: %v", errClose)
		}
	}()

	base := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	rows := []requestRow{
		{TS: base, Provider: "claude", Model: "claude-opus-5-5", AuthID: "c1", Account: "a@example.com", SessionID: "s1",
			ClientIP: "192.168.1.170", ClientKind: "claude-code", APIKeyLabel: "proart",
			InputTokens: 100, CacheReadTokens: 900, CacheWriteTokens: 50, OutputTokens: 40, TotalTokens: 1090, LatencyMs: 2000, TTFTMs: 500},
		{TS: base.Add(30 * time.Minute), Provider: "claude", Model: "claude-opus-5-5", AuthID: "c2", Account: "b@example.com", SessionID: "s1",
			ClientIP: "192.168.1.170", ClientKind: "claude-code", APIKeyLabel: "proart",
			InputTokens: 10, CacheReadTokens: 0, CacheWriteTokens: 1000, OutputTokens: 20, TotalTokens: 1030, LatencyMs: 4000, TTFTMs: 1500},
		{TS: base.Add(90 * time.Minute), Provider: "codex", Model: "gpt-6-astra", AuthID: "x1", Account: "c@example.com", SessionID: "s2",
			ClientIP: "192.168.1.171", ClientKind: "codex", APIKeyLabel: "deathstar",
			InputTokens: 5, OutputTokens: 5, TotalTokens: 10, Failed: true, StatusCode: 429},
	}
	if errInsert := store.insertRequests(ctx, rows); errInsert != nil {
		t.Fatalf("insert requests: %v", errInsert)
	}

	summary, errSummary := store.summary(ctx, base.Add(-time.Hour), base.Add(3*time.Hour), time.Hour, 0)
	if errSummary != nil {
		t.Fatalf("summary: %v", errSummary)
	}
	if summary.Totals.Requests != 3 || summary.Totals.Failed != 1 || summary.Totals.CacheReadTokens != 900 || summary.Totals.TotalTokens != 2130 {
		t.Fatalf("totals = %+v", summary.Totals)
	}
	if summary.Totals.AvgLatencyMs != 3000 || summary.Totals.AvgTTFTMs != 1000 {
		t.Fatalf("latency/ttft avg = %v/%v", summary.Totals.AvgLatencyMs, summary.Totals.AvgTTFTMs)
	}
	if len(summary.Series) != 2 || summary.Series[0].T != base.UnixMilli() || summary.Series[0].Requests != 2 {
		t.Fatalf("series = %+v", summary.Series)
	}
	if len(summary.ByProvider) != 2 || summary.ByProvider[0].Provider != "claude" {
		t.Fatalf("by provider = %+v", summary.ByProvider)
	}
	if len(summary.ByClient) != 2 || summary.ByClient[0].APIKeyLabel != "proart" || summary.ByClient[0].ClientKind != "claude-code" {
		t.Fatalf("by client = %+v", summary.ByClient)
	}
	if len(summary.ByAccount) != 3 || summary.ByAccount[0].Account != "a@example.com" {
		t.Fatalf("by account = %+v", summary.ByAccount)
	}
	if len(summary.Heatmap) != 2 {
		t.Fatalf("heatmap = %+v", summary.Heatmap)
	}

	// A local offset of +1h shifts hour buckets and heatmap hours.
	shifted, errShifted := store.summary(ctx, base.Add(-time.Hour), base.Add(3*time.Hour), 24*time.Hour, time.Hour)
	if errShifted != nil {
		t.Fatalf("shifted summary: %v", errShifted)
	}
	if len(shifted.Series) != 1 || shifted.Series[0].T != time.Date(2026, 10, 26, 23, 0, 0, 0, time.UTC).UnixMilli() {
		t.Fatalf("shifted series = %+v", shifted.Series)
	}

	sessions, errSessions := store.sessions(ctx, base.Add(-time.Hour), base.Add(3*time.Hour), 10)
	if errSessions != nil {
		t.Fatalf("sessions: %v", errSessions)
	}
	if sessions.Sessions != 1 || sessions.HoppedSessions != 1 || len(sessions.Rows) != 2 || sessions.Rows[1].Accounts != 2 {
		t.Fatalf("sessions = %+v", sessions)
	}

	samples := []quotaRow{
		{TS: base.Add(-2 * time.Hour), AuthID: "c1", Provider: "claude", WindowKey: "7d", UsedPct: 10, ResetsAt: base.Add(48 * time.Hour)},
		{TS: base.Add(-time.Hour), AuthID: "c1", Provider: "claude", WindowKey: "7d", UsedPct: 12, ResetsAt: base.Add(48 * time.Hour)},
		{TS: base.Add(time.Hour), AuthID: "c1", Provider: "claude", WindowKey: "7d", UsedPct: 20, ResetsAt: base.Add(48 * time.Hour)},
		{TS: base.Add(time.Hour), AuthID: "c1", Provider: "claude", WindowKey: "5h", UsedPct: 50},
	}
	if errInsert := store.insertQuotaSamples(ctx, samples); errInsert != nil {
		t.Fatalf("insert quota: %v", errInsert)
	}
	history, errHistory := store.quotaHistory(ctx, base, base.Add(2*time.Hour))
	if errHistory != nil {
		t.Fatalf("quota history: %v", errHistory)
	}
	weekly := history["c1"]["7d"]
	if len(weekly) != 2 || weekly[0][1] != 12 || weekly[1][1] != 20 || int64(weekly[1][2]) != base.Add(48*time.Hour).UnixMilli() {
		t.Fatalf("weekly history = %v", weekly)
	}
	if fiveHour := history["c1"]["5h"]; len(fiveHour) != 1 || fiveHour[0][2] != 0 {
		t.Fatalf("5h history = %v", fiveHour)
	}

	if errUpsert := store.upsertAccounts(ctx, []accountRow{{AuthID: "c1", Provider: "claude", Label: "a", Email: "a@example.com", SeenAt: base}}); errUpsert != nil {
		t.Fatalf("upsert: %v", errUpsert)
	}
	if errUpsert := store.upsertAccounts(ctx, []accountRow{{AuthID: "c1", Provider: "claude", Label: "a2", SeenAt: base.Add(time.Hour)}}); errUpsert != nil {
		t.Fatalf("upsert again: %v", errUpsert)
	}
	known, errKnown := store.knownAccounts(ctx)
	if errKnown != nil || len(known) != 1 || known[0].Label != "a2" || known[0].Email != "a@example.com" || !known[0].SeenAt.Equal(base.Add(time.Hour)) {
		t.Fatalf("known = %+v err=%v", known, errKnown)
	}

	recent, errRecent := store.recentRequests(ctx, 0, 2)
	if errRecent != nil || len(recent) != 2 || recent[0].Provider != "codex" || !recent[0].Failed {
		t.Fatalf("recent = %+v err=%v", recent, errRecent)
	}

	if errPrune := store.pruneBefore(ctx, base.Add(time.Hour)); errPrune != nil {
		t.Fatalf("prune: %v", errPrune)
	}
	remaining, errRemaining := store.recentRequests(ctx, 0, 10)
	if errRemaining != nil || len(remaining) != 1 {
		t.Fatalf("after prune = %+v err=%v", remaining, errRemaining)
	}
}

func TestSamplerStoresOnlyNewOrChangedObservations(t *testing.T) {
	ctx := context.Background()
	store, errOpen := OpenStore(filepath.Join(t.TempDir(), "insights.db"))
	if errOpen != nil {
		t.Fatalf("open: %v", errOpen)
	}
	defer func() { _ = store.Close() }()

	manager := coreauth.NewManager(nil, nil, nil)
	SetAuthManager(manager)
	observed := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	auth := &coreauth.Auth{
		ID:       "claude-1",
		Provider: "claude",
		Metadata: map[string]any{"email": "me@example.com"},
		Quota: coreauth.QuotaState{ObservedAt: observed, Signals: map[string]string{
			"Anthropic-Ratelimit-Unified-7d-Utilization": "0.40",
			"Anthropic-Ratelimit-Unified-7d-Reset":       "1787695200",
		}},
	}
	if _, errRegister := manager.Register(ctx, auth); errRegister != nil {
		t.Fatalf("register: %v", errRegister)
	}

	svc := &Service{store: store, last: make(map[string]storedSample)}
	svc.sampleQuotas(ctx, observed)
	svc.sampleQuotas(ctx, observed.Add(time.Minute)) // same ObservedAt: no new row
	if got := countQuotaSamples(t, store); got != 1 {
		t.Fatalf("samples after duplicate = %d, want 1", got)
	}
	if svc.accountLabel("claude-1") != "me@example.com" {
		t.Fatalf("label = %q", svc.accountLabel("claude-1"))
	}

	// A newer observation with the same value inside the heartbeat window is not stored.
	auth.Quota.ObservedAt = observed.Add(2 * time.Minute)
	if _, errUpdate := manager.Update(ctx, auth); errUpdate != nil {
		t.Fatalf("update: %v", errUpdate)
	}
	svc.sampleQuotas(ctx, observed.Add(2*time.Minute))
	if got := countQuotaSamples(t, store); got != 1 {
		t.Fatalf("samples after unchanged = %d, want 1", got)
	}

	// A changed value is stored immediately.
	auth.Quota.ObservedAt = observed.Add(3 * time.Minute)
	auth.Quota.Signals["Anthropic-Ratelimit-Unified-7d-Utilization"] = "0.45"
	if _, errUpdate := manager.Update(ctx, auth); errUpdate != nil {
		t.Fatalf("update: %v", errUpdate)
	}
	svc.sampleQuotas(ctx, observed.Add(3*time.Minute))
	if got := countQuotaSamples(t, store); got != 2 {
		t.Fatalf("samples after change = %d, want 2", got)
	}
}

func countQuotaSamples(t *testing.T, store *Store) int {
	t.Helper()
	var count int
	if errScan := store.db.QueryRow(`SELECT COUNT(*) FROM quota_samples`).Scan(&count); errScan != nil {
		t.Fatalf("count: %v", errScan)
	}
	return count
}
