package aiapp

import (
	"encoding/json"
	"sort"

	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/engine"
)

func observedCrawlerSource(source acquisition.Response) bool {
	r := source.Receipt
	return r.Method == "GET" && r.Status >= 200 && r.Status < 300 && r.URL != "" && !r.FetchedAt.IsZero() && source.Body != ""
}

// The same crawler projection used in tool feedback. URL, handle, timestamp,
// prose and model-written plans are not new structural evidence.
func crawlerSourceStructure(source acquisition.Response) any {
	if structure := crawlerJSONInspection(source.Body); structure != nil {
		return map[string]any{"json_structure": structure, "json_nodes": crawlerJSONSourceNodes(source.Body)}
	}
	return map[string]any{"selector_catalog": crawlerCatalog(source.Body, completeSourceSelectorCatalog(source.Body))}
}

func correctionEvidenceKeys(state LoopState, op agentdata.Operation) []string {
	if op.RunID != state.Run.ID || op.Stage != "succeeded" && op.Stage != "failed" {
		return nil
	}
	var out ToolResult
	if json.Unmarshal([]byte(op.ResultJSON), &out) != nil || out.ReplayOf != "" {
		return nil
	}
	if op.Stage == "failed" {
		if out.Failure != nil && out.Failure.Recovery != nil && out.Failure.Recovery.Instruction != "" && out.Failure.Recovery.Strategy != "stop" && out.Failure.Recovery.Strategy != "inspect_or_stop" {
			return []string{structuralFailureKey(op.Kind, out)}
		}
		return nil
	}
	if out.Error != "" {
		return nil
	}
	switch op.Kind {
	case "inspect_source":
		var page sourceInspection
		if json.Unmarshal(out.Data, &page) == nil && observedCrawlerSource(page.Source) {
			return []string{"structure:" + agentdata.Hash(crawlerSourceStructure(page.Source))}
		}
	case "read_data":
		var data struct {
			Handle  string            `json:"handle"`
			Nodes   []json.RawMessage `json:"nodes"`
			Catalog []json.RawMessage `json:"selector_catalog"`
			JSON    json.RawMessage   `json:"json_structure"`
		}
		if json.Unmarshal(out.Data, &data) != nil {
			return nil
		}
		_, _, page := readResource(state.Operations, data.Handle, true)
		if page == nil || !observedCrawlerSource(page.Source) {
			return nil
		}
		keys := []string{}
		for _, node := range append(data.Nodes, data.Catalog...) {
			var value map[string]any
			if json.Unmarshal(node, &value) == nil {
				delete(value, "index") // Position in a selected batch is not a new fact.
				if raw, ok := value["raw_attributes"]; ok {
					value["attributes"] = raw
				}
				if links, ok := value["links"].([]any); ok {
					for _, link := range links {
						if fields, ok := link.(map[string]any); ok {
							if raw, ok := fields["raw_href"]; ok {
								fields["href"] = raw
							}
						}
					}
				}
				keys = append(keys, "node:"+agentdata.Hash(value))
			}
		}
		if len(data.JSON) > 0 && string(data.JSON) != "null" {
			var value any
			if json.Unmarshal(data.JSON, &value) == nil {
				keys = append(keys, "structure:"+agentdata.Hash(map[string]any{"json_structure": value}))
			}
		}
		return keys
	case "validate_program":
		var data struct {
			Mode                 string                 `json:"mode"`
			ExecutionDiagnostics []engine.DOMDiagnostic `json:"execution_diagnostics"`
			Sources              []struct {
				Verified  bool            `json:"source_verified"`
				Structure json.RawMessage `json:"source_structure"`
			} `json:"observed_sources"`
			Diagnostics []struct {
				SourceVerified bool            `json:"source_verified"`
				Structure      json.RawMessage `json:"source_structure"`
				Missing        []string        `json:"missing_fields"`
			} `json:"field_diagnostics"`
		}
		if json.Unmarshal(out.Data, &data) != nil || data.Mode != "probe" {
			return nil
		}
		keys := []string{}
		for _, diagnostic := range data.ExecutionDiagnostics {
			if diagnostic.SourceDigest != "" && diagnostic.Code != "" {
				diagnostic.Instruction = "" // Wording is not a new runtime fact.
				keys = append(keys, "execution-diagnostic:"+agentdata.Hash(diagnostic))
			}
		}
		for _, source := range data.Sources {
			var structure any
			if source.Verified && json.Unmarshal(source.Structure, &structure) == nil {
				keys = append(keys, "structure:"+agentdata.Hash(structure))
			}
		}
		for _, diagnostic := range data.Diagnostics {
			if !diagnostic.SourceVerified || len(diagnostic.Structure) == 0 {
				continue
			}
			var structure any
			if json.Unmarshal(diagnostic.Structure, &structure) != nil {
				continue
			}
			keys = append(keys, "structure:"+agentdata.Hash(structure))
			sort.Strings(diagnostic.Missing)
			if len(diagnostic.Missing) > 0 {
				keys = append(keys, "probe-missing:"+agentdata.Hash([]any{structure, diagnostic.Missing}))
			}
		}
		return keys
	}
	return nil
}

