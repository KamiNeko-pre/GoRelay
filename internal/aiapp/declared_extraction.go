package aiapp

import (
	"encoding/json"
	"fmt"
	"go-lua-crawler/internal/acquisition"
	"sort"
)

// Declarations select original values, never literals or semantic verdicts.
// Publication still uses the existing frozen contract and source support gate.
type extractionSelector struct {
	fieldLocator
	SourceText bool `json:"source_text,omitempty"`
}

func extractDeclaredFields(source acquisition.Response, selectors map[string]extractionSelector, spec TaskSpec, observed ...acquisition.Response) ([]any, error) {
	if len(selectors) == 0 || len(selectors) > 16 {
		return nil, fmt.Errorf("field_selectors requires 1..16 requested fields")
	}
	item := map[string]any{}
	proof := map[string]any{}
	keys := make([]string, 0, len(selectors))
	for name := range selectors {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	for _, name := range keys {
		loc := selectors[name]
		fieldSource := source
		if loc.SourceURL != "" {
			matched := false
			for _, candidate := range append([]acquisition.Response{source}, observed...) {
				if observedCrawlerSource(candidate) && (candidate.Receipt.URL == loc.SourceURL || candidate.Receipt.RequestedURL == loc.SourceURL) {
					fieldSource = candidate
					matched = true
					break
				}
			}
			if !matched {
				return nil, toolContractFailure(fmt.Errorf("field %s source_url has no captured successful response", name), "FIELD_EXTRACTION_ERROR", "not_started", map[string]any{"field": name, "source_url": loc.SourceURL, "diagnostic_code": "SOURCE_NOT_OBSERVED"}, ToolRecovery{Strategy: "choose_observed_field_source", Tool: "read_data", Instruction: "Choose the source_url of an existing successful inspection in this Run and its observed locator; source_url does not grant a new fetch or access."})
			}
		}
		var value any
		var err error
		if loc.SourceText {
			if loc.Selector != "" || loc.JSONPointer != nil || loc.JSONKeyPointer != nil || loc.Attribute != "" || loc.Pattern != "" || loc.Index != 0 {
				return nil, fmt.Errorf("field %s: source_text cannot mix with a DOM/JSON locator", name)
			}
			value, err = acquisition.SourceText(fieldSource.Body)
		} else {
			value, _, err = evidenceValue(fieldSource, loc.fieldLocator)
		}
		if err != nil {
			return nil, toolContractFailure(fmt.Errorf("field %s: %w", name, err), "FIELD_EXTRACTION_ERROR", "not_started",
				map[string]any{"field": name, "actual_locator": loc, "source_url": fieldSource.Receipt.URL},
				ToolRecovery{Strategy: "inspect_and_correct", Tool: "read_data", Instruction: "Inspect the persisted source structure and correct this field's locator; do not substitute a literal value."})
		}
		item[name] = value
		if loc.SourceText && loc.SourceURL != "" {
			proof[name] = map[string]any{"source_url": fieldSource.Receipt.URL}
		}
		if !loc.SourceText {
			raw, _ := json.Marshal(loc.fieldLocator)
			var p any
			_ = json.Unmarshal(raw, &p)
			proof[name] = p
		}
	}
	item["_evidence"] = proof
	return []any{item}, nil
}
