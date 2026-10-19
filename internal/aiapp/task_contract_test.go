package aiapp

import (
	"encoding/json"
	"testing"
	"time"
)

func TestResultTypesFreshnessAndEntityCoverage(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	spec := TaskSpec{Goal: "compare releases", SchemaVersion: "v1", EntityField: "name", Entities: []string{"A", "B"}, RequiredFields: []FieldRequirement{{Name: "name", Type: "string"}, {Name: "version", Type: "string"}, {Name: "downloads", Type: "integer"}}, MaxAgeSeconds: 3600}
	good := Record{Key: "a", SchemaVersion: "v1", Fields: map[string]any{"name": "A", "version": "1.0", "downloads": json.Number("2")}, Provenance: Provenance{SourceID: "s", URL: "https://example.com/a", FetchedAt: now.Add(-time.Minute), DataVersion: "r1", TaskID: 1}}
	stale := good
	stale.Key = "b"
	stale.Fields = map[string]any{"name": "B", "version": "2", "downloads": 3.0}
	stale.Provenance.FetchedAt = now.Add(-2 * time.Hour)
	report, err := EvaluateRecords(spec, []Record{good, stale}, now)
	if err != nil {
		t.Fatal(err)
	}
	if report.Satisfied || report.ValidRecords != 1 || len(report.MissingEntities) != 1 || report.MissingEntities[0] != "B" || len(report.Issues) != 1 {
		t.Fatalf("bad facts: %+v", report)
	}
	wrong := good
	wrong.Fields = map[string]any{"name": "A", "version": "1", "downloads": "2"}
	report, err = EvaluateRecords(spec, []Record{wrong}, now)
	if err != nil || report.ValidRecords != 0 || report.Satisfied {
		t.Fatalf("coerced bad type: %+v %v", report, err)
	}
}

func TestMergeKeepsConflictsAndEvidence(t *testing.T) {
	a := Record{Key: "a", Fields: map[string]any{"name": "A", "version": "1"}, Provenance: Provenance{SourceID: "s1"}}
	b := a
	b.Key = "b"
	b.Provenance.SourceID = "s2"
	c := b
	c.Key = "c"
	c.Fields = map[string]any{"name": "A", "version": "2"}
	got, err := MergeRecords([]Record{a, b, c}, []string{"name"})
	if err != nil || len(got.Records) != 2 || len(got.Records[0].Sources) != 2 || len(got.Conflicts) != 1 {
		t.Fatalf("lost evidence/conflict: %+v %v", got, err)
	}
	noKey, err := MergeRecords([]Record{a, b}, nil)
	if err != nil || len(noKey.Records) != 2 {
		t.Fatal("guessed duplicate without an explicit business key")
	}
}

func TestTaskContractRejectsImpossibleOrMutableRequirements(t *testing.T) {
	for _, spec := range []TaskSpec{{Goal: ""}, {Goal: "x", RequiredFields: []FieldRequirement{{Name: "x", Type: "magic"}}}, {Goal: "x", Entities: []string{"A"}}, {Goal: "x", RequiredFields: []FieldRequirement{{Name: "x", Type: "string"}, {Name: "x", Type: "number"}}}} {
		if err := spec.Validate(); err == nil {
			t.Fatalf("bad contract accepted: %+v", spec)
		}
	}
}
