package aiapp

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

// An approved task configuration, not evidence of autonomous live discovery.
func TestMonitorSteamOpportunityDefinitionUsesConfirmedCriteria(t *testing.T) {
	raw, err := os.ReadFile("../../inputs/steam-opportunities-monitor.json")
	if err != nil {
		t.Fatal(err)
	}
	var config CreateMonitorRequest
	if err = json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	if err = config.Normalize(); err != nil {
		t.Fatal(err)
	}
	if len(config.Spec.Entities) != 0 || config.IntervalSeconds != 86400 {
		t.Fatal("discovery became a fixed watchlist", config)
	}
	for _, tc := range []struct {
		price, discount int
		currency        string
		buyable, want   bool
	}{
		{5000, 80, "CNY", true, true}, {3000, 0, "CNY", true, true}, {3001, 79, "CNY", true, false},
		{0, 0, "CNY", true, true}, {2000, 90, "USD", true, false}, {2000, 90, "CNY", false, false},
	} {
		fields := map[string]any{"price_minor": tc.price, "discount_percent": tc.discount, "currency": tc.currency, "purchasable": tc.buyable}
		got, err := MatchMonitorEligibility(*config.Spec, config, "unseen-game", fields)
		if err != nil || got != tc.want {
			t.Fatal(tc, got, err)
		}
	}
	if ok, err := MatchMonitorEligibility(*config.Spec, config, "unknown", map[string]any{"price_minor": nil, "discount_percent": 90, "currency": "CNY", "purchasable": true}); err == nil || ok {
		t.Fatal("unknown price became free", ok, err)
	}
}

func TestMonitorOpenDiscoveryAcceptsNewIdentitiesButProtectsFixedScope(t *testing.T) {
	spec := monitorSpec()
	spec.Entities = nil
	now := time.Now().UTC()
	records := []Record{}
	for _, id := range []string{"first", "new-second"} {
		records = append(records, Record{Key: id, SchemaVersion: spec.SchemaVersion, Fields: map[string]any{"id": id, "amount": 2500, "unit": "CNY", "labels": []string{"zh"}}, Provenance: Provenance{SourceID: "public", URL: "https://example.com/data", FetchedAt: now, DataVersion: "v1", TaskID: 1}})
	}
	merged, err := MergeRecords(records, spec.JoinKeys)
	if err != nil {
		t.Fatal(err)
	}
	result := TaskResult{Records: merged.Records}
	got, issues, err := monitorValidatedRows(spec, result, now)
	if err != nil || len(got) != 2 || len(issues) != 0 {
		t.Fatal("dynamic discoveries rejected", got, issues, err)
	}
	spec.Entities = []string{"first"}
	got, issues, err = monitorValidatedRows(spec, result, now)
	if err != nil || len(got) != 1 || len(issues) != 1 {
		t.Fatal("fixed scope no longer enforced", got, issues, err)
	}
	spec.Entities = nil
	records[1].Fields["amount"] = nil
	merged, err = MergeRecords(records, spec.JoinKeys)
	if err != nil {
		t.Fatal(err)
	}
	got, issues, err = monitorValidatedRows(spec, TaskResult{Records: merged.Records}, now)
	if err != nil || len(got) != 1 || len(issues) != 1 {
		t.Fatal("new identity bypassed fact validation", got, issues, err)
	}
}
