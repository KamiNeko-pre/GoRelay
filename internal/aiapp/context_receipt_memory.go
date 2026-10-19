package aiapp

import (
	"encoding/json"
	"sort"
	"strings"
)

// Recent success traffic must not evict an unresolved diagnostic. These are
// historical failures, not proof that a changed proposal will fail again.
func unresolvedToolFailures(state LoopState) []ToolResult {
	pending := map[string]ToolResult{}
	receipts := map[string]ToolResult{}
	proposals := map[string]FunctionCall{}
	for _, op := range state.Operations {
		if op.RunID != state.Run.ID || op.Kind != "model" || op.Stage != "succeeded" {
			continue
		}
		var response ChatResponse
		if json.Unmarshal([]byte(op.ResultJSON), &response) == nil {
			for _, call := range response.Message.ToolCalls {
				proposals[call.ID] = call.Function
			}
		}
	}
	for _, op := range state.Operations {
		if op.RunID != state.Run.ID || !strings.HasPrefix(op.Key, "tool:") {
			continue
		}
		var reply ToolResult
		if json.Unmarshal([]byte(op.ResultJSON), &reply) != nil || reply.CallID != strings.TrimPrefix(op.Key, "tool:") || reply.Name != op.Kind {
			continue
		}
		receipts[reply.CallID] = reply
		key := op.Kind + ":" + op.InputHash
		if op.InputHash == "" {
			key = op.Key
		}
		origin := reply
		visitedOrigin := map[string]bool{}
		for origin.ReplayOf != "" && !visitedOrigin[origin.CallID] {
			visitedOrigin[origin.CallID] = true
			prior, exists := receipts[strings.TrimPrefix(origin.ReplayOf, "tool:")]
			if !exists {
				break
			}
			origin = prior
		}
		if repairMetadataRejection(origin.Failure) {
			// Old records used a raw or a business hash for protocol-only
			// rejection. Rebuild its diagnostic identity from the durable
			// proposal, without rewriting SQL or re-admitting any business work.
			if proposal, ok := proposals[origin.CallID]; ok {
				key = op.Kind + ":" + repairRejectionIdentity(businessToolInput(proposal, state.Operations))
			}
		}
		if reply.ReplayOf == "" && !repairMetadataRejection(reply.Failure) {
			// A corrected protocol envelope reached business admission. Its
			// fresh success OR business diagnostic supersedes that metadata
			// rejection; retaining it would send the model back to a fixed error.
			delete(pending, op.Kind+":"+repairRejectionIdentity(op.InputHash))
		}
		if op.Stage == "succeeded" && reply.Error == "" {
			delete(pending, key)
			// A verified cross-tool stage can make an earlier diagnosis obsolete.
			// Keep unrelated sources and unchanged state unresolved.
			for oldKey, failure := range pending {
				if failure.Failure == nil || failure.Failure.Recovery == nil || reply.ReplayOf != "" {
					continue
				}
				condition := failure.Failure.Recovery.ResolvedBy
				if condition == nil || condition.Tool != reply.Name || len(condition.Fields) == 0 {
					continue
				}
				var data map[string]any
				if json.Unmarshal(reply.Data, &data) != nil {
					continue
				}
				matches := true
				for field, expected := range condition.Fields {
					if expected == "" || data[field] != expected {
						matches = false
						break
					}
				}
				if matches {
					delete(pending, oldKey)
				}
			}
			// Follow only validated, persisted repair links. A successful
			// descendant resolves the earlier failed attempts in this chain.
			visited := map[string]bool{}
			for id := reply.RepairOf; id != "" && !visited[id]; {
				visited[id] = true
				for oldKey, failure := range pending {
					if failure.CallID == id {
						delete(pending, oldKey)
					}
				}
				id = receipts[id].RepairOf
			}
			continue
		}
		if reply.Error != "" {
			pending[key] = reply
		}
	}
	keys := make([]string, 0, len(pending))
	for key := range pending {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]ToolResult, 0, len(keys))
	for _, key := range keys {
		out = append(out, pending[key])
	}
	return out
}

// Recovery memory replaces evicted replies; it is not a second copy of replies
// already present in this request. Recompute after every history reduction.
func contextFeedbackForHistory(data []byte, history []Message) []byte {
	var facts map[string]json.RawMessage
	if json.Unmarshal(data, &facts) != nil {
		return data
	}
	visible := map[string]bool{}
	for _, message := range history {
		if message.Role != "tool" {
			continue
		}
		var receipt ToolResult
		if json.Unmarshal([]byte(message.Content), &receipt) == nil && receipt.CallID != "" && receipt.CallID == message.ToolCallID {
			visible[receipt.CallID] = true
		}
	}
	// One owner for all fallback memories: an exact receipt already present in
	// history must not be repeated in either recovery channel. If evicted later,
	// it becomes visible again from the durable receipt on the next projection.
	for _, key := range []string{"unresolved_tool_failures", "recent_tool_feedback"} {
		var feedback []ToolResult
		if json.Unmarshal(facts[key], &feedback) != nil {
			continue
		}
		kept := []ToolResult{}
		for _, receipt := range feedback {
			if !visible[receipt.CallID] {
				kept = append(kept, receipt)
				visible[receipt.CallID] = true
			}
		}
		facts[key], _ = json.Marshal(kept)
	}
	raw, _ := json.Marshal(facts)
	return raw
}

