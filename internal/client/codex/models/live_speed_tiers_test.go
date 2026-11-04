package models

import (
	"testing"
)

const liveTiersCatalogUltrafast = `{"models":[
	{"slug":"gpt-6.1-sol","service_tiers":[{"id":"priority","name":"Fast"},{"id":"ultrafast","name":"Ultrafast"}],"additional_speed_tiers":["fast","ultrafast"]},
	{"slug":"gpt-6-sol","service_tiers":[{"id":"priority","name":"Fast"}],"additional_speed_tiers":["fast"]}
]}`

const liveTiersCatalogFastOnly = `{"models":[
	{"slug":"gpt-6.1-sol","service_tiers":[{"id":"priority","name":"Fast"}],"additional_speed_tiers":["fast"]}
]}`

func tierIDs(t *testing.T, entry map[string]any) ([]string, []string) {
	t.Helper()
	var service, speed []string
	tiers, ok := entry["service_tiers"].([]any)
	if !ok {
		t.Fatalf("service_tiers = %#v, want an array", entry["service_tiers"])
	}
	for _, tier := range tiers {
		service = append(service, speedTierID(tier))
	}
	speedTiers, ok := entry["additional_speed_tiers"].([]any)
	if !ok {
		t.Fatalf("additional_speed_tiers = %#v, want an array", entry["additional_speed_tiers"])
	}
	for _, tier := range speedTiers {
		speed = append(speed, speedTierID(tier))
	}
	return service, speed
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestApplyCodexClientLiveSpeedTiers(t *testing.T) {
	t.Cleanup(ClearLiveSpeedTiers)

	template := func() map[string]any {
		return map[string]any{
			"service_tiers":          []any{map[string]any{"id": "priority"}},
			"additional_speed_tiers": []any{"fast"},
		}
	}

	ClearLiveSpeedTiers()
	entry := template()
	applyCodexClientLiveSpeedTiers(entry, "gpt-6.1-sol")
	if service, _ := tierIDs(t, entry); !equalStrings(service, []string{"priority"}) {
		t.Fatalf("without live tiers: service tiers = %v, want template", service)
	}

	if n, err := SetLiveSpeedTiers([][]byte{[]byte(liveTiersCatalogUltrafast)}); err != nil || n != 2 {
		t.Fatalf("SetLiveSpeedTiers = %d, %v; want 2, nil", n, err)
	}
	entry = template()
	applyCodexClientLiveSpeedTiers(entry, "gpt-6.1-sol")
	service, speed := tierIDs(t, entry)
	if !equalStrings(service, []string{"priority", "ultrafast"}) || !equalStrings(speed, []string{"fast", "ultrafast"}) {
		t.Fatalf("live tiers: service=%v speed=%v, want ultrafast added", service, speed)
	}

	// A model ChatGPT does not list keeps its template tiers.
	entry = template()
	applyCodexClientLiveSpeedTiers(entry, "claude-opus-5-5")
	if service, _ := tierIDs(t, entry); !equalStrings(service, []string{"priority"}) {
		t.Fatalf("unlisted model: service tiers = %v, want template", service)
	}

	// The entry gets its own copy: changing it does not change later entries.
	entry = template()
	applyCodexClientLiveSpeedTiers(entry, "gpt-6.1-sol")
	entry["service_tiers"].([]any)[0].(map[string]any)["id"] = "mutated"
	again := template()
	applyCodexClientLiveSpeedTiers(again, "gpt-6.1-sol")
	if service, _ := tierIDs(t, again); service[0] != "priority" {
		t.Fatalf("live tiers were shared between entries: %v", service)
	}
}

func TestSetLiveSpeedTiersKeepsTiersEveryCredentialOffers(t *testing.T) {
	t.Cleanup(ClearLiveSpeedTiers)
	if _, err := SetLiveSpeedTiers([][]byte{[]byte(liveTiersCatalogUltrafast), []byte(liveTiersCatalogFastOnly)}); err != nil {
		t.Fatalf("SetLiveSpeedTiers: %v", err)
	}
	entry := map[string]any{}
	applyCodexClientLiveSpeedTiers(entry, "gpt-6.1-sol")
	service, speed := tierIDs(t, entry)
	if !equalStrings(service, []string{"priority"}) || !equalStrings(speed, []string{"fast"}) {
		t.Fatalf("service=%v speed=%v, want only the tiers both credentials offer", service, speed)
	}
	// gpt-6-sol is listed by one credential only and keeps its tiers.
	entry = map[string]any{}
	applyCodexClientLiveSpeedTiers(entry, "gpt-6-sol")
	if service, _ := tierIDs(t, entry); !equalStrings(service, []string{"priority"}) {
		t.Fatalf("gpt-6-sol service tiers = %v, want [priority]", service)
	}
}

func TestSetLiveSpeedTiersKeepsPreviousTiersOnBadCatalog(t *testing.T) {
	t.Cleanup(ClearLiveSpeedTiers)
	if _, err := SetLiveSpeedTiers([][]byte{[]byte(liveTiersCatalogUltrafast)}); err != nil {
		t.Fatalf("SetLiveSpeedTiers: %v", err)
	}
	for _, bad := range []string{"not json", `{"models":[]}`, `<html>login</html>`} {
		if _, err := SetLiveSpeedTiers([][]byte{[]byte(bad)}); err == nil {
			t.Fatalf("SetLiveSpeedTiers(%q) succeeded, want an error", bad)
		}
	}
	entry := map[string]any{}
	applyCodexClientLiveSpeedTiers(entry, "gpt-6.1-sol")
	if service, _ := tierIDs(t, entry); !equalStrings(service, []string{"priority", "ultrafast"}) {
		t.Fatalf("service tiers = %v, want the previous live tiers", service)
	}
}

func TestBuildResponseUsesLiveSpeedTiersForCodexModels(t *testing.T) {
	t.Cleanup(ClearLiveSpeedTiers)
	if _, err := SetLiveSpeedTiers([][]byte{[]byte(liveTiersCatalogUltrafast)}); err != nil {
		t.Fatalf("SetLiveSpeedTiers: %v", err)
	}
	codexOnly := func(string) []string { return []string{"codex"} }
	mixed := func(string) []string { return []string{"codex", "openai-compatibility"} }

	for _, tc := range []struct {
		name      string
		providers ProvidersForModelFunc
		want      []string
	}{
		{name: "pure codex gets ultrafast", providers: codexOnly, want: []string{"priority", "ultrafast"}},
		{name: "mixed providers advertise no tiers", providers: mixed, want: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := BuildResponseForClient([]map[string]any{{"id": "gpt-6.1-sol"}}, tc.providers, false, "0.166.0")
			models, ok := resp["models"].([]map[string]any)
			if !ok || len(models) != 1 {
				t.Fatalf("models = %#v, want one model", resp["models"])
			}
			tiers, ok := models[0]["service_tiers"].([]any)
			if !ok {
				t.Fatalf("service_tiers = %#v, want an array", models[0]["service_tiers"])
			}
			var got []string
			for _, tier := range tiers {
				got = append(got, speedTierID(tier))
			}
			if !equalStrings(got, tc.want) {
				t.Fatalf("service_tiers = %v, want %v", got, tc.want)
			}
		})
	}
}
