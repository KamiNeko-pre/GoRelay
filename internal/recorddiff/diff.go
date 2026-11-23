package recorddiff

import (
	"encoding/json"
	"fmt"
	"sort"
)

// Join fields identify the business entity, never runtime IDs or row order.
func BusinessKey(row map[string]any, fields []string) (string, error) {
	if len(fields) == 0 {
		return "", fmt.Errorf("business key fields required")
	}
	values := []any{}
	for _, field := range fields {
		value, ok := row[field]
		if !ok || value == nil {
			return "", fmt.Errorf("business key missing: %s", field)
		}
		switch v := value.(type) {
		case string:
			if v == "" {
				return "", fmt.Errorf("business key empty: %s", field)
			}
		case float64, int, int64, json.Number, bool:
		default:
			return "", fmt.Errorf("business key must be scalar: %s", field)
		}
		values = append(values, value)
	}
	raw, err := json.Marshal(values)
	return string(raw), err
}

type BusinessChange struct {
	Key    string `json:"key"`
	Kind   string `json:"kind"`
	Field  string `json:"field,omitempty"`
	Before any    `json:"before"`
	After  any    `json:"after"`
}

func Diff(before, after []map[string]any, keys []string) ([]BusinessChange, error) {
	index := func(rows []map[string]any) (map[string]map[string]any, error) {
		out := map[string]map[string]any{}
		for _, row := range rows {
			key, err := BusinessKey(row, keys)
			if err != nil {
				return nil, err
			}
			if _, ok := out[key]; ok {
				return nil, fmt.Errorf("duplicate business identity: %s", key)
			}
			out[key] = row
		}
		return out, nil
	}
	a, err := index(before)
	if err != nil {
		return nil, err
	}
	b, err := index(after)
	if err != nil {
		return nil, err
	}
	identities := map[string]bool{}
	for key := range a {
		identities[key] = true
	}
	for key := range b {
		identities[key] = true
	}
	ordered := []string{}
	for key := range identities {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)
	out := []BusinessChange{}
	for _, key := range ordered {
		old, had := a[key]
		current, has := b[key]
		if !had {
			out = append(out, BusinessChange{Key: key, Kind: "record_added", After: current})
			continue
		}
		if !has {
			out = append(out, BusinessChange{Key: key, Kind: "record_removed", Before: old})
			continue
		}
		fields := map[string]bool{}
		for field := range old {
			fields[field] = true
		}
		for field := range current {
			fields[field] = true
		}
		names := []string{}
		for field := range fields {
			names = append(names, field)
		}
		sort.Strings(names)
		for _, field := range names {
			x, xok := old[field]
			y, yok := current[field]
			if xok && yok && equalValue(x, y) {
				continue
			}
			kind := "field_changed"
			if !xok {
				kind = "field_added"
			}
			if !yok {
				kind = "field_removed"
			}
			out = append(out, BusinessChange{Key: key, Kind: kind, Field: field, Before: x, After: y})
		}
	}
	return out, nil
}

func equalValue(a, b any) bool {
	x, xe := json.Marshal(a)
	y, ye := json.Marshal(b)
	return xe == nil && ye == nil && string(x) == string(y)
}
