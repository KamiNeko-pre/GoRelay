package aiapp

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestParameterRecoveryExampleSatisfiesDeclaredTypes(t *testing.T) {
	for _, raw := range []string{
		`{"type":"object","properties":{"rate":{"type":"number"}},"required":["rate"],"additionalProperties":false}`,
		`{"type":"object","properties":{"urls":{"type":"array","items":{"type":"string"}}},"required":["urls"],"additionalProperties":false}`,
	} {
		err := ValidateParameters(raw, nil)
		var diagnostic *ParameterError
		if !errors.As(err, &diagnostic) {
			t.Fatal("missing structured correction", err)
		}
		if err := ValidateParameters(raw, diagnostic.Example); err != nil {
			t.Fatalf("host's proposed correction violates its own contract: %v", err)
		}
	}
}

func TestParameterContractTypesAndExamples(t *testing.T) {
	raw := json.RawMessage(`{"type":"object","properties":{"page":{"type":"integer"},"query":{"type":"string"},"tags":{"type":"array","items":{"type":"string"}}},"required":["page"],"additionalProperties":false}`)
	contract, err := NormalizeParameterContract(raw, map[string]any{"page": json.Number("1"), "query": "release"})
	if err != nil {
		t.Fatal(err)
	}
	for _, params := range []map[string]any{{"page": json.Number("2")}, {"page": float64(2), "tags": []any{"go", "redis"}}} {
		if err := ValidateParameters(contract, params); err != nil {
			t.Fatal(err)
		}
	}
	for _, params := range []map[string]any{{}, {"page": "2"}, {"page": 1.5}, {"page": 2, "shell": "whoami"}, {"page": 2, "tags": []any{3}}} {
		if err := ValidateParameters(contract, params); err == nil {
			t.Fatalf("invalid params accepted: %+v", params)
		}
	}
	if _, err := NormalizeParameterContract(raw, map[string]any{"page": "wrong"}); err == nil {
		t.Fatal("example violates registered schema")
	}
}

func TestParameterContractInferredTrialAndUnsupportedSchema(t *testing.T) {
	contract, err := NormalizeParameterContract(nil, map[string]any{"cursor": "abc", "limit": json.Number("10")})
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateParameters(contract, map[string]any{"cursor": "next", "limit": 20}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateParameters(contract, map[string]any{"cursor": "next"}); err == nil {
		t.Fatal("missing trial parameter accepted")
	}
	for _, raw := range []string{`{"type":"object","properties":{"nested":{"type":"object"}},"required":[],"additionalProperties":false}`, `{"type":"object","properties":{},"required":["absent"],"additionalProperties":false}`, `{"type":"object","properties":{},"additionalProperties":true}`} {
		if _, err := NormalizeParameterContract(json.RawMessage(raw), nil); err == nil {
			t.Fatalf("unsupported schema accepted: %s", raw)
		}
	}
	// Historical artifacts without an input contract retain their legacy path.
	if err := ValidateParameters("", map[string]any{"legacy": true}); err != nil {
		t.Fatal(err)
	}
}
