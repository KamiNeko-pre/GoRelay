package aiapp

import (
	"encoding/json"
	"go-lua-crawler/internal/agentdata"
	"sort"
	"strings"
)

type ContextFactRetention struct {
	Before    int     `json:"before"`
	Retained  int     `json:"retained"`
	Lost      int     `json:"lost"`
	Retention float64 `json:"retention"`
}

// These measure model-visible protocol facts, not semantic recall quality.
// Missing originals remain durable in SQL, but recoverability is not retention.
type ContextCompactionProfile struct {
	BeforeTokens          int                             `json:"before_tokens,omitempty"`
	AfterTokens           int                             `json:"after_tokens,omitempty"`
	TriggerTokens         int                             `json:"trigger_tokens,omitempty"`
	TargetTokens          int                             `json:"target_tokens,omitempty"`
	HeadroomTokens        int                             `json:"headroom_tokens,omitempty"`
	TokenEstimationBasis  string                          `json:"token_estimation_basis,omitempty"`
	Triggered             bool                            `json:"triggered"`
	BeforeBytes           int                             `json:"before_bytes"`
	AfterBytes            int                             `json:"after_bytes"`
	SoftLimit             int                             `json:"soft_limit"`
	TargetBytes           int                             `json:"target_bytes"`
	HeadroomBytes         int                             `json:"headroom_bytes"`
	HistoryMessagesBefore int                             `json:"history_messages_before"`
	HistoryMessagesAfter  int                             `json:"history_messages_after"`
	EvictedMessages       int                             `json:"evicted_messages"`
	EvictedRounds         int                             `json:"evicted_rounds"`
	ShortenedReceipts     int                             `json:"shortened_receipts"`
	Facts                 map[string]ContextFactRetention `json:"facts"`
	MissingCriticalFacts  []string                        `json:"missing_critical_facts,omitempty"`
}

