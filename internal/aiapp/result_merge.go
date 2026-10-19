package aiapp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

type MergedRecord struct {
	SchemaVersion string                  `json:"schema_version"`
	Keys          []string                `json:"keys"`
	Fields        map[string]any          `json:"fields"`
	Sources       []Provenance            `json:"sources"`
	FieldSources  map[string][]Provenance `json:"field_sources"`
}
type FieldConflict struct {
	JoinKey    string   `json:"join_key"`
	Field      string   `json:"field"`
	RecordKeys []string `json:"record_keys"`
}
type MergeResult struct {
	Records   []MergedRecord  `json:"records"`
	Conflicts []FieldConflict `json:"conflicts"`
}

// Compatible rows with an explicit matching key join. Conflicting rows
// remain separate, with all evidence; no fuzzy entity matching or silent overwrite.
func MergeRecords(records []Record, joinKeys []string) (MergeResult, error) {
	out := MergeResult{Records: []MergedRecord{}, Conflicts: []FieldConflict{}}
	groups := map[string][]int{}
	for _, r := range records {
		fields, err := json.Marshal(r.Fields)
		if err != nil {
			return out, err
		}
		key := ""
		if len(joinKeys) > 0 {
			values := []any{}
			for _, k := range joinKeys {
				v, ok := r.Fields[k]
				if !ok || v == nil {
					return out, fmt.Errorf("record %q lacks join key %q", r.Key, k)
				}
				values = append(values, v)
			}
			data, err := json.Marshal(values)
			if err != nil {
				return out, err
			}
			key = string(data)
		}
		compatible := -1
		if key != "" {
			for _, idx := range groups[key] {
				prior := out.Records[idx]
				if prior.SchemaVersion != r.SchemaVersion {
					return out, fmt.Errorf("cannot join different record schema versions")
				}
				conflicted := false
				names := make([]string, 0, len(r.Fields))
				for field := range r.Fields {
					names = append(names, field)
				}
				sort.Strings(names)
				for _, field := range names {
					old, exists := prior.Fields[field]
					if !exists {
						continue
					}
					a, _ := json.Marshal(old)
					b, _ := json.Marshal(r.Fields[field])
					if string(a) != string(b) {
						conflicted = true
						out.Conflicts = append(out.Conflicts, FieldConflict{key, field, append(append([]string{}, prior.Keys...), r.Key)})
					}
				}
				if !conflicted && compatible < 0 {
					compatible = idx
				}
			}
		}
		// Decode a copy so later caller mutations cannot change the delivered snapshot.
		copied := map[string]any{}
		decoder := json.NewDecoder(bytes.NewReader(fields))
		decoder.UseNumber()
		if err := decoder.Decode(&copied); err != nil {
			return out, err
		}
		if compatible >= 0 {
			row := &out.Records[compatible]
			row.Keys = append(row.Keys, r.Key)
			row.Sources = append(row.Sources, r.Provenance)
			for field, v := range copied {
				row.Fields[field] = v
				row.FieldSources[field] = append(row.FieldSources[field], r.Provenance)
			}
			continue
		}
		if key != "" {
			groups[key] = append(groups[key], len(out.Records))
		}
		fieldSources := map[string][]Provenance{}
		for field := range copied {
			fieldSources[field] = []Provenance{r.Provenance}
		}
		out.Records = append(out.Records, MergedRecord{SchemaVersion: r.SchemaVersion, Keys: []string{r.Key}, Fields: copied, Sources: []Provenance{r.Provenance}, FieldSources: fieldSources})
	}
	return out, nil
}
