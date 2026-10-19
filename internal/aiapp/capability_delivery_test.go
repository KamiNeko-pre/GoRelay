package aiapp

import (
	"encoding/json"
	"testing"
)

func TestRequestedCapabilityCannotDisappearBehindRecordSuccess(t *testing.T) {
	for _, tc := range []struct {
		required bool
		count    int
		valid    bool
	}{{true, 0, false}, {true, 1, true}, {false, 0, true}} {
		raw, _ := json.Marshal(map[string]any{"capability_required": tc.required, "reusable_capabilities": tc.count, "coverage": CoverageReport{Satisfied: true, ValidRecords: 3}})
		var result TaskResult
		_ = json.Unmarshal(raw, &result)
		if taskDeliverySatisfied(&result) != tc.valid {
			t.Fatal("capability requirement and data-only delivery were conflated", tc)
		}
	}
}
