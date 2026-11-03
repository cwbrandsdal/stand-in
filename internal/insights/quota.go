package insights

import (
	"sort"
	"strconv"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// QuotaWindow is one normalized quota window parsed from passive upstream signals.
type QuotaWindow struct {
	// Key identifies the window across accounts of the same provider (e.g. "5h", "7d", "7d_oi").
	Key string `json:"key"`
	// Label is a human readable window name.
	Label string `json:"label"`
	// UsedPct is the consumed share of the window in percent (0-100, may exceed 100).
	UsedPct float64 `json:"used_pct"`
	// ResetsAt is when the window resets; zero when unknown.
	ResetsAt time.Time `json:"-"`
	// WindowMinutes is the window length when reported.
	WindowMinutes int64 `json:"window_minutes,omitempty"`
	// Status is the provider-reported window status when available.
	Status string `json:"status,omitempty"`
	// ObservedAt is when the signal snapshot was observed.
	ObservedAt time.Time `json:"-"`
}

const (
	claudeSignalPrefix = "Anthropic-Ratelimit-Unified-"
	codexSignalPrefix  = "X-Codex-"
	// retiredWindowGap is how long a window may go unreported while the same account's other windows keep
	// arriving before it counts as gone. Claude stopped sending the 5-hour window for verified Ultra accounts
	// on 30 October 2026, for example; without this its last observation would linger as a current window.
	retiredWindowGap = 6 * time.Hour
)

// windowRetired reports whether a window last observed at lastSeen, resetting at resetsAt, is no longer
// reported, given the newest observation of any of the account's windows. A window whose period has not
// ended yet is kept: model-specific windows only arrive with requests for that model.
func windowRetired(lastSeen, resetsAt, newest time.Time) bool {
	if newest.Sub(lastSeen) <= retiredWindowGap {
		return false
	}
	return resetsAt.IsZero() || !resetsAt.After(newest)
}

// parseQuotaWindows converts one passive quota signal snapshot into normalized windows.
func parseQuotaWindows(provider string, signals map[string]string, observedAt time.Time) []QuotaWindow {
	if len(signals) == 0 {
		return nil
	}
	canonical := make(map[string]string, len(signals))
	for key, value := range signals {
		canonical[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
	}
	var windows []QuotaWindow
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude":
		windows = parseClaudeWindows(canonical, observedAt)
	case "codex":
		windows = parseCodexWindows(canonical, observedAt)
	default:
		return nil
	}
	sort.Slice(windows, func(i, j int) bool { return windowSortKey(windows[i].Key) < windowSortKey(windows[j].Key) })
	return windows
}

func parseClaudeWindows(signals map[string]string, observedAt time.Time) []QuotaWindow {
	prefix := strings.ToLower(claudeSignalPrefix)
	const suffix = "-utilization"
	var windows []QuotaWindow
	for key, value := range signals {
		rest := strings.TrimPrefix(key, prefix)
		if rest == key || !strings.HasSuffix(rest, suffix) {
			continue
		}
		windowID := strings.TrimSuffix(rest, suffix)
		if windowID == "" || strings.Contains(windowID, "-") {
			continue
		}
		utilization, errParse := strconv.ParseFloat(value, 64)
		if errParse != nil || utilization < 0 {
			continue
		}
		window := QuotaWindow{
			Key:        windowID,
			Label:      claudeWindowLabel(windowID),
			UsedPct:    roundPct(utilization * 100),
			Status:     signals[prefix+windowID+"-status"],
			ObservedAt: observedAt,
		}
		if resetsAt, ok := parseUnixSeconds(signals[prefix+windowID+"-reset"]); ok {
			window.ResetsAt = resetsAt
		}
		window.WindowMinutes = claudeWindowMinutes(windowID)
		windows = append(windows, window)
	}
	return windows
}

func parseCodexWindows(signals map[string]string, observedAt time.Time) []QuotaWindow {
	prefix := strings.ToLower(codexSignalPrefix)
	var windows []QuotaWindow
	for key, value := range signals {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		var slot string
		switch {
		case strings.HasSuffix(key, "primary-used-percent"):
			slot = "primary"
		case strings.HasSuffix(key, "secondary-used-percent"):
			slot = "secondary"
		default:
			continue
		}
		base := strings.TrimSuffix(key, slot+"-used-percent")
		namespace := strings.TrimSuffix(strings.TrimPrefix(base, prefix), "-")
		used, errParse := strconv.ParseFloat(value, 64)
		if errParse != nil || used < 0 {
			continue
		}
		minutes, _ := strconv.ParseInt(signals[base+slot+"-window-minutes"], 10, 64)
		if minutes <= 0 && used == 0 {
			// Plans with a single window still send the other slot's headers, as 0% with no window length.
			continue
		}
		window := QuotaWindow{
			UsedPct:       roundPct(used),
			WindowMinutes: minutes,
			ObservedAt:    observedAt,
		}
		if resetsAt, ok := parseUnixSeconds(signals[base+slot+"-reset-at"]); ok {
			window.ResetsAt = resetsAt
		} else if after, errAfter := strconv.ParseInt(signals[base+slot+"-reset-after-seconds"], 10, 64); errAfter == nil && after >= 0 && !observedAt.IsZero() {
			window.ResetsAt = observedAt.Add(time.Duration(after) * time.Second)
		}
		windowID := codexWindowID(slot, minutes)
		window.Key = windowID
		window.Label = durationLabel(windowID)
		if namespace != "" {
			window.Key = namespace + ":" + windowID
			name := signals[base+"limit-name"]
			if name == "" {
				name = namespace
			}
			window.Label = name + " " + strings.ToLower(window.Label)
		}
		windows = append(windows, window)
	}
	return windows
}

// windowsForAuth merges auth-level and model-level quota snapshots, keeping the newest observation per window
// and leaving out windows the provider no longer reports (see windowRetired).
func windowsForAuth(auth *coreauth.Auth) []QuotaWindow {
	if auth == nil {
		return nil
	}
	merged := make(map[string]QuotaWindow)
	add := func(quota coreauth.QuotaState) {
		for _, window := range parseQuotaWindows(auth.Provider, quota.Signals, quota.ObservedAt) {
			if existing, ok := merged[window.Key]; ok && !window.ObservedAt.After(existing.ObservedAt) {
				continue
			}
			merged[window.Key] = window
		}
	}
	add(auth.Quota)
	for _, state := range auth.ModelStates {
		if state != nil {
			add(state.Quota)
		}
	}
	var newest time.Time
	for _, window := range merged {
		if window.ObservedAt.After(newest) {
			newest = window.ObservedAt
		}
	}
	windows := make([]QuotaWindow, 0, len(merged))
	for _, window := range merged {
		if windowRetired(window.ObservedAt, window.ResetsAt, newest) {
			continue
		}
		windows = append(windows, window)
	}
	sort.Slice(windows, func(i, j int) bool { return windowSortKey(windows[i].Key) < windowSortKey(windows[j].Key) })
	return windows
}

func codexWindowID(slot string, minutes int64) string {
	switch minutes {
	case 300:
		return "5h"
	case 10080:
		return "7d"
	case 0:
		return slot
	}
	if minutes%1440 == 0 {
		return strconv.FormatInt(minutes/1440, 10) + "d"
	}
	if minutes%60 == 0 {
		return strconv.FormatInt(minutes/60, 10) + "h"
	}
	return strconv.FormatInt(minutes, 10) + "m"
}

func claudeWindowLabel(windowID string) string {
	base, model, hasModel := strings.Cut(windowID, "_")
	label := durationLabel(base)
	if !hasModel {
		return label
	}
	switch model {
	case "oi":
		return label + " Fable"
	case "opus":
		return label + " Opus"
	case "sonnet":
		return label + " Sonnet"
	}
	return label + " " + model
}

func claudeWindowMinutes(windowID string) int64 {
	base, _, _ := strings.Cut(windowID, "_")
	switch base {
	case "5h":
		return 300
	case "7d":
		return 10080
	}
	return 0
}

func durationLabel(windowID string) string {
	switch windowID {
	case "5h":
		return "5-hour"
	case "7d":
		return "Weekly"
	case "primary":
		return "Primary"
	case "secondary":
		return "Secondary"
	}
	return windowID
}

// windowSortKey orders short windows first, then weekly, then model-specific and namespaced windows.
func windowSortKey(key string) string {
	rank := "9"
	switch {
	case key == "5h":
		rank = "0"
	case key == "7d":
		rank = "1"
	case strings.HasPrefix(key, "7d_"):
		rank = "2"
	case !strings.Contains(key, ":"):
		rank = "3"
	}
	return rank + key
}

func parseUnixSeconds(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	seconds, errParse := strconv.ParseInt(raw, 10, 64)
	if errParse != nil || seconds <= 0 {
		return time.Time{}, false
	}
	return time.Unix(seconds, 0).UTC(), true
}

func roundPct(value float64) float64 {
	return float64(int64(value*100+0.5)) / 100
}
