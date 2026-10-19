package aiapp

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"go-lua-crawler/internal/agentdata"
)

// Only old, durable successful replies become references. Recent rounds and
// errors remain exact; originals live in the operation ledger, not this view.
func compactConsumedHistory(state LoopState, history []Message) []Message {
	out := append([]Message{}, history...)
	starts := []int{}
	for i, m := range history {
		if m.Role == "assistant" {
			starts = append(starts, i)
		}
	}
	if len(starts) <= 2 {
		return out
	}
	cutoff := starts[len(starts)-2]
	for n := 0; n < cutoff; n++ {
		m := out[n]
		if m.Role != "tool" {
			continue
		}
		var receipt ToolResult
		if json.Unmarshal([]byte(m.Content), &receipt) != nil || receipt.CallID != m.ToolCallID || receipt.Error != "" {
			continue
		}
		if !assessedHistoryRound(state, []Message{m}) {
			continue
		}
		view := consumedReceiptSummary(receipt)
		raw, _ := json.Marshal(view)
		out[n].Content = string(raw)
	}
	return out
}

func consumedReceiptSummary(receipt ToolResult) ToolResult {
	if receipt.Error != "" || len(receipt.Data) <= 1200 {
		return receipt
	}
	var data map[string]json.RawMessage
	if json.Unmarshal(receipt.Data, &data) != nil {
		return receipt
	}
	summary := map[string]any{"compacted": true, "receipt_handle": receipt.CallID, "read_with": map[string]any{"handle": receipt.CallID, "view": "json"}}
	// Preserve unknown structure by default. Only known bulk prose/record
	// fields are replaced below; an allowlist silently discarded DOM nodes,
	// observed links and JSON structure, forcing the model to rediscover them.
	for key, value := range data {
		summary[key] = value
	}
	var text string
	if json.Unmarshal(data["text"], &text) == nil && text != "" {
		delete(summary, "text")
		summary["text_preview"] = fitObservationText(text, 500)
	}
	var source struct {
		Receipt json.RawMessage `json:"receipt"`
		Body    string          `json:"body"`
	}
	if json.Unmarshal(data["source"], &source) == nil && len(source.Receipt) > 0 {
		summary["source"] = map[string]any{"receipt": source.Receipt, "text_preview": fitObservationText(source.Body, 500)}
		if value := data["selector_catalog"]; value != nil {
			summary["selector_catalog"] = value
		}
	}
	var records []struct {
		Fields map[string]json.RawMessage `json:"fields"`
	}
	if json.Unmarshal(data["records"], &records) == nil && len(records) > 0 {
		rows := []map[string]any{}
		for i, row := range records {
			if i >= 4 {
				break
			}
			fields := map[string]any{}
			for name, value := range row.Fields {
				if len(value) <= 400 {
					fields[name] = value
					continue
				}
				fields[name] = map[string]any{"stored_bytes": len(value), "read_with": map[string]any{"handle": receipt.CallID, "view": "json", "pointer": fmt.Sprintf("/records/%d/fields/%s", i, pointerKey(name))}}
			}
			rows = append(rows, map[string]any{"fields": fields})
		}
		delete(summary, "records")
		summary["record_count"], summary["record_summaries"] = len(records), rows
	}
	summary["available_fields"] = sortedMapKeys(data)
	receipt.Data, _ = json.Marshal(summary)
	return receipt
}

func sortedMapKeys(data map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(data))
	for key := range data {
		keys = append(keys, key)
	}
	// Reuse the canonical JSON ordering through the simple standard-library sort.
	sort.Strings(keys)
	return keys
}

func savedHistoryCheckpoint(state LoopState, messages []Message) map[string]json.RawMessage {
	for n := len(messages) - 1; n >= 0; n-- {
		m := messages[n]
		if m.Role != "tool" || m.ToolCallID != hostContextCallID(state) {
			continue
		}
		var snapshot struct {
			Checkpoint map[string]json.RawMessage `json:"history_checkpoint"`
		}
		if json.Unmarshal([]byte(m.Content), &snapshot) == nil {
			return snapshot.Checkpoint
		}
	}
	return nil
}

