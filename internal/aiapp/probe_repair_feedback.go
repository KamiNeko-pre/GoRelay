package aiapp

import (
	"encoding/json"
	"fmt"
	"strings"

	"go-lua-crawler/internal/engine"
)

func probeObservedSources(result engine.ProgramResult) []map[string]any {
	out := []map[string]any{}
	seen := map[string]bool{}
	for _, source := range result.SourceEvidence {
		if !observedCrawlerSource(source) || seen[source.Receipt.URL] {
			continue
		}
		seen[source.Receipt.URL] = true
		out = append(out, map[string]any{"source_url": source.Receipt.URL, "source_verified": true, "source_structure": crawlerSourceStructure(source), "inspect_with": map[string]any{"url": source.Receipt.URL}})
	}
	return out
}

type probeFieldDiagnostic struct {
	Row            int      `json:"row_index"`
	SourceURL      string   `json:"source_url,omitempty"`
	Missing        []string `json:"missing_fields"`
	SourceVerified bool     `json:"source_verified"`
	Structure      any      `json:"source_structure,omitempty"`
	Instruction    string   `json:"instruction"`
}

// A successful probe is still an intermediate observation. Diagnose every row,
// using actual runner GET evidence for source structure, never a printed URL or
// a model-proposed field value. Completeness is independent of preview length.
func probeFieldDiagnostics(spec TaskSpec, result engine.ProgramResult) []probeFieldDiagnostic {
	items, _ := result.Collection["items"].([]any)
	diagnostics := []probeFieldDiagnostic{}
	for index, row := range items {
		item, _ := row.(map[string]any)
		missing := []string{}
		for _, field := range spec.RequiredFields {
			value, ok := item[field.Name]
			if !ok || value == nil || strings.TrimSpace(fmt.Sprint(value)) == "" {
				missing = append(missing, field.Name)
			}
		}
		if len(missing) == 0 {
			continue
		}
		d := probeFieldDiagnostic{Row: index, Missing: missing, Instruction: "Repair these missing fields using observed source selectors/JSON paths; preserve working navigation and other fields. Include each field's _evidence locator, then validate mode=acquisition and follow publication/submission steps. Do not repeat identical probe arguments."}
		d.SourceURL, _ = item["source_url"].(string)
		for _, source := range result.SourceEvidence {
			if observedCrawlerSource(source) && (source.Receipt.URL == d.SourceURL || source.Receipt.RequestedURL == d.SourceURL) {
				d.SourceVerified = true
				d.Structure = crawlerSourceStructure(source)
				break
			}
		}
		if !d.SourceVerified {
			d.Instruction = "This row has no corresponding successful GET in this probe. Use the actual fetched source_url and observed source structure before repairing missing fields; printed values and URLs do not establish evidence. Then validate mode=acquisition with real _evidence locators."
		}
		diagnostics = append(diagnostics, d)
	}
	return diagnostics
}

// Only the latest validation owns the next program repair. A subsequent
// acquisition diagnosis supersedes an old probe rather than reviving it.
func latestProbeRepair(state LoopState) map[string]any {
	for i := len(state.Operations) - 1; i >= 0; i-- {
		op := state.Operations[i]
		if op.RunID != state.Run.ID || op.Kind != "validate_program" || op.Stage == "reserved" {
			continue
		}
		var out ToolResult
		var data struct {
			Mode        string                 `json:"mode"`
			Diagnostics []probeFieldDiagnostic `json:"field_diagnostics"`
			Next        map[string]any         `json:"next_action"`
		}
		if op.Stage != "succeeded" || json.Unmarshal([]byte(op.ResultJSON), &out) != nil || out.Error != "" || out.ReplayOf != "" || json.Unmarshal(out.Data, &data) != nil || data.Mode != "probe" || len(data.Diagnostics) == 0 {
			return nil
		}
		return map[string]any{"probe_call_id": out.CallID, "field_diagnostics": data.Diagnostics, "next_action": data.Next}
	}
	return nil
}
