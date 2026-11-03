package insights

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// The overview is the compact summary Luna's AI subscriptions screen and voice tool read: how much of
// each account's quota is left and when it runs out, what the subscriptions are worth at API prices,
// today's costliest threads and today's waste. Times are Unix milliseconds; "today" starts at the hub's
// local midnight.

const overviewTopThreads = 3

// Overview is the response of GET /insights/overview.
type Overview struct {
	GeneratedAt int64              `json:"generated_at"`
	DayStart    int64              `json:"day_start"`
	Accounts    []OverviewAccount  `json:"accounts"`
	Providers   []OverviewProvider `json:"providers"`
	Today       OverviewTotals     `json:"today"`
	TopThreads  []OverviewThread   `json:"top_threads"`
	Waste       OverviewWaste      `json:"waste"`
	OpenAlerts  int64              `json:"open_alerts"`
}

// OverviewAccount is one subscription account and its quota windows.
type OverviewAccount struct {
	AuthID     string   `json:"auth_id"`
	Provider   string   `json:"provider"`
	Label      string   `json:"label"`
	Plan       string   `json:"plan,omitempty"`
	MonthlyUSD *float64 `json:"monthly_usd"`
	// State is available, cooling (temporarily blocked until AvailableAt), unavailable or disabled.
	State       string           `json:"state"`
	AvailableAt int64            `json:"available_at,omitempty"`
	Windows     []OverviewWindow `json:"windows"`
}

// OverviewWindow is one quota window. RunsOutAt is set only when the recent burn rate reaches 100%
// before the window resets.
type OverviewWindow struct {
	Key           string  `json:"key"`
	Label         string  `json:"label"`
	UsedPct       float64 `json:"used_pct"`
	ResetsAt      int64   `json:"resets_at,omitempty"`
	WindowMinutes int64   `json:"window_minutes,omitempty"`
	ObservedAt    int64   `json:"observed_at,omitempty"`
	RunsOutAt     int64   `json:"runs_out_at,omitempty"`
	BurnPerHour   float64 `json:"burn_per_hour,omitempty"`
}

// OverviewProvider sums a provider's accounts: API value over the last 7 days, the monthly pace and the
// plan price (null unless every account has one).
type OverviewProvider struct {
	Provider     string   `json:"provider"`
	Accounts     int      `json:"accounts"`
	WeekRequests int64    `json:"week_requests"`
	WeekCostUSD  float64  `json:"week_cost_usd"`
	MonthPaceUSD float64  `json:"month_pace_usd"`
	PlanUSD      *float64 `json:"plan_usd"`
}

// OverviewTotals counts today's requests.
type OverviewTotals struct {
	Requests int64   `json:"requests"`
	Failed   int64   `json:"failed"`
	Tokens   int64   `json:"tokens"`
	CostUSD  float64 `json:"cost_usd"`
}

// OverviewThread is one of today's costliest threads.
type OverviewThread struct {
	SessionID      string  `json:"session_id"`
	Machine        string  `json:"machine"`
	Harness        string  `json:"harness"`
	HarnessWarning bool    `json:"harness_warning"`
	Models         string  `json:"models"`
	Requests       int64   `json:"requests"`
	CostUSD        float64 `json:"cost_usd"`
	LastMs         int64   `json:"last_ms"`
}

// OverviewWaste sums today's waste report.
type OverviewWaste struct {
	FirstWrites      int     `json:"first_writes"`
	FirstWriteUSD    float64 `json:"first_write_usd"`
	IdleRewrites     int     `json:"idle_rewrites"`
	IdleUSD          float64 `json:"idle_usd"`
	FastModeRefusals int64   `json:"fast_mode_refusals"`
	Moves            int     `json:"moves"`
	MovesUSD         float64 `json:"moves_usd"`
}

func localDayStart(now time.Time) time.Time {
	local := now.In(time.Local)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.Local)
}