// Compare against all earlier host receipts, not just visible messages. A new
// diagnosis is usable once; replay, renamed references and compaction add none.
func newCorrectionEvidence(state LoopState, modelID uint64) bool {
	seen := map[string]bool{}
	for _, op := range state.Operations {
		for _, key := range correctionEvidenceKeys(state, op) {
			if op.ID > modelID && !seen[key] {
				return true
			}
			seen[key] = true
		}
	}
	return false
}

func correctionBehavior(state LoopState, model agentdata.Operation) string {
	var response ChatResponse
	if json.Unmarshal([]byte(model.ResultJSON), &response) != nil {
		return ""
	}
	keys := []string{}
	for _, call := range response.Message.ToolCalls {
		key := businessToolInput(call.Function, state.Operations)
		for _, op := range state.Operations {
			if op.Key != "tool:"+call.ID {
				continue
			}
			var out ToolResult
			if json.Unmarshal([]byte(op.ResultJSON), &out) != nil {
				break
			}
			if op.Stage == "failed" && out.Failure != nil && out.ReplayOf == "" {
				key = structuralFailureKey(call.Function.Name, out)
				if identities := fieldFailureIdentities(out.Failure, true); len(identities) > 0 {
					key += ":action:" + agentdata.Hash(identities)
				}
			}
			if op.Kind == "inspect_source" && op.Stage == "succeeded" {
				var page sourceInspection
				if json.Unmarshal(out.Data, &page) == nil && observedCrawlerSource(page.Source) {
					key = "inspect:" + agentdata.Hash(crawlerSourceStructure(page.Source))
				}
			}
			if op.Stage == "succeeded" && out.Error == "" && (op.Kind == "read_data" || op.Kind == "validate_program") {
				var data struct {
					Mode string `json:"mode"`
				}
				json.Unmarshal(out.Data, &data)
				if op.Kind == "read_data" || data.Mode == "probe" {
					facts := correctionEvidenceKeys(state, op)
					sort.Strings(facts)
					// Code spelling or a different reader alias cannot turn the
					// same source/diagnostic outcome into a different method.
					key = op.Kind + ":" + agentdata.Hash(facts)
				}
			}
			if op.Kind == "update_task_progress" || op.Kind == "read_task_context" || op.Kind == "evaluate_result" || op.Kind == "get_task_results" {
				key = op.Kind
			}
			break
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return agentdata.Hash(keys)
}

func repeatedCorrectionBehavior(state LoopState, modelID uint64) bool {
	var last agentdata.Operation
	for _, op := range state.Operations {
		if op.ID == modelID && op.Kind == "model" {
			last = op
			break
		}
	}
	behavior := correctionBehavior(state, last)
	if behavior == "" {
		return false
	}
	for _, op := range state.Operations {
		if op.Kind != "model" || op.Stage != "succeeded" || op.ID >= modelID {
			continue
		}
		var response ChatResponse
		if json.Unmarshal([]byte(op.ResultJSON), &response) == nil && response.DeliveryRecoveryPhase == "correcting" && correctionBehavior(state, op) == behavior {
			return true
		}
	}
	return false
}