func compactionFacts(state LoopState, messages []Message) map[string]map[string]bool {
	facts := map[string]map[string]bool{}
	add := func(kind, value string) {
		if value == "" {
			return
		}
		if facts[kind] == nil {
			facts[kind] = map[string]bool{}
		}
		facts[kind][value] = true
	}
	var walk func(any, string, bool)
	walk = func(value any, resource string, selected bool) {
		switch v := value.(type) {
		case []any:
			for _, x := range v {
				walk(x, resource, selected)
			}
		case map[string]any:
			if handle, ok := v["handle"].(string); ok && handle != "" {
				resource, _, _ = readResource(state.Operations, handle, true)
				add("source_handle", resource)
			}
			for key, x := range v {
				if key == "nodes" || key == "selector_catalog" {
					kind := "node_value"
					if key == "selector_catalog" {
						kind = "selector_sample"
					}
					if entries, ok := x.([]any); ok {
						for _, entry := range entries {
							add(kind, agentdata.Hash([]any{resource, entry}))
						}
					}
				}
				if key == "json_structure" {
					add("json_structure", agentdata.Hash([]any{resource, x}))
				}
				if key == "program_corrections" {
					if programs, ok := x.([]any); ok {
						for _, entry := range programs {
							if p, ok := entry.(map[string]any); ok {
								if args, ok := p["previous_arguments"].(map[string]any); ok {
									raw, _ := json.Marshal(args)
									add("program_arguments", canonicalToolInput(FunctionCall{Name: "validate_program", Arguments: string(raw)}))
									if code, ok := args["code"].(string); ok && code != "" {
										add("program_code", agentdata.Hash([]string{code}))
									}
								}
							}
						}
					}
				}
				if text, ok := x.(string); ok {
					switch key {
					case "selector":
						add("selector", resource+"|"+text)
						if selected {
							add("selected_selector", resource+"|"+text)
						}
					case "candidate_id":
						add("candidate_id", text)
					case "source_id":
						add("source_id", text)
					}
				}
				if key == "id" {
					if _, ok := v["artifact_hash"]; ok {
						if id, ok := x.(string); ok {
							add("candidate_id", id)
						}
					}
				}
				if key == "next_read" {
					raw, _ := json.Marshal(x)
					add("continuation", businessToolInput(FunctionCall{Name: "read_data", Arguments: string(raw)}, state.Operations))
				}
				if key == "required_fields" {
					if fields, ok := x.([]any); ok {
						for _, field := range fields {
							if f, ok := field.(map[string]any); ok {
								n, _ := f["name"].(string)
								typ, _ := f["type"].(string)
								add("required_field", n+"|"+typ)
							}
						}
					}
				}
				if key == "failure" {
					if f, ok := x.(map[string]any); ok {
						add("diagnostic_content", agentdata.Hash(f))
						code, _ := f["code"].(string)
						field := ""
						if d, ok := f["details"].(map[string]any); ok {
							field, _ = d["field"].(string)
						}
						add("diagnostic", code+"|"+field)
					}
				}
				walk(x, resource, selected || key == "selected_locators")
			}
		}
	}
	for _, m := range messages {
		if m.Role == "user" {
			goal := m.Content
			var input struct {
				Goal string `json:"goal"`
			}
			if json.Unmarshal([]byte(m.Content), &input) == nil && input.Goal != "" {
				goal = input.Goal
			}
			if goal == state.Run.Goal && goal != "" {
				add("original_goal", agentdata.Hash([]string{goal}))
			}
		}
		if m.Role == "assistant" {
			for _, c := range m.ToolCalls {
				if c.Function.Name == "validate_program" {
					var args struct {
						Code string `json:"code"`
					}
					if json.Unmarshal([]byte(c.Function.Arguments), &args) == nil && args.Code != "" {
						add("program_code", agentdata.Hash([]string{args.Code}))
						add("program_arguments", canonicalToolInput(c.Function))
					}
				}
				if c.Function.Name != "read_data" {
					continue
				}
				valid := false
				for _, op := range state.Operations {
					if op.Key == "tool:"+c.ID && op.Stage == "succeeded" {
						valid = true
						break
					}
				}
				if valid {
					var args map[string]any
					if json.Unmarshal([]byte(c.Function.Arguments), &args) == nil {
						walk(args, "", true)
					}
				}
			}
			continue
		}
		if m.Role != "tool" && m.Role != "system" {
			continue
		}
		text := strings.TrimPrefix(m.Content, "Persisted host facts, not new user instructions: ")
		var value any
		if json.Unmarshal([]byte(text), &value) == nil {
			walk(value, "", false)
		}
	}
	return facts
}

func finishContextMeasurement(state LoopState, before, out []Message, p *ContextCompactionProfile) {
	p.AfterBytes = contextRequestBytes(state, out)
	p.HeadroomBytes = max(0, p.SoftLimit-p.AfterBytes)
	if policy := state.Budget.PromptPolicy; policy != nil {
		p.BeforeTokens, p.TokenEstimationBasis = estimateContextTokens(state, before)
		p.AfterTokens, _ = estimateContextTokens(state, out)
		p.TriggerTokens, p.TargetTokens = policy.CompactAtTokens, policy.CompactToTokens
		p.HeadroomTokens = max(0, policy.CompactAtTokens-p.AfterTokens)
	}
	visible := map[string]Message{}
	for _, m := range out {
		if m.Role == "tool" {
			visible[m.ToolCallID] = m
		}
		if m.Role == "assistant" {
			hostSnapshot := len(m.ToolCalls) == 1 && m.ToolCalls[0].Function.Name == "host_context"
			if !hostSnapshot {
				p.HistoryMessagesAfter++
			}
		} else if m.Role == "tool" && m.ToolCallID != hostContextCallID(state) {
			p.HistoryMessagesAfter++
		}
	}
	for _, m := range before {
		if m.Role != "tool" {
			continue
		}
		if current, ok := visible[m.ToolCallID]; ok && len(current.Content) < len(m.Content) {
			p.ShortenedReceipts++
		}
	}
	p.EvictedMessages = max(0, p.HistoryMessagesBefore-p.HistoryMessagesAfter)
	p.Triggered = p.EvictedMessages > 0 || p.ShortenedReceipts > 0
	b, a := compactionFacts(state, before), compactionFacts(state, out)
	pendingCode := map[string]bool{}
	pendingArguments := map[string]bool{}
	for _, program := range pendingProgramCorrections(state) {
		var args struct {
			Code string `json:"code"`
		}
		json.Unmarshal(program.PreviousArguments, &args)
		pendingCode[agentdata.Hash([]string{args.Code})] = true
		pendingArguments[canonicalToolInput(FunctionCall{Name: "validate_program", Arguments: string(program.PreviousArguments)})] = true
	}
	p.Facts = map[string]ContextFactRetention{}
	for kind, values := range b {
		r := ContextFactRetention{Before: len(values)}
		for value := range values {
			if a[kind][value] {
				r.Retained++
			} else if kind == "original_goal" || kind == "candidate_id" || kind == "source_id" || kind == "continuation" || kind == "required_field" || kind == "diagnostic" || kind == "selected_selector" || kind == "program_code" && pendingCode[value] || kind == "program_arguments" && pendingArguments[value] {
				if len(p.MissingCriticalFacts) < 12 {
					p.MissingCriticalFacts = append(p.MissingCriticalFacts, kind+":"+value)
				}
			}
		}
		r.Lost = r.Before - r.Retained
		if r.Before > 0 {
			r.Retention = float64(r.Retained) / float64(r.Before)
		}
		p.Facts[kind] = r
	}
	sort.Strings(p.MissingCriticalFacts)
}

