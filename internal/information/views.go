package information

import (
	"encoding/json"
	"math/big"
	"reflect"
	"sort"
	"strings"
)

type ViewSpec struct {
	ObservationIDs []string          `json:"observation_ids"`
	Columns        []string          `json:"columns"`
	JoinFields     []string          `json:"join_fields,omitempty"`
	Filters        []ViewFilter      `json:"filters,omitempty"`
	Sort           []ViewSort        `json:"sort,omitempty"`
	Resolutions    []FieldResolution `json:"resolutions,omitempty"`
}
type ViewFilter struct {
	Field string `json:"field"`
	Op    string `json:"op"`
	Value any    `json:"value,omitempty"`
}
type ViewSort struct {
	Field     string `json:"field"`
	Direction string `json:"direction"`
}
type FieldResolution struct {
	ObservationIDs        []string `json:"observation_ids"`
	Field                 string   `json:"field"`
	SelectedObservationID string   `json:"selected_observation_id"`
}
type ValueCandidate struct {
	Value          any      `json:"value"`
	ObservationIDs []string `json:"observation_ids"`
}
type ViewDiagnostic struct {
	Code           string           `json:"code"`
	Field          string           `json:"field"`
	ObservationIDs []string         `json:"observation_ids"`
	Candidates     []ValueCandidate `json:"candidates,omitempty"`
	Message        string           `json:"message"`
}
type DeliveryTable struct {
	Title             string           `json:"title,omitempty"`
	Columns           []string         `json:"columns,omitempty"`
	Rows              []map[string]any `json:"rows"`
	RowObservationIDs [][]string       `json:"row_observation_ids,omitempty"`
	Diagnostics       []ViewDiagnostic `json:"diagnostics,omitempty"`
	JoinFields        []string         `json:"join_fields,omitempty"`
}

