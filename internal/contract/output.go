package contract

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

type OutputField struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Required    bool   `json:"required,omitempty"`
	Description string `json:"description,omitempty"`
}

type OutputSchema struct {
	Fields      []OutputField `json:"fields"`
	EntityKeys  []string      `json:"entity_keys,omitempty"`
	MinRows     int           `json:"min_rows,omitempty"`
	MaxTextLen  int           `json:"max_text_len,omitempty"`
}

// FieldMatches validates a value against a canonical GoRelay output field type.
func FieldMatches(v any, kind string) bool {
	switch kind {
	case "string", "source_text":
		s, ok := v.(string)
		return ok && strings.TrimSpace(s) != ""
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "number", "integer":
		var n float64
		switch x := v.(type) {
		case float64:
			n = x
		case int:
			n = float64(x)
		case int64:
			n = float64(x)
		case json.Number:
			var err error
			n, err = x.Float64()
			if err != nil {
				return false
			}
		default:
			return false
		}
		return !math.IsNaN(n) && !math.IsInf(n, 0) && (kind != "integer" || math.Trunc(n) == n)
	case "string_array":
		switch a := v.(type) {
		case []string:
			for _, s := range a {
				if strings.TrimSpace(s) == "" {
					return false
				}
			}
			return true
		case []any:
			for _, s := range a {
				if !FieldMatches(s, "string") {
					return false
				}
			}
			return true
		}
	}
	return false
}

// NormalizeOutputSchema parses and canonicalizes a JSON output_schema definition.
func NormalizeOutputSchema(raw json.RawMessage) (string, OutputSchema, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", OutputSchema{}, fmt.Errorf("output_schema is required")
	}
	var schema OutputSchema
	if err := json.Unmarshal(raw, &schema); err != nil {
		return "", OutputSchema{}, fmt.Errorf("decode output_schema: %w", err)
	}
	if len(schema.Fields) == 0 {
		return "", OutputSchema{}, fmt.Errorf("output_schema requires at least one field")
	}
	seen := make(map[string]bool, len(schema.Fields))
	for i := range schema.Fields {
		f := &schema.Fields[i]
		f.Name = strings.TrimSpace(f.Name)
		f.Type = strings.TrimSpace(f.Type)
		if f.Name == "" {
			return "", OutputSchema{}, fmt.Errorf("output field name must not be empty")
		}
		if seen[f.Name] {
			return "", OutputSchema{}, fmt.Errorf("duplicate output field %q", f.Name)
		}
		seen[f.Name] = true
		switch f.Type {
		case "string", "source_text", "number", "integer", "boolean", "string_array":
		default:
			return "", OutputSchema{}, fmt.Errorf("unsupported field type %q for %q", f.Type, f.Name)
		}
	}
	for _, key := range schema.EntityKeys {
		if !seen[key] {
			return "", OutputSchema{}, fmt.Errorf("entity_key %q is not in output_schema.fields", key)
		}
	}
	sort.Slice(schema.Fields, func(i, j int) bool {
		return schema.Fields[i].Name < schema.Fields[j].Name
	})
	encoded, err := json.Marshal(schema)
	return string(encoded), schema, err
}
