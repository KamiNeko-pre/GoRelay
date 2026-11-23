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
type JoinIdentityIssue struct {
	JoinKey    string   `json:"join_key"`
	JoinFields []string `json:"join_fields"`
	SourceID   string   `json:"source_id"`
	Reason     string   `json:"reason"`
}
type MergeResult struct {
	IdentityIssues []JoinIdentityIssue `json:"identity_issues,omitempty"`
	Records        []MergedRecord      `json:"records"`
	Conflicts      []FieldConflict     `json:"conflicts"`
}

// Compatible rows with an explicit matching key join. Conflicting rows
// remain separate, with all evidence; no fuzzy entity matching or silent overwrite.
// MergeAvailableRecords preserves facts that cannot yet bind to the chosen
// identity. They remain separate observations with explicit identity issues;
// callers still evaluate the complete delivery contract afterward.
func MergeAvailableRecords(records []Record, joinKeys []string) (MergeResult, error) {
	bound, unbound := []Record{}, []Record{}
	issues := []JoinIdentityIssue{}
	for _, record := range records {
		if _, err := recordJoinKey(record, joinKeys); err != nil {
			unbound = append(unbound, record)
			issues = append(issues, JoinIdentityIssue{JoinFields: append([]string{}, joinKeys...), SourceID: record.Provenance.SourceID, Reason: err.Error()})
		} else {
			bound = append(bound, record)
		}
	}
	merged, err := MergeRecords(bound, joinKeys)
	if err != nil {
		return merged, err
	}
	separate, err := MergeRecords(unbound, nil)
	if err != nil {
		return merged, err
	}
	merged.Records = append(merged.Records, separate.Records...)
	merged.IdentityIssues = append(merged.IdentityIssues, issues...)
	return merged, nil
}

func MergeRecords(records []Record, joinKeys []string) (MergeResult, error) {
	out := MergeResult{Records: []MergedRecord{}, Conflicts: []FieldConflict{}}
	groups := map[string][]int{}
	// A join key is usable only if it identifies one row within a source snapshot.
	// Multiple different rows leave entity identity unresolved; never guess a new key.
	seen := map[string]map[string]map[string]any{}
	schemas := map[string]string{}
	ambiguous := map[string]bool{}
	for _, r := range records {
		key, err := recordJoinKey(r, joinKeys)
		if err != nil {
			return out, err
		}
		if prior, ok := schemas[key]; key != "" && ok && prior != r.SchemaVersion {
			return out, fmt.Errorf("cannot join different record schema versions")
		}
		schemas[key] = r.SchemaVersion
		if key == "" || r.Provenance.SourceID == "" || r.Provenance.TaskID == 0 {
			continue
		}
		observation, _ := json.Marshal([]any{r.Provenance.SourceID, r.Provenance.URL, r.Provenance.TaskID, r.Provenance.DataVersion})
		if seen[key] == nil {
			seen[key] = map[string]map[string]any{}
		}
		prior, exists := seen[key][string(observation)]
		differs := false
		for field, value := range r.Fields {
			if old, ok := prior[field]; ok {
				a, e := json.Marshal(old)
				if e != nil {
					return out, e
				}
				b, e := json.Marshal(value)
				if e != nil {
					return out, e
				}
				if string(a) != string(b) {
					differs = true
				}
			}
		}
		if exists && differs && !ambiguous[key] {
			ambiguous[key] = true
			out.IdentityIssues = append(out.IdentityIssues, JoinIdentityIssue{JoinKey: key, JoinFields: append([]string{}, joinKeys...), SourceID: r.Provenance.SourceID, Reason: "join key identifies multiple different rows in one source snapshot; retain observations separately until identity is clarified"})
		}
		combined := map[string]any{}
		for k, v := range prior {
			combined[k] = v
		}
		for k, v := range r.Fields {
			combined[k] = v
		}
		seen[key][string(observation)] = combined
	}
	for _, r := range records {
		fields, err := json.Marshal(r.Fields)
		if err != nil {
			return out, err
		}
		key, err := recordJoinKey(r, joinKeys)
		if err != nil {
			return out, err
		}
		if ambiguous[key] {
			key = ""
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

func recordJoinKey(r Record, joinKeys []string) (string, error) {
	if len(joinKeys) == 0 {
		return "", nil
	}
	values := []any{}
	for _, k := range joinKeys {
		v, ok := r.Fields[k]
		if !ok || v == nil {
			return "", fmt.Errorf("record %q lacks join key %q", r.Key, k)
		}
		values = append(values, v)
	}
	data, err := json.Marshal(values)
	return string(data), err
}
