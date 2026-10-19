package aiapp

import "time"

// Check each field against its own acquired sources. A fresh name/version may
// not make an unrelated stale license current. Conflicts remain explicit gaps.
func EvaluateMergedRecords(spec TaskSpec, merged MergeResult, now time.Time) (CoverageReport, error) {
	report := CoverageReport{Fields: []FieldCoverage{}, MissingEntities: []string{}, Issues: []RecordIssue{}}
	if err := spec.Validate(); err != nil {
		return report, err
	}
	covered := map[string][]string{}
	entities := map[string]bool{}
	accepted := []Record{}
	for _, row := range merged.Records {
		key := ""
		if len(row.Keys) > 0 {
			key = row.Keys[0]
		}
		valid := true
		anyValid := false
		for _, field := range spec.RequiredFields {
			fieldSpec := spec
			fieldSpec.Entities = nil
			fieldSpec.EntityField = ""
			fieldSpec.JoinKeys = nil
			fieldSpec.MinRecords = 0
			fieldSpec.RequiredCombinations = nil
			fieldSpec.RequiredFields = []FieldRequirement{field}
			found := false
			for _, source := range row.FieldSources[field.Name] {
				facts, err := EvaluateRecords(fieldSpec, []Record{{Key: key, SchemaVersion: row.SchemaVersion, Fields: row.Fields, Provenance: source}}, now)
				if err != nil {
					return report, err
				}
				if facts.ValidRecords == 1 {
					found = true
					break
				}
			}
			if !found {
				valid = false
				report.Issues = append(report.Issues, RecordIssue{key, "missing, invalid or stale evidence for field: " + field.Name})
			}
			if found {
				anyValid = true
				covered[field.Name] = append(covered[field.Name], key)
			}
		}
		if anyValid {
			report.PartialValidRecords++
		}
		if valid {
			report.ValidRecords++
			accepted = append(accepted, Record{Key: key, Fields: row.Fields})
			if entity, ok := row.Fields[spec.EntityField].(string); ok {
				entities[entity] = true
			}
		}
	}
	report.Satisfied = report.ValidRecords > 0 && len(merged.Conflicts) == 0
	for _, f := range spec.RequiredFields {
		keys := covered[f.Name]
		if keys == nil {
			keys = []string{}
		}
		report.Fields = append(report.Fields, FieldCoverage{f.Name, keys})
		if len(keys) == 0 {
			report.Satisfied = false
		}
	}
	for _, entity := range spec.Entities {
		if !entities[entity] {
			report.MissingEntities = append(report.MissingEntities, entity)
			report.Satisfied = false
		}
	}
	applyDeliveryConditions(spec, accepted, &report)
	return report, nil
}