func historyCheckpoint(messages []Message, previous map[string]json.RawMessage, crawler ...bool) any {
	indexOnly := len(crawler) > 0 && crawler[0]
	notes := []map[string]any{}
	previousCount := 0
	if previous != nil {
		_ = json.Unmarshal(previous["recent_outcomes"], &notes)
		_ = json.Unmarshal(previous["evicted_messages"], &previousCount)
	}
	if indexOnly {
		// The crawler-state projection owns executable facts and recent feedback
		// owns diagnostics. This checkpoint is only an index, not a third copy
		// of model prose, next_action schemas or verbose next_step instructions.
		kept := []map[string]any{}
		for _, note := range notes {
			if note["tool"] != nil {
				kept = append(kept, map[string]any{"tool": note["tool"], "handle": note["handle"]})
			}
		}
		notes = kept
	}
	for _, m := range messages {
		if !indexOnly && m.Role == "assistant" && strings.TrimSpace(m.Content) != "" {
			notes = append(notes, map[string]any{"model_note_unverified": fitObservationText(m.Content, 700)})
		}
		if m.Role != "tool" {
			continue
		}
		var receipt ToolResult
		if json.Unmarshal([]byte(m.Content), &receipt) != nil || receipt.CallID != m.ToolCallID {
			continue
		}
		if indexOnly {
			notes = append(notes, map[string]any{"tool": receipt.Name, "handle": receipt.CallID})
			continue
		}
		entry := map[string]any{"tool": receipt.Name, "handle": receipt.CallID, "read_with": map[string]any{"handle": receipt.CallID, "view": "json"}}
		if receipt.Error != "" {
			entry["error"] = receipt.Error
		} else {
			// The checkpoint is an outcome index, not another observation window.
			// Rich page/catalog previews already have their own persisted readers.
			var data map[string]json.RawMessage
			if json.Unmarshal(receipt.Data, &data) == nil {
				outcome := map[string]json.RawMessage{}
				for _, key := range []string{"task_id", "candidate_id", "source_id", "artifact_hash", "status", "record_count", "produced_fields", "next_action", "next_step", "read_with", "next_read"} {
					if value := data[key]; value != nil {
						outcome[key] = value
					}
				}
				var source struct {
					Receipt struct {
						URL string `json:"url"`
					} `json:"receipt"`
				}
				if json.Unmarshal(data["source"], &source) == nil && source.Receipt.URL != "" {
					outcome["url"], _ = json.Marshal(source.Receipt.URL)
				}
				entry["outcome"] = outcome
			}
		}
		notes = append(notes, entry)
	}
	if len(notes) > 8 {
		notes = notes[len(notes)-8:]
	}
	hint := "Persisted operation receipts; use read_data/list_sources/get_task_results to recover. Model notes are unverified."
	if indexOnly {
		hint = "Use crawler_state for selected locators, pending structure reads and capability IDs; view=receipt restores saved diagnostics. No business action is repeated."
	}
	return map[string]any{"evicted_messages": previousCount + len(messages), "recent_outcomes": notes, "older_originals": hint}
}

type PromptContextProfile struct {
	PromptHash        string                    `json:"prompt_hash"`
	ToolSchemaHash    string                    `json:"tool_schema_hash"`
	Compaction        *ContextCompactionProfile `json:"compaction,omitempty"`
	RequestBytes      int                       `json:"request_bytes"`
	ToolSchemaBytes   int                       `json:"tool_schema_bytes"`
	StablePrefixBytes int                       `json:"stable_prefix_bytes"`
	HostSnapshotBytes int                       `json:"host_snapshot_bytes"`
}

func promptContextProfile(request ChatRequest) *PromptContextProfile {
	raw, _ := json.Marshal(request)
	tools, _ := json.Marshal(request.Tools)
	profile := &PromptContextProfile{PromptHash: agentdata.Hash(request.Messages), ToolSchemaHash: agentdata.Hash(request.Tools), RequestBytes: len(raw), ToolSchemaBytes: len(tools)}
	prefix := true
	for _, m := range request.Messages {
		encoded, _ := json.Marshal(m)
		if m.Role == "system" && strings.HasPrefix(m.Content, "Persisted host facts") {
			prefix = false
		}
		if prefix {
			profile.StablePrefixBytes += len(encoded)
		}
		if m.Role == "system" && strings.HasPrefix(m.Content, "Persisted host facts") || m.Role == "tool" && strings.HasPrefix(m.ToolCallID, "host-context-") {
			profile.HostSnapshotBytes += len(encoded)
		}
	}
	return profile
}
