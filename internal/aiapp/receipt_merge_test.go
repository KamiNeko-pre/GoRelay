package aiapp

import (
	"testing"
	"time"
)

func TestReceiptURLsDoNotConflictWithComplementaryBusinessFacts(t *testing.T) {
	spec := TaskSpec{Goal: "facts", SchemaVersion: "v1", RequiredFields: []FieldRequirement{{Name: "name", Type: "string"}, {Name: "version", Type: "string"}, {Name: "license", Type: "string"}}, JoinKeys: []string{"name"}}
	for _, host := range []string{"one.example", "sibling.example"} {
		p := Provenance{SourceID: host, URL: "https://" + host + "/", FetchedAt: time.Now(), TaskID: 1, DataVersion: "v1"}
		a := Record{Key: "a", SchemaVersion: "v1", Fields: map[string]any{"name": "Alpha", "version": "1", "source_url": p.URL}, Provenance: p}
		p.URL = "https://other.example/"
		p.TaskID = 2
		b := Record{Key: "b", SchemaVersion: "v1", Fields: map[string]any{"name": "Alpha", "license": "MIT", "source_url": p.URL}, Provenance: p}
		result, err := evaluateRows(spec, []ChildResult{{Records: []Record{a, b}}})
		if err != nil || !result.Coverage.Satisfied || len(result.Conflicts) != 0 {
			t.Fatalf("receipt marker blocked valid multi-source join: %+v %v", result, err)
		}
		specURL := spec
		specURL.RequiredFields = append(specURL.RequiredFields, FieldRequirement{Name: "source_url", Type: "string"})
		result, _ = evaluateRows(specURL, []ChildResult{{Records: []Record{a, b}}})
		if len(result.Conflicts) == 0 {
			t.Fatal("explicit required URL conflict erased")
		}
	}
}
