package aiapp

import (
	"testing"
	"time"
)

func TestMergedCoverageDoesNotLaunderStaleField(t *testing.T) {
	now := time.Now().UTC()
	spec := TaskSpec{Goal: "A release and license", SchemaVersion: "v1", EntityField: "name", Entities: []string{"A"}, RequiredFields: []FieldRequirement{{Name: "name", Type: "string"}, {Name: "version", Type: "string"}, {Name: "license", Type: "string"}}, MaxAgeSeconds: 3600}
	p := Provenance{SourceID: "releases", URL: "https://example.com/releases", FetchedAt: now.Add(-time.Minute), TaskID: 1, DataVersion: "r1"}
	a := Record{Key: "a", SchemaVersion: "v1", Fields: map[string]any{"name": "A", "version": "1"}, Provenance: p}
	p.SourceID = "catalog"
	p.URL = "https://example.org/catalog"
	p.TaskID = 2
	b := Record{Key: "b", SchemaVersion: "v1", Fields: map[string]any{"name": "A", "license": "MIT"}, Provenance: p}
	joined, _ := MergeRecords([]Record{a, b}, []string{"name"})
	got, err := EvaluateMergedRecords(spec, joined, now)
	if err != nil || !got.Satisfied || got.ValidRecords != 1 {
		t.Fatalf("complementary sources not complete: %+v %v", got, err)
	}
	b.Provenance.FetchedAt = now.Add(-2 * time.Hour)
	joined, _ = MergeRecords([]Record{a, b}, []string{"name"})
	got, err = EvaluateMergedRecords(spec, joined, now)
	if err != nil || got.Satisfied || len(got.Issues) == 0 {
		t.Fatalf("fresh release laundered stale license: %+v %v", got, err)
	}
}