func (t *DeliveryTable) UnmarshalJSON(data []byte) error {
	type Alias DeliveryTable
	var raw struct {
		Alias
		RawRowObs json.RawMessage `json:"row_observation_ids"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*t = DeliveryTable(raw.Alias)
	if len(raw.RawRowObs) > 0 {
		var twoD [][]string
		if err := json.Unmarshal(raw.RawRowObs, &twoD); err == nil {
			t.RowObservationIDs = twoD
		} else {
			var oneD []string
			if err := json.Unmarshal(raw.RawRowObs, &oneD); err == nil {
				if len(oneD) > 0 {
					t.RowObservationIDs = [][]string{oneD}
				}
			}
		}
	}
	return nil
}

func viewNumber(value any) (*big.Rat, bool) {
	var raw string
	if s, ok := value.(string); ok {
		raw = s
	} else {
		switch value.(type) {
		case json.Number, float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
			b, err := json.Marshal(value)
			if err != nil {
				return nil, false
			}
			raw = string(b)
		default:
			return nil, false
		}
	}
	// Rat also accepts fractions and hexadecimal values; business numbers do not.
	if !json.Valid([]byte(raw)) || len(raw) == 0 || strings.ContainsAny(raw, "/xX\"[]{}") {
		return nil, false
	}
	return new(big.Rat).SetString(raw)
}
func viewCompare(a, b any) (int, bool) {
	if a == nil || b == nil {
		return 0, false
	}
	if an, ok := viewNumber(a); ok {
		if bn, valid := viewNumber(b); valid {
			return an.Cmp(bn), true
		}
	}
	if as, ok := a.(string); ok {
		if bs, valid := b.(string); valid {
			if strings.EqualFold(strings.TrimSpace(as), strings.TrimSpace(bs)) {
				return 0, true
			}
			return strings.Compare(as, bs), true
		}
	}
	if reflect.DeepEqual(a, b) {
		return 0, true
	}
	return 0, false
}
func viewMatch(value any, f ViewFilter) (bool, bool) {
	if f.Op == "exists" {
		return value != nil, true
	}
	if value == nil {
		return false, false
	}
	if f.Op == "contains" {
		a, ok := value.(string)
		b, valid := f.Value.(string)
		return strings.Contains(a, b), ok && valid
	}
	if f.Op == "lt" || f.Op == "lte" || f.Op == "gt" || f.Op == "gte" {
		a, ok := viewNumber(value)
		b, valid := viewNumber(f.Value)
		if !ok || !valid {
			return false, false
		}
		c := a.Cmp(b)
		switch f.Op {
		case "lt":
			return c < 0, true
		case "lte":
			return c <= 0, true
		case "gt":
			return c > 0, true
		default:
			return c >= 0, true
		}
	}
	c, ok := viewCompare(value, f.Value)
	if !ok {
		return false, false
	}
	if f.Op == "ne" {
		return c != 0, true
	}
	return c == 0, true
}

// projectObservations merges before filtering, and never modifies its inputs.
func projectObservations(observations []Observation, spec ViewSpec) (DeliveryTable, error) {
	table := DeliveryTable{Columns: append([]string(nil), spec.Columns...), Rows: []map[string]any{}, RowObservationIDs: [][]string{}, JoinFields: append([]string(nil), spec.JoinFields...)}
	for i := range spec.Filters {
		op := strings.ToLower(strings.TrimSpace(spec.Filters[i].Op))
		switch op {
		case "==", "=":
			op = "eq"
		case "!=":
			op = "ne"
		case "<":
			op = "lt"
		case "<=":
			op = "lte"
		case ">":
			op = "gt"
		case ">=":
			op = "gte"
		}
		spec.Filters[i].Op = op
		switch op {
		case "eq", "ne", "lt", "lte", "gt", "gte", "contains", "exists":
		default:
			return table, businessError("INVALID_FILTER", "views", "不支持的筛选操作", "使用 eq/ne/lt/lte/gt/gte/contains/exists")
		}
	}
	for i := range spec.Sort {
		dir := strings.ToLower(strings.TrimSpace(spec.Sort[i].Direction))
		if dir == "" || dir == "ascending" {
			dir = "asc"
		} else if dir == "descending" {
			dir = "desc"
		}
		spec.Sort[i].Direction = dir
		if dir != "asc" && dir != "desc" {
			return table, businessError("INVALID_SORT", "views", "排序方向无效", "使用 asc 或 desc")
		}
	}
	byID := map[string]Observation{}
	for _, o := range observations {
		byID[o.ID] = o
	}
	groups := [][]Observation{}
	identities := map[string]int{}
	seen := map[string]bool{}
	for _, id := range spec.ObservationIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		o, ok := byID[id]
		if !ok {
			return table, businessError("OBSERVATION_NOT_LINKED", "views", "所选事实不在当前任务中", "查询当前任务事实，或先显式关联有权使用的资料")
		}
		values := []any{}
		complete := len(spec.JoinFields) > 0
		for _, field := range spec.JoinFields {
			value := o.Fields[field]
			if value == nil || reflect.DeepEqual(value, "") {
				complete = false
			}
			values = append(values, value)
		}
		if complete {
			keyBytes, err := json.Marshal(values)
			if err != nil {
				return table, err
			}
			key := string(keyBytes)
			if index, exists := identities[key]; exists {
				groups[index] = append(groups[index], o)
				continue
			}
			identities[key] = len(groups)
		}
		groups = append(groups, []Observation{o})
	}
	for _, group := range groups {
		ids := []string{}
		candidates := map[string][]ValueCandidate{}
		for _, o := range group {
			ids = append(ids, o.ID)
			for field, value := range o.Fields {
				if value == nil {
					continue
				}
				matched := false
				for i := range candidates[field] {
					if reflect.DeepEqual(candidates[field][i].Value, value) {
						candidates[field][i].ObservationIDs = append(candidates[field][i].ObservationIDs, o.ID)
						matched = true
						break
					}
				}
				if !matched {
					candidates[field] = append(candidates[field], ValueCandidate{Value: value, ObservationIDs: []string{o.ID}})
				}
			}
		}
		row := map[string]any{}
		for field, values := range candidates {
			if len(values) == 1 {
				row[field] = values[0].Value
				continue
			}
			row[field] = nil
			for _, resolution := range spec.Resolutions {
				if resolution.Field != field || !sameViewIDs(resolution.ObservationIDs, ids) {
					continue
				}
				selected, ok := byID[resolution.SelectedObservationID]
				if !ok || !containsViewID(ids, selected.ID) || selected.Fields[field] == nil {
					return table, businessError("INVALID_RESOLUTION", "views", "冲突选择未指向同组已有字段", "从冲突诊断列出的观测中选择已有字段值")
				}
				row[field] = selected.Fields[field]
			}
			table.Diagnostics = append(table.Diagnostics, ViewDiagnostic{Code: "FIELD_CONFLICT", Field: field, ObservationIDs: ids, Candidates: values, Message: "同一实体存在不同字段值，原始事实全部保留"})
		}
		keep := true
		for _, f := range spec.Filters {
			match, known := viewMatch(row[f.Field], f)
			if known && !match {
				keep = false
				break
			}
			if !known {
				table.Diagnostics = append(table.Diagnostics, ViewDiagnostic{Code: "FILTER_UNKNOWN", Field: f.Field, ObservationIDs: ids, Message: "字段缺失、冲突或类型不适用，保留待核实记录"})
			}
		}
		if !keep {
			continue
		}
		output := map[string]any{}
		for _, field := range spec.Columns {
			output[field] = row[field]
		}
		table.Rows = append(table.Rows, output)
		table.RowObservationIDs = append(table.RowObservationIDs, ids)
	}
	// Keep row and provenance arrays paired through stable sorting.
	order := make([]int, len(table.Rows))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool {
		a, b := table.Rows[order[i]], table.Rows[order[j]]
		for _, s := range spec.Sort {
			av, bv := a[s.Field], b[s.Field]
			if av == nil && bv != nil {
				return false
			}
			if av != nil && bv == nil {
				return true
			}
			c, known := viewCompare(av, bv)
			if known && c != 0 {
				if s.Direction == "desc" {
					return c > 0
				}
				return c < 0
			}
		}
		return false
	})
	rows := table.Rows
	ids := table.RowObservationIDs
	table.Rows = nil
	table.RowObservationIDs = nil
	for _, i := range order {
		table.Rows = append(table.Rows, rows[i])
		table.RowObservationIDs = append(table.RowObservationIDs, ids[i])
	}
	return table, nil
}
func containsViewID(ids []string, id string) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}
func sameViewIDs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]string(nil), a...)
	y := append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	return reflect.DeepEqual(x, y)
}
