package aiapp

import "testing"

func TestDeliveryCannotDropResearchOrComparisonRequirements(t *testing.T) {
	for _, description := range []string{"historical period with explicit missing years", "compare versions of two components"} {
		spec := TaskSpec{Requirements: []TaskRequirement{{ID: "scope", Description: description}, {ID: "definitions", Description: "separate measurement definitions"}}}
		first := RequirementResult{ID: "scope", Status: "satisfied", Explanation: "supported by acquired evidence", Citations: []string{"ev_1"}}
		second := RequirementResult{ID: "definitions", Status: "satisfied", Explanation: "definitions distinguished", Citations: []string{"ev_2"}}
		if err := validateRequirementDelivery(spec, "completed", []RequirementResult{first}, []string{"ev_1"}); err == nil {
			t.Fatal("dropped requirement accepted")
		}
		if err := validateRequirementDelivery(spec, "completed", []RequirementResult{first, second}, []string{"ev_1"}); err == nil {
			t.Fatal("unsupported requirement accepted")
		}
		if err := validateRequirementDelivery(spec, "completed", []RequirementResult{first, second}, []string{"ev_1", "ev_2"}); err != nil {
			t.Fatal(err)
		}
		second.Status = "missing"
		if err := validateRequirementDelivery(spec, "completed", []RequirementResult{first, second}, []string{"ev_1", "ev_2"}); err == nil {
			t.Fatal("missing requirement completed")
		}
		if err := validateRequirementDelivery(spec, "partial", []RequirementResult{first, second}, []string{"ev_1", "ev_2"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := validateRequirementDelivery(TaskSpec{}, "completed", nil, nil); err != nil {
		t.Fatal("legacy structured task changed", err)
	}
}
