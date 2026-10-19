package aiapp

import (
	"strings"
	"testing"
)

func TestProjectionSeparatesReceiptURLButKeepsRequestedURLAndBusinessFacts(t *testing.T) {
	spec := TaskSpec{RequiredFields: []FieldRequirement{{Name: "name", Type: "string"}, {Name: "homepage", Type: "string"}}}
	first := Record{Fields: map[string]any{"name": "Alpha", "homepage": "https://alpha.example/", "source_url": "https://source.example/old"}}
	before, err := projectionBody(first, spec)
	if err != nil {
		t.Fatal(err)
	}
	first.Fields["source_url"] = "https://source.example/new"
	after, err := projectionBody(first, spec)
	if err != nil || after != before {
		t.Fatal("receipt-only URL contaminated body identity")
	}
	first.Fields["homepage"] = "https://alpha.example/new"
	business, _ := projectionBody(first, spec)
	if business == before {
		t.Fatal("real business fact change disappeared")
	}
	spec.RequiredFields = append(spec.RequiredFields, FieldRequirement{Name: "source_url", Type: "string"})
	requested, _ := projectionBody(first, spec)
	if !strings.Contains(requested, "https://source.example/new") {
		t.Fatal("explicit URL requirement was dropped")
	}
	first.Fields["body"] = "user-supplied acquired body"
	if body, _ := projectionBody(first, spec); body != "user-supplied acquired body" {
		t.Fatal("raw acquired body transformed")
	}
}
