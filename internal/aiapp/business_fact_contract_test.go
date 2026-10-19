package aiapp

import (
	"testing"
	"time"
)

// Synthetic contract qualification, not live pricing gold or Agent capability evidence.
func TestBusinessAtomicFactsPreserveConditionalPricesAndDistinctLimitTypes(t *testing.T) {
	now := time.Now().UTC()
	spec := TaskSpec{
		Goal: "preserve conditional prices and limits", SchemaVersion: "v1",
		JoinKeys:      []string{"product", "capability", "variant", "plan", "fact_kind", "fact_name"},
		MaxAgeSeconds: 3600,
	}
	for _, name := range []string{"product", "capability", "variant", "plan", "fact_kind", "fact_name", "value", "unit", "conditions", "scope"} {
		spec.RequiredFields = append(spec.RequiredFields, FieldRequirement{Name: name, Type: "string"})
	}
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	makeFact := func(key, plan, kind, name, value, unit, conditions string) Record {
		return Record{Key: key, SchemaVersion: "v1", Fields: map[string]any{
			"product": "Example", "capability": "search", "variant": "deep", "plan": plan,
			"fact_kind": kind, "fact_name": name, "value": value, "unit": unit,
			"conditions": conditions, "scope": "account",
		}, Provenance: Provenance{SourceID: "official", URL: "https://example.com/docs", FetchedAt: now, DataVersion: "fixture-v1", TaskID: 1}}
	}
	records := []Record{
		makeFact("p1", "payg", "price", "run", "0.012–1.00", "USD/run", "depends on selected options"),
		makeFact("p2", "subscription", "price", "run", "0.008", "USD/credit", "credit conversion is mode-dependent"),
		makeFact("l1", "subscription", "rate_limit", "requests", "60", "requests/minute", "per account"),
		makeFact("l2", "subscription", "period_quota", "requests", "1000", "requests/month", "per account"),
	}
	coverage, err := EvaluateRecords(spec, records, now)
	if err != nil || !coverage.Satisfied || coverage.ValidRecords != 4 {
		t.Fatalf("contract rejected facts: %+v %v", coverage, err)
	}
	merged, err := MergeRecords(records, spec.JoinKeys)
	if err != nil || len(merged.Records) != 4 || len(merged.Conflicts) != 0 {
		t.Fatalf("tiers or limit types collided: %+v %v", merged, err)
	}
	for _, r := range merged.Records {
		if r.Fields["conditions"] == "" || len(r.FieldSources["value"]) == 0 {
			t.Fatalf("lost condition or field source: %+v", r)
		}
	}
	// An alternative condition for the same identity is a conflict, never an overwrite.
	changed := makeFact("p3", "payg", "price", "run", "0.012–1.00", "USD/run", "different options")
	merged, err = MergeRecords(append(records, changed), spec.JoinKeys)
	if err != nil || len(merged.Conflicts) == 0 {
		t.Fatalf("different conditions silently merged: %+v %v", merged, err)
	}
	// Missing business conditions and stale evidence cannot satisfy delivery.
	for _, mode := range []string{"missing_condition", "stale"} {
		bad := makeFact("bad", "payg", "price", "run", "0.012–1.00", "USD/run", "depends on selected options")
		if mode == "missing_condition" {
			delete(bad.Fields, "conditions")
		} else {
			bad.Provenance.FetchedAt = now.Add(-2 * time.Hour)
		}
		got, e := EvaluateRecords(spec, []Record{bad}, now)
		if e != nil || got.Satisfied || len(got.Issues) == 0 {
			t.Fatalf("%s accepted: %+v %v", mode, got, e)
		}
	}
}
