package contract

import (
	"encoding/json"
	"testing"
)

func TestOutputAndParameterContracts(t *testing.T) {
	rawOut, schema, err := NormalizeOutputSchema(json.RawMessage(`{
		"entity_keys": ["id"],
		"fields": [
			{"name": "title", "type": "string", "required": true},
			{"name": "id", "type": "string", "required": true},
			{"name": "stars", "type": "integer"}
		]
	}`))
	if err != nil {
		t.Fatalf("NormalizeOutputSchema failed: %v", err)
	}
	if rawOut == "" || len(schema.Fields) != 3 || schema.Fields[0].Name != "id" {
		t.Fatalf("unexpected normalized output schema: %+v", schema)
	}

	paramSchema, err := NormalizeParameterContract(nil, map[string]any{"org": "golang", "page": 1})
	if err != nil {
		t.Fatalf("NormalizeParameterContract failed: %v", err)
	}
	if err := ValidateParameters(paramSchema, map[string]any{"org": "golang", "page": 1}); err != nil {
		t.Fatalf("valid params rejected: %v", err)
	}
	if err := ValidateParameters(paramSchema, map[string]any{"org": "golang"}); err == nil {
		t.Fatal("missing required parameter was accepted")
	}
}
