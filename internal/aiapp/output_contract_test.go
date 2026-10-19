package aiapp

import (
	"testing"
	"time"
)

func TestSourceOutputContractAllowsComplementaryFieldsWithoutWeakeningGoal(t *testing.T) {
	spec := TaskSpec{Goal: "release facts", SchemaVersion: "v1", RequiredFields: []FieldRequirement{{"name", "string"}, {"version", "string"}, {"license", "string"}}, JoinKeys: []string{"name"}}
	for _, fields := range [][]string{{"name", "version"}, {"name", "license"}} {
		sub, err := OutputContract(spec, fields)
		if err != nil {
			t.Fatal(err)
		}
		if len(sub.RequiredFields) != 2 {
			t.Fatal("wrong subset")
		}
	}
	if len(spec.RequiredFields) != 3 {
		t.Fatal("global requirement weakened")
	}
	if _, err := OutputContract(spec, []string{"license"}); err == nil {
		t.Fatal("source lacks join key")
	}
	sub, _ := OutputContract(spec, []string{"name", "version"})
	row := Record{Key: "x", SchemaVersion: "v1", Fields: map[string]any{"name": "A", "version": 42}, Provenance: Provenance{SourceID: "s", URL: "https://example.com", FetchedAt: time.Now(), DataVersion: "v", TaskID: 1}}
	facts, err := EvaluateRecords(sub, []Record{row}, time.Now())
	if err != nil || len(facts.Issues) == 0 {
		t.Fatal("wrong type allowed by source subset")
	}
}
