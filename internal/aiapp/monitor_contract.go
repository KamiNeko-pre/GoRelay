package aiapp

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Conditions refer to a task's typed fields, never to a built-in business type.
type MonitorCondition struct {
	Field    string          `json:"field"`
	Operator string          `json:"operator"`
	Value    json.RawMessage `json:"value"`
}

type CreateMonitorRequest struct {
	RequestKey       string                        `json:"request_key"`
	Goal             string                        `json:"goal"`
	Spec             *TaskSpec                     `json:"spec,omitempty"`
	SourceScope      []string                      `json:"source_scope,omitempty"`
	IntervalSeconds  int64                         `json:"interval_seconds,omitempty"`
	Conditions       []MonitorCondition            `json:"conditions"`
	ConditionGroups  [][]MonitorCondition          `json:"condition_groups,omitempty"` // OR of AND groups, after mandatory conditions.
	EntityConditions map[string][]MonitorCondition `json:"entity_conditions,omitempty"`
}

func (r *CreateMonitorRequest) Normalize() error {
	r.Goal = strings.TrimSpace(r.Goal)
	if r.RequestKey == "" || len(r.RequestKey) > 128 || r.Goal == "" || len(r.Goal) > 8000 {
		return fmt.Errorf("monitor goal and request_key required")
	}
	if r.IntervalSeconds == 0 {
		r.IntervalSeconds = int64(24 * time.Hour / time.Second)
	}
	if r.IntervalSeconds < int64(time.Hour/time.Second) || r.IntervalSeconds > int64(30*24*time.Hour/time.Second) {
		return fmt.Errorf("monitor interval must be 1 hour to 30 days")
	}
	total := len(r.Conditions)
	for _, group := range r.ConditionGroups {
		if len(group) == 0 {
			return fmt.Errorf("OR condition group must not be empty")
		}
		total += len(group)
	}
	if total > 20 {
		return fmt.Errorf("monitor supports at most 20 conditions")
	}
	if r.Spec == nil {
		// The first Agent run can discover/freeze field names before conditions
		// are configured. Never guess a typed condition without that contract.
		if total != 0 || len(r.EntityConditions) != 0 {
			return fmt.Errorf("typed task spec required before configuring conditions")
		}
		return nil
	}
	if r.Spec.Goal != r.Goal {
		return fmt.Errorf("monitor goal and task spec differ")
	}
	if err := r.Spec.Validate(); err != nil {
		return err
	}
	if len(r.Spec.Entities) > 10 {
		return fmt.Errorf("monitor supports at most 10 entities")
	}
	if err := ValidateMonitorConditions(*r.Spec, r.Conditions); err != nil {
		return err
	}
	for _, group := range r.ConditionGroups {
		if err := ValidateMonitorConditions(*r.Spec, group); err != nil {
			return err
		}
	}
	known := map[string]bool{}
	for _, entity := range r.Spec.Entities {
		known[entity] = true
	}
	for entity, conditions := range r.EntityConditions {
		if !known[entity] {
			return fmt.Errorf("condition override refers to unknown entity %q", entity)
		}
		if len(conditions)+total > 20 {
			return fmt.Errorf("monitor supports at most 20 conditions per entity")
		}
		if err := ValidateMonitorConditions(*r.Spec, conditions); err != nil {
			return err
		}
	}
	return nil
}

func (r CreateMonitorRequest) ConditionsFor(entity string) []MonitorCondition {
	return append(append([]MonitorCondition{}, r.Conditions...), r.EntityConditions[entity]...)
}

func MatchMonitorEligibility(spec TaskSpec, config CreateMonitorRequest, entity string, fields map[string]any) (bool, error) {
	mandatory, err := MatchMonitorConditions(spec, config.ConditionsFor(entity), fields)
	if err != nil {
		return false, err
	}
	if len(config.ConditionGroups) == 0 {
		return mandatory, nil
	}
	alternative := false
	for _, group := range config.ConditionGroups {
		if len(group) == 0 {
			return false, fmt.Errorf("OR condition group must not be empty")
		}
		matched, err := MatchMonitorConditions(spec, group, fields)
		if err != nil {
			return false, err
		}
		alternative = alternative || matched
	}
	return mandatory && alternative, nil
}

func conditionValue(c MonitorCondition) (any, error) {
	var value any
	if len(c.Value) == 0 {
		return nil, fmt.Errorf("condition value required")
	}
	if err := json.Unmarshal(c.Value, &value); err != nil {
		return nil, err
	}
	return value, nil
}

func ValidateMonitorConditions(spec TaskSpec, conditions []MonitorCondition) error {
	fields := map[string]string{}
	for _, f := range spec.RequiredFields {
		fields[f.Name] = f.Type
	}
	for _, c := range conditions {
		kind := fields[c.Field]
		value, err := conditionValue(c)
		if kind == "" || err != nil {
			return fmt.Errorf("condition requires an existing typed field and valid JSON value")
		}
		switch c.Operator {
		case "eq":
			if !fieldMatches(value, kind) {
				return fmt.Errorf("condition %s value type differs", c.Field)
			}
		case "lt", "lte", "gt", "gte":
			if (kind != "integer" && kind != "number") || !fieldMatches(value, kind) {
				return fmt.Errorf("ordered condition requires a compatible numeric field")
			}
		case "contains":
			if kind != "string_array" || !fieldMatches(value, "string") {
				return fmt.Errorf("contains condition requires a string array and string member")
			}
		default:
			return fmt.Errorf("unsupported condition operator %q", c.Operator)
		}
	}
	return nil
}

func numericMonitorValue(value any) (float64, error) {
	// JSON round-trip gives Go integer variants and JSON numbers the same
	// representation, without treating strings or null as zero.
	raw, err := json.Marshal(value)
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseFloat(string(raw), 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, fmt.Errorf("invalid numeric field")
	}
	return n, nil
}

func MatchMonitorConditions(spec TaskSpec, conditions []MonitorCondition, fields map[string]any) (bool, error) {
	if err := ValidateMonitorConditions(spec, conditions); err != nil {
		return false, err
	}
	kinds := map[string]string{}
	for _, f := range spec.RequiredFields {
		kinds[f.Name] = f.Type
	}
	matched := true
	for _, c := range conditions {
		actual, exists := fields[c.Field]
		if !exists || !fieldMatches(actual, kinds[c.Field]) {
			return false, fmt.Errorf("condition field %s is missing or has an incompatible type", c.Field)
		}
		wanted, _ := conditionValue(c)
		pass := false
		switch c.Operator {
		case "eq":
			a, err := json.Marshal(actual)
			if err != nil {
				return false, err
			}
			b, _ := json.Marshal(wanted)
			pass = string(a) == string(b)
		case "contains":
			raw, _ := json.Marshal(actual)
			var members []string
			if err := json.Unmarshal(raw, &members); err != nil {
				return false, err
			}
			for _, member := range members {
				if member == wanted.(string) {
					pass = true
					break
				}
			}
		default:
			a, err := numericMonitorValue(actual)
			if err != nil {
				return false, err
			}
			b, err := numericMonitorValue(wanted)
			if err != nil {
				return false, err
			}
			switch c.Operator {
			case "lt":
				pass = a < b
			case "lte":
				pass = a <= b
			case "gt":
				pass = a > b
			case "gte":
				pass = a >= b
			}
		}
		matched = matched && pass
	}
	return matched, nil
}
