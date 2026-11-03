package models

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
)

// Live speed tiers.
//
// The template catalog can lag behind the speed tiers ChatGPT offers. Ultrafast for gpt-6.1-sol
// and gpt-6-astra, for example, appears only in ChatGPT's own /backend-api/codex/models catalog.
// Native Codex clients read that catalog directly; clients behind the proxy read this one instead,
// so the proxy copies each model's service_tiers and additional_speed_tiers from ChatGPT's catalog.

type liveSpeedTiers struct {
	serviceTiers         []any
	additionalSpeedTiers []any
}

var liveSpeedTiersBySlug atomic.Pointer[map[string]liveSpeedTiers]

// SetLiveSpeedTiers replaces the speed tiers taken from ChatGPT's Codex catalog. Each element of
// catalogs is one credential's catalog response. A model listed by several credentials keeps only
// the tiers every one of them offers, because a request can be routed to any of them. It returns
// the number of models with live tiers. Unparseable catalogs are skipped; when none parses, the
// previous tiers are kept and an error is returned.
func SetLiveSpeedTiers(catalogs [][]byte) (int, error) {
	parsed := make([]map[string]liveSpeedTiers, 0, len(catalogs))
	var lastErr error
	for _, catalog := range catalogs {
		tiers, errParse := parseLiveSpeedTiers(catalog)
		if errParse != nil {
			lastErr = errParse
			continue
		}
		parsed = append(parsed, tiers)
	}
	if len(parsed) == 0 {
		if lastErr == nil {
			lastErr = fmt.Errorf("no catalogs")
		}
		return 0, lastErr
	}
	merged := intersectLiveSpeedTiers(parsed)
	liveSpeedTiersBySlug.Store(&merged)
	return len(merged), nil
}

// ClearLiveSpeedTiers drops the live speed tiers so templates are used unchanged.
func ClearLiveSpeedTiers() {
	liveSpeedTiersBySlug.Store(nil)
}

func parseLiveSpeedTiers(catalog []byte) (map[string]liveSpeedTiers, error) {
	var payload struct {
		Models []map[string]any `json:"models"`
	}
	if errUnmarshal := json.Unmarshal(catalog, &payload); errUnmarshal != nil {
		return nil, fmt.Errorf("parse ChatGPT Codex catalog: %w", errUnmarshal)
	}
	if len(payload.Models) == 0 {
		return nil, fmt.Errorf("ChatGPT Codex catalog has no models")
	}
	tiers := make(map[string]liveSpeedTiers, len(payload.Models))
	for _, model := range payload.Models {
		slug := stringModelValue(model, "slug")
		serviceTiers, ok := model["service_tiers"].([]any)
		if slug == "" || !ok {
			continue
		}
		speedTiers, _ := model["additional_speed_tiers"].([]any)
		tiers[slug] = liveSpeedTiers{serviceTiers: serviceTiers, additionalSpeedTiers: speedTiers}
	}
	return tiers, nil
}

func intersectLiveSpeedTiers(catalogs []map[string]liveSpeedTiers) map[string]liveSpeedTiers {
	merged := make(map[string]liveSpeedTiers)
	for _, catalog := range catalogs {
		for slug, tiers := range catalog {
			current, seen := merged[slug]
			if !seen {
				merged[slug] = tiers
				continue
			}
			merged[slug] = liveSpeedTiers{
				serviceTiers:         keepCommonTiers(current.serviceTiers, tiers.serviceTiers),
				additionalSpeedTiers: keepCommonTiers(current.additionalSpeedTiers, tiers.additionalSpeedTiers),
			}
		}
	}
	return merged
}

// keepCommonTiers keeps the entries of left whose identity also appears in right. Service tiers
// are objects identified by "id"; additional speed tiers are plain strings.
func keepCommonTiers(left, right []any) []any {
	present := make(map[string]struct{}, len(right))
	for _, item := range right {
		present[speedTierID(item)] = struct{}{}
	}
	common := make([]any, 0, len(left))
	for _, item := range left {
		if _, ok := present[speedTierID(item)]; ok {
			common = append(common, item)
		}
	}
	return common
}

func speedTierID(item any) string {
	switch typed := item.(type) {
	case string:
		return strings.TrimSpace(typed)
	case map[string]any:
		return stringModelValue(typed, "id")
	}
	return ""
}

// applyCodexClientLiveSpeedTiers replaces the template's speed tiers with ChatGPT's for the first
// of ids that ChatGPT's catalog lists. Models ChatGPT does not list keep their template tiers.
func applyCodexClientLiveSpeedTiers(entry map[string]any, ids ...string) {
	overlay := liveSpeedTiersBySlug.Load()
	if overlay == nil {
		return
	}
	for _, id := range ids {
		tiers, ok := (*overlay)[strings.TrimSpace(id)]
		if !ok {
			continue
		}
		entry["service_tiers"] = cloneCodexClientModelValue(tiers.serviceTiers)
		speedTiers := tiers.additionalSpeedTiers
		if speedTiers == nil {
			speedTiers = []any{}
		}
		entry["additional_speed_tiers"] = cloneCodexClientModelValue(speedTiers)
		return
	}
}
