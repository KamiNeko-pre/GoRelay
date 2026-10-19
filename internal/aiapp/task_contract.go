package aiapp

import (
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"strings"
	"time"
)

type FieldRequirement struct {
	Name string `json:"name"`
	Type string `json:"type"`
}
type TaskSpec struct {
	Goal                       string             `json:"goal"`
	ContractVersion            string             `json:"contract_version,omitempty"`
	SchemaVersion              string             `json:"schema_version"`
	Entities                   []string           `json:"entities,omitempty"`
	EntityField                string             `json:"entity_field,omitempty"`
	RequiredFields             []FieldRequirement `json:"required_fields"`
	JoinKeys                   []string           `json:"join_keys,omitempty"`
	SourceNamespace            string             `json:"source_namespace,omitempty"` // Declared logical dataset, frozen with the caller's contract.
	MaxAgeSeconds              int64              `json:"max_age_seconds,omitempty"`
	SourceScope                []string           `json:"source_scope,omitempty"`
	RequireSummary             bool               `json:"require_summary,omitempty"`
	RequirePublishedCapability bool               `json:"require_published_capability,omitempty"`
	RequireExtractiveSummary   bool               `json:"require_extractive_summary,omitempty"`
	Requirements               []TaskRequirement  `json:"requirements,omitempty"`
	MinRecords                 int                `json:"min_records,omitempty"`
	RequiredCombinations       []RecordCondition  `json:"required_combinations,omitempty"`
	RequireFieldSupport        bool               `json:"require_field_support,omitempty"`
}

// Conditions contain public requirements only, never private gold values.
type RecordCondition struct {
	ID         string         `json:"id"`
	Equals     map[string]any `json:"equals"`
	MinRecords int            `json:"min_records"`
}

// Written by the host evidence checker, not accepted from Lua item fields.
// A successful URL receipt without a field check leaves this map empty.
type FieldSupport struct {
	DiagnosticCode   string        `json:"diagnostic_code,omitempty"`
	ActualLocator    *fieldLocator `json:"actual_locator,omitempty"`
	SourceDigest     string        `json:"source_digest,omitempty"`
	FullSource       bool          `json:"full_source,omitempty"`
	Status           string        `json:"status"`
	CheckerVersion   string        `json:"checker_version"`
	EvidenceRefs     []string      `json:"evidence_refs"`
	Reason           string        `json:"reason,omitempty"`
	SuggestedLocator *fieldLocator `json:"suggested_locator,omitempty"`
}
type TaskRequirement struct {
	ID          string `json:"id"`
	Description string `json:"description"`
}
type RequirementResult struct {
	ID          string   `json:"id"`
	Status      string   `json:"status"`
	Explanation string   `json:"explanation"`
	Citations   []string `json:"citations"`
}
type Provenance struct {
	SourceID     string                  `json:"source_id"`
	URL          string                  `json:"url"`
	FetchedAt    time.Time               `json:"fetched_at"`
	PublishedAt  *time.Time              `json:"published_at,omitempty"`
	DataVersion  string                  `json:"data_version"`
	TaskID       uint64                  `json:"task_id"`
	FieldSupport map[string]FieldSupport `json:"field_support,omitempty"`
}
type Record struct {
	Key           string         `json:"key"`
	SchemaVersion string         `json:"schema_version"`
	Fields        map[string]any `json:"fields"`
	Provenance    Provenance     `json:"provenance"`
}
type FieldCoverage struct {
	Field   string   `json:"field"`
	Records []string `json:"records"`
}
type RecordIssue struct {
	RecordKey string `json:"record_key"`
	Reason    string `json:"reason"`
}
type CoverageReport struct {
	Satisfied            bool                `json:"satisfied"` // Structural/coverage facts only, not semantic truth.
	ValidRecords         int                 `json:"valid_records"`
	PartialValidRecords  int                 `json:"partial_valid_records"`
	Fields               []FieldCoverage     `json:"fields"`
	MissingEntities      []string            `json:"missing_entities"`
	Issues               []RecordIssue       `json:"issues"`
	DistinctRecords      int                 `json:"distinct_records"`
	RequiredRecords      int                 `json:"required_records"`
	Combinations         []ConditionCoverage `json:"combinations,omitempty"`
	FieldSupportRequired bool                `json:"field_support_required"`
}