// Historical feedback is data, not a new instruction or completion claim.
// SQL receipts provide bounded recent distinct proposals when old rounds leave
// the prompt. Original receipts remain readable; execution checks stay current.
func recentToolFeedback(state LoopState) []ToolResult {
	out := []ToolResult{}
	seen := map[string]bool{}
	bytes := 0
	contextManaged := crawlerRun(state.Run) && state.Budget.PromptPolicy != nil
	for i := len(state.Operations) - 1; i >= 0 && len(out) < 8; i-- {
		op := state.Operations[i]
		identity := op.Kind + ":" + op.InputHash
		if op.InputHash == "" {
			identity = op.Key
		}
		if op.RunID != state.Run.ID || !strings.HasPrefix(op.Key, "tool:") || (op.Stage != "succeeded" && op.Stage != "failed") || seen[identity] {
			continue
		}
		var receipt ToolResult
		if json.Unmarshal([]byte(op.ResultJSON), &receipt) != nil || receipt.CallID != strings.TrimPrefix(op.Key, "tool:") || receipt.Name != op.Kind {
			continue
		}
		seen[identity] = true
		if sourceReaderName(receipt.Name) {
			var proposal ToolCall
			for _, message := range state.Messages {
				for _, call := range message.ToolCalls {
					if call.ID == receipt.CallID {
						proposal = call
					}
				}
			}
			receipt = historyReaderView(state, receipt, proposal)
		}
		if crawlerRun(state.Run) {
			receipt = modelObservationForRun(state.Run, receipt, state.Spec)
		}
		if !contextManaged && sourceReaderName(receipt.Name) {
			receipt = shortReaderFeedback(receipt)
		} else if !contextManaged {
			if isFetchedInspection(receipt) {
				receipt = shortReaderFeedback(modelObservationForRun(state.Run, receipt, state.Spec))
			} else {
				receipt = observationWithinBudget(receipt, 2<<10, state.Spec)
			}
		}
		raw, _ := json.Marshal(receipt)
		if !contextManaged && bytes+len(raw) > 12<<10 {
			continue
		}
		bytes += len(raw)
		out = append(out, receipt)
	}
	return out
}

// Acquired pages are reusable inputs, not just the latest outcome of a tool.
// Keep their identities when round text is evicted; the original receipt stays
// behind read_data. No source prose becomes a system instruction.
func acquiredPageReferences(state LoopState) []map[string]any {
	out := []map[string]any{}
	seen := map[string]bool{}
	for i := len(state.Operations) - 1; i >= 0 && len(out) < 16; i-- {
		op := state.Operations[i]
		if op.RunID != state.Run.ID || op.Kind != "inspect_source" || op.Stage != "succeeded" {
			continue
		}
		var result ToolResult
		var page sourceInspection
		if json.Unmarshal([]byte(op.ResultJSON), &result) != nil || op.Key != "tool:"+result.CallID || json.Unmarshal(result.Data, &page) != nil {
			continue
		}
		url := page.Source.Receipt.URL
		if url == "" || seen[url] || page.Source.Receipt.Method != "GET" || page.Source.Receipt.Status < 200 || page.Source.Receipt.Status >= 300 {
			continue
		}
		seen[url] = true
		label := []rune(url)
		// Name every read this handle supports, not just the receipt view. A
		// caller handed only view=receipt has no stated way to reach the page
		// text, so it guesses a handle or a view and the call comes back
		// "valid handle and bounded read required" even though the handle it was
		// given is correct. The recorded run hit that rejection twice right after
		// reading these references.
		out = append(out, map[string]any{
			"handle":              result.CallID,
			"url":                 string(label[:min(512, len(label))]),
			"url_truncated":       len(label) > 512,
			"read_structure_with": map[string]any{"handle": result.CallID, "view": "dom"},
			"read_receipt_with":   map[string]any{"handle": result.CallID, "view": "receipt"},
			// This string is the host telling the caller how to read the handle it
			// just handed over. It named view=content, which read_data now refuses,
			// so following the host's own advice produced a rejected call. It names
			// the two reads that work: the selector inventory (view=dom with no
			// selector) and its matched nodes, plus the listing reader.
			"reads": "view=dom lists observed selectors or matched nodes with bounded field samples, own attributes and nested links. Its continuation pages structure, never article text.",
		})
	}
	return out
}
