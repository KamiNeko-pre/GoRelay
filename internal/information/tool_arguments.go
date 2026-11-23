package information

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"sort"
	"strings"
)

// Domain tools have nested objects, unlike the flat PARAMS contract of a Lua
// capability. Check their advertised shape without reusing that narrower parser.
type informationArgumentShape struct {
	Type                 string                     `json:"type"`
	Properties           map[string]json.RawMessage `json:"properties"`
	Required             []string                   `json:"required"`
	AdditionalProperties json.RawMessage            `json:"additionalProperties"`
	Items                json.RawMessage            `json:"items"`
	Enum                 []any                      `json:"enum"`
	Minimum              *json.Number               `json:"minimum"`
	Maximum              *json.Number               `json:"maximum"`
	MinLength            *int                       `json:"minLength"`
}

func checkInformationArgumentShape(raw json.RawMessage, value any, path string) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("true")) {
		return nil
	}
	if bytes.Equal(trimmed, []byte("false")) {
		if value != nil {
			return fmt.Errorf("parameter %s is not permitted", path)
		}
		return nil
	}
	var shape informationArgumentShape
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&shape); err != nil {
		return nil
	}
	wrongType := func() error { return fmt.Errorf("parameter %s expects %s", path, shape.Type) }
	switch shape.Type {
	case "object":
		object, ok := value.(map[string]any)
		if !ok {
			return wrongType()
		}
		for _, field := range shape.Required {
			if v, exists := object[field]; !exists || v == nil {
				return fmt.Errorf("missing required parameter %s.%s", path, field)
			}
		}
		keys := []string{}
		for field := range object {
			keys = append(keys, field)
		}
		sort.Strings(keys)
		for _, field := range keys {
			child, declared := shape.Properties[field]
			if !declared {
				trimmedAdditional := bytes.TrimSpace(shape.AdditionalProperties)
				if len(trimmedAdditional) == 0 || bytes.Equal(trimmedAdditional, []byte("true")) {
					continue
				}
				if bytes.Equal(trimmedAdditional, []byte("false")) {
					return fmt.Errorf("unexpected parameter %s.%s (additionalProperties is false)", path, field)
				}
				if err := checkInformationArgumentShape(trimmedAdditional, object[field], path+"."+field); err != nil {
					return err
				}
				continue
			}
			if err := checkInformationArgumentShape(child, object[field], path+"."+field); err != nil {
				return err
			}
		}
	case "array":
		array, ok := value.([]any)
		if !ok {
			return wrongType()
		}
		if len(shape.Items) > 0 {
			for i, item := range array {
				if err := checkInformationArgumentShape(shape.Items, item, fmt.Sprintf("%s[%d]", path, i)); err != nil {
					// Allow 1D string array to pass when 2D array expected (coerced by custom UnmarshalJSON)
					var itemShape informationArgumentShape
					if e := json.Unmarshal(shape.Items, &itemShape); e == nil && itemShape.Type == "array" {
						if _, isStr := item.(string); isStr {
							continue
						}
					}
					return err
				}
			}
		}
	case "string":
		text, ok := value.(string)
		if !ok {
			return wrongType()
		}
		if shape.MinLength != nil && len([]rune(text)) < *shape.MinLength {
			return fmt.Errorf("parameter %s requires at least %d characters", path, *shape.MinLength)
		}
	case "number", "integer":
		n, ok := toolArgumentNumber(value)
		if !ok || shape.Type == "integer" && !n.IsInt() {
			return wrongType()
		}
		if shape.Minimum != nil {
			minimum, _ := toolArgumentNumber(*shape.Minimum)
			if n.Cmp(minimum) < 0 {
				return fmt.Errorf("parameter %s must be >= %v", path, *shape.Minimum)
			}
		}
		if shape.Maximum != nil {
			maximum, _ := toolArgumentNumber(*shape.Maximum)
			if n.Cmp(maximum) > 0 {
				return fmt.Errorf("parameter %s must be <= %v", path, *shape.Maximum)
			}
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return wrongType()
		}
	}
	if len(shape.Enum) > 0 {
		actual, _ := json.Marshal(value)
		matched := false
		for _, allowed := range shape.Enum {
			a, actualNumber := toolArgumentNumber(value)
			b, allowedNumber := toolArgumentNumber(allowed)
			if actualNumber && allowedNumber {
				matched = matched || a.Cmp(b) == 0
				continue
			}
			encoded, _ := json.Marshal(allowed)
			matched = matched || string(actual) == string(encoded)
		}
		if !matched {
			return fmt.Errorf("parameter %s must be one of %v", path, shape.Enum)
		}
	}
	return nil
}

func normalizePointerMapValues(m map[string]any) {
	for k, v := range m {
		if s, ok := v.(string); ok {
			if np, err := normalizeJSONPointer(s); err == nil {
				m[k] = np
			}
		}
	}
}

