package aiapp

import "testing"

func TestInterventionClassificationPreservesKnownAndUnknownBoundaries(t *testing.T) {
	for kind, want := range map[string]string{"approval": "L1", "source_hint": "L2", "manual_code": "L3", "manual_registration": "L3", "expanded_scope": "L3", "manual_recovery": "L3", "future_kind": "unknown"} {
		t.Run(kind, func(t *testing.T) {
			got := AssessInterventions(&RunAccounting{Interventions: []HumanIntervention{{Kind: kind}}})
			if got.Level != want || got.EventsByLevel[want] != 1 {
				t.Fatalf("kind %s classified as %+v", kind, got)
			}
		})
	}
	if got := AssessInterventions(nil); got.Level != "unknown" {
		t.Fatal("absent accounting means zero intervention", got)
	}
	if got := AssessInterventions(&RunAccounting{}); got.Level != "unknown" {
		t.Fatal("absent ledger means zero intervention", got)
	}
	if got := AssessInterventions(&RunAccounting{Interventions: []HumanIntervention{}}); got.Level != "L0" {
		t.Fatal("explicit empty ledger lost", got)
	}
	got := AssessInterventions(&RunAccounting{Interventions: []HumanIntervention{{Kind: "manual_code"}, {Kind: "future_kind"}, {Kind: "approval"}}, InterventionAssessment: InterventionAssessment{Level: "L0"}})
	if got.Level != "unknown" || got.EventsByLevel["L3"] != 1 || got.EventsByLevel["L1"] != 1 {
		t.Fatal("unknown intervention was lowered or trusted a cached claim", got)
	}
}
