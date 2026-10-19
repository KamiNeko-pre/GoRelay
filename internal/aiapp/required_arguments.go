package aiapp

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Required fields have one source of truth: the same schema sent to the model.
// Parsing/type errors remain owned by decodeArgs; this checks missing fields
// before zero-value structs can turn them into vague execution errors.
func requiredToolArguments(fn FunctionCall, defs []ToolDefinition) error {
	var args map[string]json.RawMessage
	if json.Unmarshal([]byte(fn.Arguments), &args) != nil || args == nil {
		return nil
	}
	for _, d := range defs {
		if d.Function.Name != fn.Name {
			continue
		}
		var schema struct {
			Required   []string `json:"required"`
			Properties map[string]struct {
				Type string `json:"type"`
			} `json:"properties"`
			OneOf []struct {
				Required []string `json:"required"`
			} `json:"oneOf"`
		}
		if json.Unmarshal(d.Function.Parameters, &schema) != nil {
			return nil
		}
		for key := range args {
			if _, ok := schema.Properties[key]; !ok {
				return &ArgumentDecodeError{Reason: "unknown parameter " + key, Field: key, Expected: "a parameter declared in the current tool schema"}
			}
		}
		missing := []string{}
		has := func(k string) bool { v := args[k]; return len(v) > 0 && string(v) != "null" && string(v) != `""` }
		for _, k := range schema.Required {
			if !has(k) {
				missing = append(missing, k)
			}
		}
		alternatives := [][]string{}
		if len(schema.OneOf) > 0 {
			matches := 0
			for _, option := range schema.OneOf {
				valid := true
				for _, k := range option.Required {
					if !has(k) {
						valid = false
					}
				}
				if valid {
					matches++
				}
				alternatives = append(alternatives, option.Required)
			}
			if matches > 1 {
				message := fmt.Sprintf("%s requires exactly one argument alternative, got %d: %v", fn.Name, matches, alternatives)
				return toolContractFailure(fmt.Errorf("%s", message), "ARGUMENT_CONTRACT_ERROR", "not_started", map[string]any{"field": strings.Join(schema.OneOf[0].Required, " + "), "alternatives": alternatives, "matched_alternatives": matches}, ToolRecovery{Strategy: "correct_arguments", Tool: fn.Name, Instruction: message + ". Keep one actual observed resource identity and remove the conflicting alternative; do not invent a replacement ID."})
			}
			if matches == 0 {
				for _, option := range alternatives {
					missing = append(missing, strings.Join(option, " + "))
				}
			}
		}
		if len(missing) == 0 {
			return validateToolArgumentValues(fn, d.Function.Parameters)
		}
		sort.Strings(missing)
		accepted := map[string]string{}
		example := map[string]any{}
		for k, p := range schema.Properties {
			accepted[k] = p.Type
		}
		for _, k := range schema.Required {
			example[k] = "<" + accepted[k] + ": " + k + ">"
		}
		message := "missing required parameter(s): " + strings.Join(missing, ", ")
		return toolContractFailure(fmt.Errorf("%s", message), "MISSING_REQUIRED_PARAMETER", "not_started", map[string]any{"field": missing[0], "missing": missing, "accepted": accepted, "alternatives": alternatives, "example": example}, ToolRecovery{Strategy: "correct_arguments", Tool: fn.Name, Instruction: message + ". Supply the named fields with values from the user's task or observed receipts; preserve the rest of the successful work. An unchanged call or renamed call ID cannot correct this error."})
	}
	return nil
}
