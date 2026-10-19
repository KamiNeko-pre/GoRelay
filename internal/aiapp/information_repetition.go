package aiapp

import (
	"encoding/json"
	"go-lua-crawler/internal/agentdata"
	"strings"
)

// This is exact content novelty, NOT an embedding similarity or entropy score.
// Compare material across batches; cursor spelling and batch grouping are not
// information. Diagnostics are advisory: rereading after eviction can be valid.
func repeatedReadInformation(ops []agentdata.Operation) map[string]any {
	seen := map[string]string{}
	var repeated []map[string]string
	newMaterial, hasRead := false, false
	for _, op := range ops {
		if op.Kind == "model" {
			repeated = nil
			newMaterial, hasRead = false, false
			continue
		}
		if op.Kind != "read_data" {
			continue
		}
		var out ToolResult
		var data map[string]any
		if op.Stage != "succeeded" || json.Unmarshal([]byte(op.ResultJSON), &out) != nil || out.Error != "" || json.Unmarshal(out.Data, &data) != nil {
			continue
		}
		// Only located textual views have a known distinction between content
		// and paging metadata. Arbitrary JSON may have meaningful offset fields.
		text, ok := data["text"].(string)
		if !ok || text == "" || data["view_version"] == nil {
			continue
		}
		hasRead = true
		// Identity is the resource and the content, never the call that produced
		// it.
		//
		// A handle is an opaque pointer minted per fetch, so keying on it made
		// every read of the same page through a fresh handle look like new
		// material: a measured real run returned 22 duplicate contents across 58
		// reads and this check never fired, because the model re-inspected the
		// same URL and received a new handle each time.
		//
		// The URL is the resource's stable identity, so it replaces the handle
		// rather than being dropped: the same page read twice is a repeat, while
		// a different page that happens to share wording is genuinely new
		// material. The selector and pointer still identify which part of the
		// resource was asked for, so a different view of one page stays distinct.
		key := agentdata.Hash([]any{data["url"], data["view_version"], data["selector"], data["pointer"], strings.Join(strings.Fields(text), " ")})
		if original, exists := seen[key]; exists {
			repeated = append(repeated, map[string]string{"current_receipt": out.CallID, "previous_receipt": original})
		} else {
			seen[key] = out.CallID
			newMaterial = true
		}
	}
	if !hasRead || newMaterial || len(repeated) == 0 {
		return nil
	}
	return map[string]any{"signal": "unchanged_read_information", "repeated_receipts": repeated, "instruction": "These reads returned material already obtained, despite different paging arguments or batch grouping. Use the retained evidence to address the remaining task requirements. If necessary facts were evicted from context, recover that specific material and record the resulting conclusion; do not restart the whole investigation. This signal alone does not prohibit legitimate rereading or establish task completion."}
}