type ConditionCoverage struct {
	ID        string `json:"id"`
	Required  int    `json:"required"`
	Actual    int    `json:"actual"`
	Satisfied bool   `json:"satisfied"`
}
type TaskResult struct {
	AcquiredMaterials    []map[string]any    `json:"acquired_materials,omitempty"` // Unanalysed source references, never accepted records.
	CapabilityRequired   bool                `json:"capability_required,omitempty"`
	ReusableCapabilities int                 `json:"reusable_capabilities,omitempty"`
	ScopeConfirmation    string              `json:"scope_confirmation,omitempty"`
	Records              []MergedRecord      `json:"records"`
	Coverage             CoverageReport      `json:"coverage"`
	Conflicts            []FieldConflict     `json:"conflicts"`
	Summary              string              `json:"summary,omitempty"`
	Gaps                 []string            `json:"gaps,omitempty"`
	Limitations          []string            `json:"limitations,omitempty"`
	ReuseIssues          []string            `json:"reuse_issues,omitempty"`
	SummaryRequired      bool                `json:"summary_required,omitempty"`
	Answer               *RAGAnswer          `json:"answer,omitempty"`
	Requirements         []RequirementResult `json:"requirements,omitempty"`
}

func taskDeliverySatisfied(result *TaskResult) bool {
	return result != nil && result.ScopeConfirmation != "unconfirmed" && result.ScopeConfirmation != "awaiting_confirmation" && result.Coverage.Satisfied && (!result.CapabilityRequired || result.ReusableCapabilities > 0) && (!result.SummaryRequired || (result.Answer != nil && result.Answer.Status == "answered"))
}

func (s TaskSpec) Validate() error {
	if len(s.SourceNamespace) > 128 || strings.TrimSpace(s.SourceNamespace) != s.SourceNamespace {
		return fmt.Errorf("source_namespace must be a trimmed logical dataset name of at most 128 bytes")
	}
	if strings.TrimSpace(s.Goal) == "" {
		return fmt.Errorf("goal is required")
	}
	if len(s.RequiredFields) == 0 {
		return fmt.Errorf("required fields are required")
	}
	if s.RequireExtractiveSummary && !s.RequireSummary {
		return fmt.Errorf("extractive summary requires a cited summary")
	}
	if s.MaxAgeSeconds < 0 {
		return fmt.Errorf("freshness cannot be negative")
	}
	if s.ContractVersion != "" && s.ContractVersion != "v1" && s.ContractVersion != "v2" {
		return fmt.Errorf("unsupported task contract version %q", s.ContractVersion)
	}
	if s.MinRecords < 0 {
		return fmt.Errorf("minimum records cannot be negative")
	}
	if (s.MinRecords > 0 || len(s.RequiredCombinations) > 0 || s.RequireFieldSupport) && s.ContractVersion != "v2" {
		return fmt.Errorf("delivery conditions require task contract v2")
	}
	if s.MinRecords > 1 && len(s.JoinKeys) == 0 {
		return fmt.Errorf("multiple distinct records require explicit business join keys")
	}
	ids := map[string]bool{}
	for _, requirement := range s.Requirements {
		if requirement.ID == "" || strings.TrimSpace(requirement.Description) == "" || ids[requirement.ID] {
			return fmt.Errorf("requirements need unique IDs and descriptions")
		}
		ids[requirement.ID] = true
	}
	known := map[string]string{}
	for _, f := range s.RequiredFields {
		if strings.TrimSpace(f.Name) == "" || known[f.Name] != "" {
			return fmt.Errorf("empty or duplicate required field")
		}
		switch f.Type {
		case "string", "source_text", "number", "integer", "boolean", "string_array":
		default:
			return fmt.Errorf("unsupported field type %q", f.Type)
		}
		known[f.Name] = f.Type
	}
	if len(s.Entities) > 0 && known[s.EntityField] != "string" {
		return fmt.Errorf("entity field must be a required string field")
	}
	for _, k := range s.JoinKeys {
		if known[k] == "" {
			return fmt.Errorf("join key %q is not a required field", k)
		}
	}
	conditionIDs := map[string]bool{}
	for _, condition := range s.RequiredCombinations {
		if strings.TrimSpace(condition.ID) == "" || conditionIDs[condition.ID] || len(condition.Equals) == 0 || condition.MinRecords < 1 {
			return fmt.Errorf("conditions require unique IDs, field matches and positive minimums")
		}
		conditionIDs[condition.ID] = true
		if condition.MinRecords > 1 && len(s.JoinKeys) == 0 {
			return fmt.Errorf("condition %s requires explicit business join keys", condition.ID)
		}
		for field, value := range condition.Equals {
			if known[field] == "" || !fieldMatches(value, known[field]) {
				return fmt.Errorf("condition %s has unknown field or invalid value: %s", condition.ID, field)
			}
		}
	}
	return nil
}