func (s *Service) overview(ctx context.Context, now time.Time) (*Overview, error) {
	dayStart := localDayStart(now)
	out := &Overview{GeneratedAt: now.UnixMilli(), DayStart: dayStart.UnixMilli(), Accounts: []OverviewAccount{}, Providers: []OverviewProvider{}, TopThreads: []OverviewThread{}}

	week, errWeek := s.store.accountsReport(ctx, now.Add(-7*24*time.Hour), now, 24*time.Hour, 0, now)
	if errWeek != nil {
		return nil, errWeek
	}
	usage := make(map[string]AccountUsage, len(week.Accounts))
	for _, account := range week.Accounts {
		usage[account.AuthID] = account
	}

	if manager := authManager.Load(); manager != nil {
		for _, auth := range manager.List() {
			if auth == nil || auth.ID == "" || !coreauth.ProviderSupportsQuotaObservation(auth.Provider) {
				continue
			}
			out.Accounts = append(out.Accounts, s.overviewAccount(ctx, auth, usage[auth.ID], now))
		}
	}
	sort.SliceStable(out.Accounts, func(i, j int) bool {
		if out.Accounts[i].Provider != out.Accounts[j].Provider {
			return out.Accounts[i].Provider < out.Accounts[j].Provider
		}
		return out.Accounts[i].Label < out.Accounts[j].Label
	})

	providers := make(map[string]*OverviewProvider)
	unpriced := make(map[string]bool)
	add := func(provider string) *OverviewProvider {
		if p := providers[provider]; p != nil {
			return p
		}
		p := &OverviewProvider{Provider: provider}
		providers[provider] = p
		return p
	}
	counted := make(map[string]bool)
	for _, account := range out.Accounts {
		counted[account.AuthID] = true
		p := add(account.Provider)
		p.Accounts++
		if account.State == "disabled" {
			continue
		}
		if account.MonthlyUSD == nil {
			unpriced[account.Provider] = true
		} else {
			if p.PlanUSD == nil {
				p.PlanUSD = new(float64)
			}
			*p.PlanUSD += *account.MonthlyUSD
		}
	}
	for _, account := range week.Accounts {
		if account.Requests == 0 && account.Cost30dUSD == 0 {
			continue
		}
		p := add(account.Provider)
		if !counted[account.AuthID] {
			// Traffic from a credential that is no longer on the hub still counts toward the provider.
			unpriced[account.Provider] = unpriced[account.Provider] || account.MonthlyUSD == nil
		}
		p.WeekRequests += account.Requests
		p.WeekCostUSD += account.CostUSD
		if account.CoverageDays > 0 {
			p.MonthPaceUSD += account.Cost30dUSD / max(1, account.CoverageDays) * 30
		}
	}
	for _, p := range providers {
		if unpriced[p.Provider] {
			p.PlanUSD = nil
		}
		out.Providers = append(out.Providers, *p)
	}
	sort.Slice(out.Providers, func(i, j int) bool { return out.Providers[i].Provider < out.Providers[j].Provider })

	if errScan := s.store.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(failed),0), COALESCE(SUM(total_tokens),0), COALESCE(SUM(cost_usd),0)
		FROM requests WHERE ts_ms >= ? AND ts_ms < ?`, dayStart.UnixMilli(), now.UnixMilli()).Scan(
		&out.Today.Requests, &out.Today.Failed, &out.Today.Tokens, &out.Today.CostUSD); errScan != nil {
		return nil, fmt.Errorf("insights: today's totals: %w", errScan)
	}

	threads, errThreads := s.store.threads(ctx, dayStart, now, true, overviewTopThreads)
	if errThreads != nil {
		return nil, errThreads
	}
	labels := hostLabels(os.Getenv("INSIGHTS_HOST_LABELS"))
	for _, thread := range threads {
		if thread.CostUSD <= 0 {
			continue
		}
		out.TopThreads = append(out.TopThreads, OverviewThread{
			SessionID: thread.SessionID, Machine: firstNonEmpty(labels[thread.ClientIP], thread.ClientIP, "unknown"),
			Harness: thread.Harness, HarnessWarning: thread.HarnessWarning, Models: thread.Models,
			Requests: thread.Requests, CostUSD: thread.CostUSD, LastMs: thread.LastMs,
		})
	}

	waste, errWaste := s.store.waste(ctx, dayStart, now)
	if errWaste != nil {
		return nil, errWaste
	}
	out.Waste = summarizeWaste(waste)

	open, errOpen := s.store.openAlertCount(ctx, now)
	if errOpen != nil {
		return nil, errOpen
	}
	out.OpenAlerts = open
	return out, nil
}

func summarizeWaste(waste *WasteReport) OverviewWaste {
	out := OverviewWaste{FirstWrites: len(waste.FirstWrites), IdleRewrites: len(waste.IdleRewrites), Moves: len(waste.Moves)}
	for _, item := range waste.FirstWrites {
		out.FirstWriteUSD += item.CostUSD
	}
	for _, item := range waste.IdleRewrites {
		out.IdleUSD += item.ExtraUSD
	}
	for _, item := range waste.FastMode {
		out.FastModeRefusals += item.Refusals
	}
	for _, item := range waste.Moves {
		out.MovesUSD += item.ExtraUSD
	}
	return out
}

func (s *Service) overviewAccount(ctx context.Context, auth *coreauth.Auth, usage AccountUsage, now time.Time) OverviewAccount {
	view := liveAccountView(auth, now)
	account := OverviewAccount{
		AuthID: auth.ID, Provider: auth.Provider, Label: view.Label, Plan: view.Plan, MonthlyUSD: usage.MonthlyUSD,
		State: "available", Windows: []OverviewWindow{},
	}
	if account.MonthlyUSD == nil {
		if price, ok := defaultPlanUSD[strings.ToLower(view.Plan)]; ok {
			account.MonthlyUSD = &price
		}
	}
	switch {
	case auth.Disabled:
		account.State = "disabled"
	case view.NextRetryAt > now.UnixMilli():
		account.State, account.AvailableAt = "cooling", view.NextRetryAt
	case auth.Unavailable:
		account.State = "unavailable"
	}
	for _, w := range windowsForAuth(auth) {
		item := OverviewWindow{Key: w.Key, Label: w.Label, UsedPct: w.UsedPct, WindowMinutes: w.WindowMinutes,
			ResetsAt: unixMilliOrZero(w.ResetsAt), ObservedAt: unixMilliOrZero(w.ObservedAt)}
		if !w.ResetsAt.IsZero() && !w.ResetsAt.After(now) {
			// The window reset after this observation; the next period's reset is not known yet.
			item.UsedPct, item.ResetsAt = 0, 0
			account.Windows = append(account.Windows, item)
			continue
		}
		at := w.ObservedAt
		if at.IsZero() {
			at = now
		}
		prev, ok, errHistory := s.store.quotaUsedAt(ctx, auth.ID, w.Key, at.Add(-projectionLookback(w.WindowMinutes)))
		if errHistory != nil {
			log.Warnf("insights: %v", errHistory)
		}
		if p, projected := projectWindow(w, prev, ok && errHistory == nil, at); projected {
			item.RunsOutAt, item.BurnPerHour = p.HitAt.UnixMilli(), p.PerHour
		}
		account.Windows = append(account.Windows, item)
	}
	return account
}
