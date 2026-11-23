package contract

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

type ParameterField struct {
	Description          string                    `json:"description,omitempty"`
	Type                 string                    `json:"type,omitempty"`
	Enum                 []any                     `json:"enum,omitempty"`
	Required             []string                  `json:"required,omitempty"`
	Properties           map[string]ParameterField `json:"properties,omitempty"`
	Items                *ParameterField           `json:"items,omitempty"`
	AdditionalProperties json.RawMessage           `json:"additionalProperties,omitempty"`
	Minimum              *json.Number              `json:"minimum,omitempty"`
	Maximum              *json.Number              `json:"maximum,omitempty"`
}

type ParameterSchema struct {
	Type                 string                    `json:"type"`
	Properties           map[string]ParameterField `json:"properties"`
	Required             []string                  `json:"required"`
	AdditionalProperties any                       `json:"additionalProperties"`
}

func inferParameterField(value any) ParameterField {
	field := ParameterField{}
	switch v := value.(type) {
	case nil:
		field.Type = "null"
	case string:
		field.Type = "string"
	case bool:
		field.Type = "boolean"
	case json.Number:
		field.Type = "number"
		if FieldMatches(v, "integer") {
			field.Type = "integer"
		}
	case []any:
		field.Type = "array"
	case map[string]any:
		field.Type = "object"
		field.Properties = map[string]ParameterField{}
		field.AdditionalProperties = json.RawMessage("false")
		for key, item := range v {
			field.Properties[key] = inferParameterField(item)
			field.Required = append(field.Required, key)
		}
		sort.Strings(field.Required)
	}
	return field
}

func NormalizeParameterContract(raw json.RawMessage, example map[string]any) (string, error) {
	if len(raw) != 0 && string(raw) != "null" {
		var schema ParameterSchema
		if err := json.Unmarshal(raw, &schema); err != nil {
			return "", err
		}
		if err := schema.validate(); err != nil {
			return "", err
		}
		if err := ValidateParameters(string(raw), example); err != nil {
			return "", err
		}
		var canonical any
		decoder := json.NewDecoder(strings.NewReader(string(raw)))
		decoder.UseNumber()
		if err := decoder.Decode(&canonical); err != nil {
			return "", err
		}
		encoded, err := json.Marshal(canonical)
		return string(encoded), err
	}
	encoded, err := json.Marshal(example)
	if err != nil {
		return "", err
	}
	var value any
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return "", err
	}
	if value == nil {
		value = map[string]any{}
	}
	field := inferParameterField(value)
	schema := ParameterSchema{Type: "object", Properties: field.Properties, Required: field.Required, AdditionalProperties: false}
	encoded, err = json.Marshal(schema)
	if err != nil {
		return "", err
	}
	return string(encoded), ValidateParameters(string(encoded), example)
}

func (s ParameterSchema) validate() error {
	if s.Type != "object" {
		return fmt.Errorf("parameter schema requires an object")
	}
	return nil
}

type ParameterError struct {
	Reason   string            `json:"reason"`
	Required []string          `json:"required,omitempty"`
	Accepted map[string]string `json:"accepted_parameters,omitempty"`
	Example  map[string]any    `json:"example,omitempty"`
}

func (e *ParameterError) Error() string { return e.Reason }

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

func exampleParameters(schema ParameterSchema) map[string]any {
	out := map[string]any{}
	for name, field := range schema.Properties {
		out[name] = exampleParameterField(field)
	}
	return out
}

func exampleParameterField(field ParameterField) any {
	if len(field.Enum) > 0 {
		return field.Enum[0]
	}
	switch field.Type {
	case "integer", "number":
		if field.Minimum != nil {
			return *field.Minimum
		}
		return 1
	case "boolean":
		return true
	case "null":
		return nil
	case "object":
		out := map[string]any{}
		for name, child := range field.Properties {
			out[name] = exampleParameterField(child)
		}
		return out
	case "array":
		if field.Items != nil {
			return []any{exampleParameterField(*field.Items)}
		}
		return []any{}
	default:
		return "value"
	}
}

func ValidateParameters(raw string, params map[string]any) error {
	if raw == "" {
		return nil
	}
	var schema ParameterSchema
	if err := json.Unmarshal([]byte(raw), &schema); err != nil {
		return err
	}
	if err := schema.validate(); err != nil {
		return err
	}
	if params == nil {
		params = map[string]any{}
	}
	for _, req := range schema.Required {
		if _, ok := params[req]; !ok {
			return describeParameterContract(schema, fmt.Sprintf("missing required parameter: %s", req), exampleParameters(schema))
		}
	}
	return nil
}