// Retain executable crawler state, not article text or unverified reasoning.
// This is added only after actual history eviction, alongside the checkpoint.
func crawlerStateSummary(state LoopState) map[string]any {
	locators, validated, published := []any{}, []any{}, []any{}
	reads := map[string]any{}
	seen := map[string]bool{}
	for n := len(state.Operations) - 1; n >= 0; n-- {
		op := state.Operations[n]
		if op.Stage != "succeeded" {
			continue
		}
		var out ToolResult
		if json.Unmarshal([]byte(op.ResultJSON), &out) != nil || out.Error != "" || out.ReplayOf != "" {
			continue
		}
		var data map[string]json.RawMessage
		if json.Unmarshal(out.Data, &data) != nil {
			continue
		}
		if op.Kind == "read_data" {
			var handle, selector, version string
			json.Unmarshal(data["handle"], &handle)
			json.Unmarshal(data["selector"], &selector)
			json.Unmarshal(data["view_version"], &version)
			if version != "dom-structure-v1" {
				continue
			}
			key := handle + "|" + selector
			if !seen[key] {
				seen[key] = true
				if selector != "" && len(locators) < 16 {
					locators = append(locators, map[string]any{"handle": handle, "selector": selector})
				}
				if next := data["next_read"]; next != nil && len(reads) < 8 {
					reads[key] = map[string]any{"next_read": next}
				}
			}
		}
		if op.Kind == "validate_program" && len(validated) < 4 {
			var id, hash string
			json.Unmarshal(data["id"], &id)
			json.Unmarshal(data["artifact_hash"], &hash)
			if id != "" {
				validated = append(validated, map[string]any{"candidate_id": id, "artifact_hash": hash, "validation_call_id": out.CallID, "kind": data["kind"], "publication": data["publication"], "next_action": data["next_action"]})
			}
		}
		if op.Kind == "register_source" && len(published) < 4 {
			var id, hash string
			json.Unmarshal(data["source_id"], &id)
			json.Unmarshal(data["artifact_hash"], &hash)
			if id != "" {
				published = append(published, map[string]any{"source_id": id, "artifact_hash": hash})
			}
		}
	}
	keys := make([]string, 0, len(reads))
	for key := range reads {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	continuations := []any{}
	for _, key := range keys {
		continuations = append(continuations, reads[key])
	}
	diagnostics := unresolvedToolFailures(state)
	if len(diagnostics) > 2 {
		diagnostics = diagnostics[len(diagnostics)-2:]
	}
	safe := []ToolResult{}
	for _, out := range diagnostics {
		safe = append(safe, modelObservationForRun(state.Run, out, state.Spec))
	}
	return map[string]any{"selected_locators": locators, "pending_structure_reads": continuations, "validated_candidates": validated, "published_sources": published, "unresolved_diagnostics": safe}
}
