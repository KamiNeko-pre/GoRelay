package aiapp

import (
	"encoding/json"
	"math/big"
)

func fieldIsSupported(p Provenance, field string) bool {
	e := p.FieldSupport[field]
	if e.Status != "supported" || e.CheckerVersion == "" || len(e.EvidenceRefs) == 0 {
		return false
	}
	for _, ref := range e.EvidenceRefs {
		if ref == "" {
			return false
		}
	}
	return true
}

func deliveryIdentity(spec TaskSpec, row Record) string {
	if len(spec.JoinKeys) == 0 {
		return row.Key
	}
	parts := make([]string, 0, len(spec.JoinKeys))
	for _, field := range spec.JoinKeys {
		parts = append(parts, canonicalDeliveryValue(row.Fields[field]))
	}
	raw, err := json.Marshal(parts)
	if err != nil {
		return ""
	}
	return string(raw)
}

func applyDeliveryConditions(spec TaskSpec, rows []Record, report *CoverageReport) {
	report.FieldSupportRequired = spec.RequireFieldSupport
	report.RequiredRecords = spec.MinRecords
	if report.RequiredRecords == 0 {
		report.RequiredRecords = 1
	}
	distinct := map[string]bool{}
	matches := make([]map[string]bool, len(spec.RequiredCombinations))
	for i := range matches {
		matches[i] = map[string]bool{}
	}
	for _, row := range rows {
		key := deliveryIdentity(spec, row)
		if key == "" {
			continue
		}
		distinct[key] = true
		for i, condition := range spec.RequiredCombinations {
			matched := true
			for field, value := range condition.Equals {
				if canonicalDeliveryValue(row.Fields[field]) != canonicalDeliveryValue(value) {
					matched = false
					break
				}
			}
			if matched {
				matches[i][key] = true
			}
		}
	}
	report.DistinctRecords = len(distinct)
	if len(distinct) < report.RequiredRecords {
		report.Satisfied = false
	}
	for i, condition := range spec.RequiredCombinations {
		c := ConditionCoverage{ID: condition.ID, Required: condition.MinRecords, Actual: len(matches[i]), Satisfied: len(matches[i]) >= condition.MinRecords}
		report.Combinations = append(report.Combinations, c)
		if !c.Satisfied {
			report.Satisfied = false
		}
	}
}

// Preserve JSON-number precision while treating 1, 1.0 and 1e0 equally.
func canonicalDeliveryValue(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	switch value.(type) {
	case json.Number, float64, float32, int, int64, int32, uint, uint64, uint32:
		if n, ok := new(big.Rat).SetString(string(raw)); ok {
			return "number:" + n.RatString()
		}
	}
	return string(raw)
}
