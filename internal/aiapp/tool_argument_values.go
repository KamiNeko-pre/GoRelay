package aiapp

import (
	"encoding/json"
	"fmt"
	"math/big"
	"reflect"
	"sort"
	"strings"
)

// Validate the constraints already published to the model, rather than letting
// zero-value structs or per-tool checks turn them into an unspecified invalid.
// This is the tool schema subset in AgentToolDefinitions, not an artifact schema.
type toolArgumentSchema struct {
	Type                 string                        `json:"type"`
	Enum                 []any                         `json:"enum"`
	Required             []string                      `json:"required"`
	Properties           map[string]toolArgumentSchema `json:"properties"`
	Items                *toolArgumentSchema           `json:"items"`
	AdditionalProperties json.RawMessage               `json:"additionalProperties"`
	Minimum              *json.Number                  `json:"minimum"`
	Maximum              *json.Number                  `json:"maximum"`
	MaxItems             *int                          `json:"maxItems"`
	MinItems             *int                          `json:"minItems"`
	If                   *toolArgumentSchema           `json:"if"`
	Then                 *toolArgumentSchema           `json:"then"`
	MinProperties        *int                          `json:"minProperties"`
}

func validateToolArgumentValues(fn FunctionCall, raw json.RawMessage) error {
	var schema toolArgumentSchema
	if json.Unmarshal(raw, &schema) != nil {
		return nil
	}
	var args any
	decoder := json.NewDecoder(strings.NewReader(fn.Arguments))
	decoder.UseNumber()
	if decoder.Decode(&args) != nil {
		return nil
	} // decodeArgs owns malformed JSON.
	return checkToolArgumentValue(fn.Name, "", args, schema)
}

func checkToolArgumentValue(tool, path string, value any, schema toolArgumentSchema) error {
	invalid := func(expected string, actual any) error {
		field := path
		if field == "" {
			field = "arguments"
		}
		message := fmt.Sprintf("%s parameter %s: expected %s", tool, field, expected)
		return toolContractFailure(fmt.Errorf("%s", message), "ARGUMENT_CONTRACT_ERROR", "not_started", map[string]any{"field": field, "expected": expected, "actual": actual}, ToolRecovery{Strategy: "correct_arguments", Tool: tool, Instruction: message + ". Correct this field using the published schema and observed values; changing the call ID or repeating unchanged arguments cannot repair it."})
	}
	typeName := "null"
	switch value.(type) {
	case map[string]any:
		typeName = "object"
	case []any:
		typeName = "array"
	case string:
		typeName = "string"
	case bool:
		typeName = "boolean"
	case json.Number:
		typeName = "number"
	}
	validType := schema.Type == "" || schema.Type == typeName
	if schema.Type == "integer" {
		if number, ok := value.(json.Number); ok {
			n, ok := new(big.Rat).SetString(string(number))
			validType = ok && n.IsInt()
		}
	}
	if !validType {
		return invalid(schema.Type, typeName)
	}
	if len(schema.Enum) > 0 {
		valid := false
		for _, option := range schema.Enum {
			if reflect.DeepEqual(option, value) {
				valid = true
				break
			}
		}
		if !valid {
			raw, _ := json.Marshal(schema.Enum)
			return invalid("one of "+string(raw), value)
		}
	}
	if schema.If != nil && schema.Then != nil && checkToolArgumentValue(tool, path, value, *schema.If) == nil {
		if err := checkToolArgumentValue(tool, path, value, *schema.Then); err != nil {
			return err
		}
	}
	if number, ok := value.(json.Number); ok {
		n, _ := new(big.Rat).SetString(string(number))
		for _, bound := range []struct {
			value   *json.Number
			minimum bool
		}{{schema.Minimum, true}, {schema.Maximum, false}} {
			if bound.value == nil || n == nil {
				continue
			}
			limit, ok := new(big.Rat).SetString(string(*bound.value))
			if !ok {
				continue
			}
			cmp := n.Cmp(limit)
			if bound.minimum && cmp < 0 {
				return invalid("a number >= "+string(*bound.value), number)
			}
			if !bound.minimum && cmp > 0 {
				return invalid("a number <= "+string(*bound.value), number)
			}
		}
	}
	childPath := func(key string) string {
		if path == "" {
			return key
		}
		return path + "." + key
	}
	switch v := value.(type) {
	case map[string]any:
		for _, key := range schema.Required {
			x, present := v[key]
			if !present || x == nil {
				return checkMissingToolField(tool, childPath(key), schema.Properties[key].Type)
			}
		}
		if schema.MinProperties != nil && len(v) < *schema.MinProperties {
			return invalid(fmt.Sprintf("at least %d object properties", *schema.MinProperties), len(v))
		}
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			child, known := schema.Properties[key]
			if !known {
				if string(schema.AdditionalProperties) == "false" {
					return &ArgumentDecodeError{Reason: "unknown parameter " + childPath(key), Field: childPath(key), Expected: "a parameter declared in the current tool schema"}
				}
				continue
			}
			if err := checkToolArgumentValue(tool, childPath(key), v[key], child); err != nil {
				return err
			}
		}
	case []any:
		if schema.MinItems != nil && len(v) < *schema.MinItems {
			return invalid(fmt.Sprintf("at least %d array entries", *schema.MinItems), len(v))
		}
		if schema.MaxItems != nil && len(v) > *schema.MaxItems {
			return invalid(fmt.Sprintf("at most %d array entries", *schema.MaxItems), len(v))
		}
		if schema.Items != nil {
			for i, item := range v {
				if err := checkToolArgumentValue(tool, fmt.Sprintf("%s[%d]", path, i), item, *schema.Items); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func checkMissingToolField(tool, path, expected string) error {
	message := fmt.Sprintf("%s missing required parameter %s (expected %s)", tool, path, expected)
	return toolContractFailure(fmt.Errorf("%s", message), "MISSING_REQUIRED_PARAMETER", "not_started", map[string]any{"field": path, "missing": []string{path}, "expected": expected}, ToolRecovery{Strategy: "correct_arguments", Tool: tool, Instruction: message + ". Supply the named nested field using the current tool schema; do not repeat unchanged arguments."})
}
