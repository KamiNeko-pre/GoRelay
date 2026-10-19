package aiapp

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestCrawlerPlanFailureNamesActualArguments(t *testing.T) {
	for _, tc := range []struct {
		name   string
		spec   TaskSpec
		fields []string
	}{
		{"reported requirements only", TaskSpec{Requirements: []TaskRequirement{{ID: "selection", Description: "keep requested rows"}}}, []string{"requirements"}},
		{"sibling summary only", TaskSpec{RequireSummary: true}, []string{"require_summary"}},
		{"both causes", TaskSpec{RequireSummary: true, Requirements: []TaskRequirement{{ID: "join", Description: "join requested identity"}}}, []string{"require_summary", "requirements"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := crawlerPlanFailure(tc.spec)
			var f *ToolContractError
			if !errors.As(err, &f) {
				t.Fatalf("missing contract error: %v", err)
			}
			d := f.Failure.Details.(map[string]any)
			if !reflect.DeepEqual(d["fields"], tc.fields) || d["field"] != tc.fields[0] {
				t.Fatalf("wrong offending fields: %+v", d)
			}
			if f.Failure.Recovery == nil || !strings.Contains(f.Failure.Recovery.Instruction, "required_combinations") || !strings.Contains(f.Failure.Recovery.Instruction, "requirements omitted") {
				t.Fatal("missing supported repair steps")
			}
		})
	}
	if err := crawlerPlanFailure(TaskSpec{Goal: "collect records", MinRecords: 6, RequiredFields: []FieldRequirement{{Name: "id", Type: "string"}}}); err != nil {
		t.Fatalf("valid acquisition changed: %v", err)
	}
}
