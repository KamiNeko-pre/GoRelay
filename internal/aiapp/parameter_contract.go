package aiapp

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// A deliberately small JSON Schema subset for reusable adapter PARAMS:
// flat scalar fields and string arrays. Unsupported constraints are rejected.
type ParameterField struct {
	Type        string          `json:"type"`
	Description string          `json:"description,omitempty"`
	Items       *ParameterField `json:"items,omitempty"`
}
type ParameterSchema struct {
	Type                 string                    `json:"type"`
	Properties           map[string]ParameterField `json:"properties"`
	Required             []string                  `json:"required"`
	AdditionalProperties bool                      `json:"additionalProperties"`
}

func NormalizeParameterContract(raw json.RawMessage, example map[string]any) (string, error) {
	schema := ParameterSchema{Type: "object", Properties: map[string]ParameterField{}, Required: []string{}}
	if len(raw) != 0 && string(raw) != "null" {
		if err := decodeArgs(string(raw), &schema); err != nil {
			return "", err
		}
	} else {
		for name, value := range example {
			kind := ""
			for _, candidate := range []string{"string", "integer", "number", "boolean", "string_array"} {
				if fieldMatches(value, candidate) {
					kind = candidate
					break
				}
			}
			field := ParameterField{Type: kind}
			if kind == "string_array" {
				field.Type = "array"
				field.Items = &ParameterField{Type: "string"}
			}
			if kind == "" {
				return "", fmt.Errorf("parameter %s has unsupported type", name)
			}
			schema.Properties[name] = field
			schema.Required = append(schema.Required, name)
		}
	}
	if err := schema.validate(); err != nil {
		return "", err
	}
	sort.Strings(schema.Required)
	data, err := json.Marshal(schema)
	if err != nil {
		return "", err
	}
	if err = ValidateParameters(string(data), example); err != nil {
		return "", fmt.Errorf("parameter example: %w", err)
	}
	return string(data), nil
}
func (s ParameterSchema) validate() error {
	if s.Type != "object" || s.AdditionalProperties || len(s.Properties) > 32 {
		return fmt.Errorf("parameter schema requires bounded object and additionalProperties=false")
	}
	for name, field := range s.Properties {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("empty parameter name")
		}
		switch field.Type {
		case "string", "integer", "number", "boolean":
			if field.Items != nil {
				return fmt.Errorf("items requires an array")
			}
		case "array":
			if field.Items == nil || field.Items.Type != "string" || field.Items.Items != nil {
				return fmt.Errorf("only string arrays supported")
			}
		default:
			return fmt.Errorf("unsupported parameter type %s", field.Type)
		}
	}
	seen := map[string]bool{}
	for _, name := range s.Required {
		if _, exists := s.Properties[name]; !exists || seen[name] {
			return fmt.Errorf("undefined/duplicate required parameter %s", name)
		}
		seen[name] = true
	}
	return nil
}

// ParameterError names the parameter contract failure and the exact arguments
// that would satisfy it. A bare "missing parameter x" is not actionable: the
// caller cannot see which other parameters exist, their types, or an accepted
// value, so the recorded selector-discovery gate repeated the same rejected
// submit_acquisition twelve times until the run budget was exhausted. The
// contract itself is model-authored, so restating it is not new information —
// it is the same contract the caller already declared.
type ParameterError struct {
	Reason   string            `json:"reason"`
	Required []string          `json:"required,omitempty"`
	Accepted map[string]string `json:"accepted_parameters,omitempty"`
	Example  map[string]any    `json:"example,omitempty"`
}

func (e *ParameterError) Error() string {
	return e.Reason
}

func describeParameterContract(schema ParameterSchema, reason string, example map[string]any) *ParameterError {
	accepted := map[string]string{}
	for name, field := range schema.Properties {
		accepted[name] = field.Type
	}
	return &ParameterError{
		Reason:   reason + "; supply params as {" + strings.Join(schema.Required, ", ") + "}",
		Required: append([]string{}, schema.Required...),
		Accepted: accepted,
		Example:  example,
	}
}

// exampleParameters builds one accepted value per declared property, so the
// caller can see a valid request instead of guessing the value encoding.
func exampleParameters(schema ParameterSchema) map[string]any {
	out := map[string]any{}
	for name, field := range schema.Properties {
		switch field.Type {
		case "integer", "number":
			out[name] = 3
		case "boolean":
			out[name] = true
		case "array":
			out[name] = []string{"value"}
		default:
			out[name] = "value"
		}
	}
	return out
}

func ValidateParameters(raw string, params map[string]any) error {
	if raw == "" {
		return nil
	} // Legacy artifact; new trials always bind a contract.
	var schema ParameterSchema
	if err := decodeArgs(raw, &schema); err != nil {
		return err
	}
	if err := schema.validate(); err != nil {
		return err
	}
	for _, name := range schema.Required {
		if _, ok := params[name]; !ok {
			// Name the whole contract, not just the missing key: the caller
			// declared this schema and can only correct it if reminded.
			return describeParameterContract(schema, "missing required parameter "+name, exampleParameters(schema))
		}
	}
	for name, value := range params {
		field, ok := schema.Properties[name]
		if !ok {
			known := make([]string, 0, len(schema.Properties))
			for n := range schema.Properties {
				known = append(known, n)
			}
			sort.Strings(known)
			return describeParameterContract(schema, "unknown parameter "+name+" (declared parameters: "+strings.Join(known, ", ")+")", exampleParameters(schema))
		}
		kind := field.Type
		if kind == "array" {
			kind = "string_array"
		}
		if !fieldMatches(value, kind) {
			return describeParameterContract(schema, "parameter "+name+" expects "+field.Type, exampleParameters(schema))
		}
	}
	return nil
}