// normalizeToolArgsDeterministic canonicalizes deterministic model-supplied aliases
// and JSON pointers BEFORE idempotency hashing and schema checks, without touching
// mutable database-dependent defaults (which are bound once in bindCurrentRead).
func normalizeToolArgsDeterministic(name, raw string) string {
	cleaned := cleanJSONString(raw)
	var args map[string]any
	decoder := json.NewDecoder(strings.NewReader(cleaned))
	decoder.UseNumber()
	if err := decoder.Decode(&args); err != nil || args == nil {
		return raw
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return raw
	}
	switch name {
	case "update_plan":
		if planVal, hasPlan := args["plan"]; hasPlan {
			if planMap, ok := planVal.(map[string]any); ok {
				for k, v := range planMap {
					if _, exists := args[k]; !exists {
						args[k] = v
					}
				}
			} else if planText, ok := planVal.(string); ok && strings.TrimSpace(planText) != "" {
				if _, exists := args["coverage_rationale"]; !exists {
					args["coverage_rationale"] = planText
				}
			}
			delete(args, "plan")
		}
	case "run_lua":
		if code, ok := args["code"].(string); ok {
			args["code"] = strings.TrimSpace(code)
		}
		if mode, present := args["mode"]; !present {
			args["mode"] = "sample"
		} else if mode, ok := mode.(string); ok {
			mode = strings.ToLower(strings.TrimSpace(mode))
			if mode == "" {
				mode = "sample"
			}
			args["mode"] = mode
		}
	case "acquire_sources":
		if apiMap, ok := args["api"].(map[string]any); ok {
			normalizeRequestMethod(apiMap)
			for _, pk := range []string{"items_pointer", "next_pointer"} {
				if v, exists := apiMap[pk].(string); exists {
					if np, err := normalizeJSONPointer(v); err == nil {
						apiMap[pk] = np
					}
				}
			}
			if fieldsMap, ok := apiMap["fields"].(map[string]any); ok {
				normalizePointerMapValues(fieldsMap)
			}
		}
		if args["api"] == nil && args["asset_id"] == nil && args["inspect_url"] == nil && args["inspect_urls"] == nil {
			normalizeRequestMethod(args)
			urls, list := args["urls"].([]any)
			if _, provided := args["urls"]; !provided || list && len(urls) == 0 {
				if u, ok := args["url"].(string); ok && strings.TrimSpace(u) != "" {
					urls = []any{u}
					args["urls"] = urls
					delete(args, "url")
				}
			}
			for i, value := range urls {
				if u, ok := value.(string); ok {
					u = strings.TrimSpace(u)
					if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
						u = "https://" + u
					}
					urls[i] = u
				}
			}
		}
	case "read_materials":
		kind, _ := args["kind"].(string)
		kind = strings.ToLower(strings.TrimSpace(kind))
		if kind == "" {
			if d, _ := args["delivery_id"].(string); strings.TrimSpace(d) != "" {
				kind = "delivery"
			}
		}
		if kind != "" {
			args["kind"] = kind
		}
	case "submit_delivery":
		if outcome, ok := args["outcome"].(string); ok {
			args["outcome"] = normalizeDeliveryOutcome(outcome)
		}
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return raw
	}
	return string(encoded)
}

func normalizeRequestMethod(args map[string]any) {
	if value, present := args["method"]; !present {
		args["method"] = "GET"
	} else if method, ok := value.(string); ok {
		method = strings.ToUpper(strings.TrimSpace(method))
		if method == "" {
			method = "GET"
		}
		args["method"] = method
	}
}

func validateInformationToolArgs(name, raw string) error {
	normalizedRaw := normalizeToolArgsDeterministic(name, raw)
	var args map[string]any
	if err := decodeInformationArgs(normalizedRaw, &args); err != nil {
		return err
	}
	for _, def := range informationToolDefinitions() {
		if def.Function.Name != name {
			continue
		}
		if err := checkInformationArgumentShape(def.Function.Parameters, args, name); err != nil {
			return actionFailure("ROUTING_ARGUMENTS_INVALID", fmt.Errorf("%s: %w", name, err), "Correct the exact missing or invalid parameter above using the advertised schema and observed source facts. A corrected real parameter is a new request; changing call_id or reason cannot repeat the old request.")
		}
		return nil
	}
	return actionFailure("UNKNOWN_TOOL", fmt.Errorf("unknown information tool %q", name), "Choose an advertised tool and its exact schema.")
}

func toolArgumentNumber(value any) (*big.Rat, bool) {
	switch value.(type) {
	case json.Number, float64, float32, int, int64, int32, uint64, uint:
		return viewNumber(value)
	}
	return nil, false
}
