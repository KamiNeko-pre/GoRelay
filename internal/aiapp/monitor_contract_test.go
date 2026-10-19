package aiapp

import (
	"encoding/json"
	"testing"
	"time"
)

func monitorSpec() TaskSpec {
	return TaskSpec{Goal: "watch public facts", SchemaVersion: "v1", RequiredFields: []FieldRequirement{{"id", "string"}, {"amount", "integer"}, {"unit", "string"}, {"labels", "string_array"}}, JoinKeys: []string{"id"}, EntityField: "id", Entities: []string{"a", "b"}, MaxAgeSeconds: 3600}
}

func TestMonitorEligibilitySupportsOrOfAndGroups(t *testing.T) {
	spec := monitorSpec()
	in := CreateMonitorRequest{RequestKey: "alternatives", Goal: spec.Goal, Spec: &spec, Conditions: []MonitorCondition{{Field: "unit", Operator: "eq", Value: json.RawMessage(`"CNY"`)}}, ConditionGroups: [][]MonitorCondition{
		{{Field: "amount", Operator: "lte", Value: json.RawMessage(`3000`)}},
		{{Field: "amount", Operator: "lte", Value: json.RawMessage(`5000`)}, {Field: "labels", Operator: "contains", Value: json.RawMessage(`"discounted"`)}},
	}}
	if err := in.Normalize(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		amount any
		unit   string
		labels []string
		want   bool
	}{
		{2500, "CNY", []string{}, true}, {4500, "CNY", []string{"discounted"}, true}, {4500, "CNY", []string{}, false}, {2500, "USD", []string{"discounted"}, false},
	} {
		got, err := MatchMonitorEligibility(spec, in, "a", map[string]any{"amount": tc.amount, "unit": tc.unit, "labels": tc.labels})
		if err != nil || got != tc.want {
			t.Fatal(tc, got, err)
		}
	}
	if got, err := MatchMonitorEligibility(spec, in, "a", map[string]any{"amount": nil, "unit": "CNY", "labels": []string{"discounted"}}); err == nil || got {
		t.Fatal("missing price accepted", got, err)
	}
	in.ConditionGroups = [][]MonitorCondition{{}}
	if err := in.Normalize(); err == nil {
		t.Fatal("empty OR group matched everything")
	}
}

func TestMonitorConditionsAreTypedAndBusinessIndependent(t *testing.T) {
	spec := monitorSpec()
	conditions := []MonitorCondition{{Field: "amount", Operator: "lte", Value: json.RawMessage(`5000`)}, {Field: "unit", Operator: "eq", Value: json.RawMessage(`"CNY"`)}, {Field: "labels", Operator: "contains", Value: json.RawMessage(`"zh"`)}}
	if err := ValidateMonitorConditions(spec, conditions); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		fields map[string]any
		match  bool
	}{
		{map[string]any{"amount": 4500, "unit": "CNY", "labels": []string{"zh", "en"}}, true},
		{map[string]any{"amount": float64(5000), "unit": "CNY", "labels": []any{"zh"}}, true},
		{map[string]any{"amount": 5100, "unit": "CNY", "labels": []string{"zh"}}, false},
		{map[string]any{"amount": 4500, "unit": "USD", "labels": []string{"zh"}}, false},
		{map[string]any{"amount": 4500, "unit": "CNY", "labels": []string{"en"}}, false},
	} {
		got, err := MatchMonitorConditions(spec, conditions, tc.fields)
		if err != nil || got != tc.match {
			t.Fatalf("fields=%v match=%v err=%v", tc.fields, got, err)
		}
	}
	// A sibling task uses unrelated columns and receives the same operators.
	sibling := TaskSpec{Goal: "public events", SchemaVersion: "v1", RequiredFields: []FieldRequirement{{"available", "boolean"}, {"seats", "number"}}}
	cs := []MonitorCondition{{Field: "available", Operator: "eq", Value: json.RawMessage(`true`)}, {Field: "seats", Operator: "gte", Value: json.RawMessage(`2`)}}
	if ok, err := MatchMonitorConditions(sibling, cs, map[string]any{"available": true, "seats": 3.5}); err != nil || !ok {
		t.Fatal(ok, err)
	}
}

func TestMonitorConditionsDoNotConvertMissingOrWrongTypesIntoMatches(t *testing.T) {
	spec := monitorSpec()
	c := []MonitorCondition{{Field: "amount", Operator: "lte", Value: json.RawMessage(`5000`)}}
	for _, fields := range []map[string]any{{}, {"amount": "4500"}, {"amount": nil}, {"amount": 1.2}} {
		if got, err := MatchMonitorConditions(spec, c, fields); err == nil || got {
			t.Fatalf("invalid field matched: %v %v %v", fields, got, err)
		}
	}
	for _, bad := range []MonitorCondition{
		{Field: "unknown", Operator: "eq", Value: json.RawMessage(`1`)},
		{Field: "amount", Operator: "lte", Value: json.RawMessage(`"5000"`)},
		{Field: "amount", Operator: "lte", Value: json.RawMessage(`1.2`)},
		{Field: "unit", Operator: "gte", Value: json.RawMessage(`"CNY"`)},
		{Field: "amount", Operator: "exec", Value: json.RawMessage(`1`)},
	} {
		if err := ValidateMonitorConditions(spec, []MonitorCondition{bad}); err == nil {
			t.Fatal("accepted invalid condition", bad)
		}
	}
}

func TestMonitorConfigDefaultAndOverrides(t *testing.T) {
	spec := monitorSpec()
	in := CreateMonitorRequest{RequestKey: "watch", Goal: spec.Goal, Spec: &spec, Conditions: []MonitorCondition{{Field: "unit", Operator: "eq", Value: json.RawMessage(`"CNY"`)}}, EntityConditions: map[string][]MonitorCondition{"a": {{Field: "amount", Operator: "lte", Value: json.RawMessage(`5000`)}}}}
	if err := in.Normalize(); err != nil {
		t.Fatal(err)
	}
	if in.IntervalSeconds != int64((24*time.Hour)/time.Second) {
		t.Fatal("wrong default interval")
	}
	if len(in.ConditionsFor("a")) != 2 || len(in.ConditionsFor("b")) != 1 {
		t.Fatal("override discarded global conditions")
	}
	in.EntityConditions["foreign"] = in.EntityConditions["a"]
	if err := in.Normalize(); err == nil {
		t.Fatal("foreign entity accepted")
	}
	delete(in.EntityConditions, "foreign")
	in.IntervalSeconds = 0
	in.Spec.Entities = make([]string, 11)
	if err := in.Normalize(); err == nil {
		t.Fatal("unbounded monitored entities")
	}
}
