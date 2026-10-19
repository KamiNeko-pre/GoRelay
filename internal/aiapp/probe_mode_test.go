package aiapp

import (
	"testing"
)

// Exploration is separated by mode and does not commit acquired facts.
func TestProbeAllowsFieldsAlsoRequestedByDelivery(t *testing.T) {
	spec := TaskSpec{
		Goal:            "collect notices",
		ContractVersion: "v2",
		SchemaVersion:   "v1",
		RequiredFields: []FieldRequirement{
			{Name: "title", Type: "string"},
			{Name: "published_date", Type: "string"},
		},
		JoinKeys: []string{"source_url"},
	}
	for _, claimed := range []string{"title", "published_date"} {
		err := probeFieldGuard(spec, []string{"href", claimed})
		if err != nil {
			t.Fatalf("valid exploration field %q rejected: %v", claimed, err)
		}
	}
	// Intermediate-only fields are exactly what a probe is for.
	if err := probeFieldGuard(spec, []string{"href", "text"}); err != nil {
		t.Fatalf("intermediate-only probe fields were rejected: %v", err)
	}
}