func fieldMatches(v any, kind string) bool {
	switch kind {
	case "string", "source_text":
		s, ok := v.(string)
		return ok && strings.TrimSpace(s) != ""
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "number", "integer":
		var n float64
		switch x := v.(type) {
		case float64:
			n = x
		case int:
			n = float64(x)
		case int64:
			n = float64(x)
		case json.Number:
			var e error
			n, e = x.Float64()
			if e != nil {
				return false
			}
		default:
			return false
		}
		return !math.IsNaN(n) && !math.IsInf(n, 0) && (kind != "integer" || math.Trunc(n) == n)
	case "string_array":
		switch a := v.(type) {
		case []string:
			for _, s := range a {
				if strings.TrimSpace(s) == "" {
					return false
				}
			}
			return true
		case []any:
			for _, s := range a {
				if !fieldMatches(s, "string") {
					return false
				}
			}
			return true
		}
	}
	return false
}

func EvaluateRecords(spec TaskSpec, records []Record, now time.Time) (CoverageReport, error) {
	report := CoverageReport{Fields: []FieldCoverage{}, MissingEntities: []string{}, Issues: []RecordIssue{}}
	if err := spec.Validate(); err != nil {
		return report, err
	}
	entities := map[string]bool{}
	covered := map[string][]string{}
	accepted := []Record{}
	for _, r := range records {
		reason := ""
		u, e := url.Parse(r.Provenance.URL)
		switch {
		case r.Key == "" || r.SchemaVersion != spec.SchemaVersion:
			reason = "missing record key or schema version mismatch"
		case e != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") || r.Provenance.SourceID == "" || r.Provenance.DataVersion == "" || r.Provenance.TaskID == 0 || r.Provenance.FetchedAt.IsZero():
			reason = "incomplete acquisition provenance"
		case r.Provenance.FetchedAt.After(now):
			reason = "acquisition timestamp is in the future"
		case spec.MaxAgeSeconds > 0 && now.Sub(r.Provenance.FetchedAt).Seconds() > float64(spec.MaxAgeSeconds):
			reason = "stale acquisition"
		}
		for _, f := range spec.RequiredFields {
			if reason == "" && !fieldMatches(r.Fields[f.Name], f.Type) {
				reason = "missing or invalid field: " + f.Name
			}
			if reason == "" && spec.RequireFieldSupport && !fieldIsSupported(r.Provenance, f.Name) {
				reason = "unsupported field evidence: " + f.Name
			}
			if reason == "" && f.Type == "source_text" && (!fieldIsSupported(r.Provenance, f.Name) || !r.Provenance.FieldSupport[f.Name].FullSource) {
				reason = "incomplete fetched source text: " + f.Name
			}
		}
		if reason != "" {
			report.Issues = append(report.Issues, RecordIssue{r.Key, reason})
			continue
		}
		report.ValidRecords++
		accepted = append(accepted, r)
		for _, f := range spec.RequiredFields {
			covered[f.Name] = append(covered[f.Name], r.Key)
		}
		if s, ok := r.Fields[spec.EntityField].(string); ok {
			entities[s] = true
		}
	}
	report.Satisfied = report.ValidRecords > 0
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
	// A run that acquired real, contract-valid records but did not satisfy every
	// requirement has delivered something, and the host must say so. Without this
	// the partial counter stayed zero forever, so the terminal status collapsed to
	// "failed" even when valid records existed, and honest partial delivery was
	// reported to the user as total failure.
	if !report.Satisfied && report.ValidRecords > 0 {
		report.PartialValidRecords = report.ValidRecords
	}
	return report, nil
}
