package aiapp

import (
	"encoding/json"
	"fmt"
	"strings"

	"go-lua-crawler/internal/agentdata"
)

func businessRepairTarget(fn FunctionCall, ops []agentdata.Operation) string {
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(fn.Arguments), &fields) != nil {
		return ""
	}
	for _, key := range []string{"handle", "inspection_call_id", "tool_call_id"} {
		var handle string
		if json.Unmarshal(fields[key], &handle) != nil || handle == "" {
			continue
		}
		identity, _, _ := readResource(ops, handle, true)
		fields[key], _ = json.Marshal(identity)
	}
	raw, _ := json.Marshal(fields)
	fn.Arguments = string(raw)
	return toolRepairTarget(fn)
}

// References are protocol addresses, not business identities. Resolve them from
// this Run's committed receipts before reserving work. No model IDs, wall clock
// or fetch timestamp enters a resolved source identity.
func businessToolInput(fn FunctionCall, ops []agentdata.Operation) string {
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(fn.Arguments), &fields) != nil {
		return canonicalToolInput(fn)
	}
	// Protocol metadata is excluded before typed argument decoding as well as
	// hashing. Otherwise read_data rejects repair_of as an unknown field here
	// and skips source/default normalization only for rejected envelopes.
	delete(fields, "repair_of")
	stripped, _ := json.Marshal(fields)
	fn.Arguments = string(stripped)
	if fn.Name == "read_data" {
		var a dataReadArgs
		if decodeArgs(fn.Arguments, &a) != nil {
			return canonicalToolInput(fn)
		}
		resolved, err := resolveDataReadArgs(a, ops)
		if err != nil {
			return canonicalToolInput(fn)
		}
		a = resolved
		// Reading /text from a read receipt is a slice of the same source text,
		// not a new resource created by the model's latest handle.
		a = sourceTextReadIdentity(a, ops)
		identity, _, page := readResource(ops, a.Handle, a.View == "dom" || a.View == "content")
		if page != nil {
			if page.JSONStructure != nil && (a.View == "json" || a.View == "dom" || a.View == "") {
				a.View = "json"
				if a.Limit == 0 {
					a.Limit = 64
				}
				if a.ViewVersion == "json-structure-v1" {
					a.ViewVersion = ""
				}
			}
			if a.View == "" && len(a.Fields) == 0 {
				a.View = "dom"
			}
			if a.View == "dom" {
				if a.Limit == 0 {
					a.Limit = 64
				}
				if a.ViewVersion == "dom-structure-v1" {
					a.ViewVersion = ""
				}
			}
			if a.View == "content" {
				if a.Limit == 0 {
					a.Limit = defaultReadRuness
				}
				if a.ViewVersion == htmlInformationVersion || a.ViewVersion == "plain-text-v1" {
					a.ViewVersion = ""
				}
			}
		}
		a.Handle = identity
		a.RunID = ""
		a.ReceiptID = 0
		raw, _ := json.Marshal(a)
		fn.Arguments = string(raw)
		return canonicalToolInput(fn)
	}
	for _, field := range []string{"inspection_call_id", "tool_call_id", "handle"} {
		var handle string
		if json.Unmarshal(fields[field], &handle) != nil || handle == "" {
			continue
		}
		identity, _, _ := readResource(ops, handle, false)
		fields[field], _ = json.Marshal(identity)
	}
	raw, _ := json.Marshal(fields)
	fn.Arguments = string(raw)
	return canonicalToolInput(fn)
}

func sourceTextReadIdentity(a dataReadArgs, ops []agentdata.Operation) dataReadArgs {
	byKey := map[string]agentdata.Operation{}
	for _, op := range ops {
		byKey[op.Key] = op
	}
	seen := map[string]bool{}
	for (a.View == "json" || a.View == "content" || a.View == "") && a.Pointer == "/text" && !seen[a.Handle] {
		seen[a.Handle] = true
		op, ok := byKey["tool:"+a.Handle]
		if !ok || !sourceReaderName(op.Kind) || op.Stage != "succeeded" {
			break
		}
		var out ToolResult
		var data struct {
			Handle, Selector, Pointer string
			Offset                    int
			Version                   string `json:"view_version"`
			Text                      string
			Block                     string `json:"block_id"`
		}
		if json.Unmarshal([]byte(op.ResultJSON), &out) != nil || out.Error != "" || json.Unmarshal(out.Data, &data) != nil || data.Handle == "" || data.Text == "" {
			break
		}
		a.Handle, a.Selector, a.Pointer, a.BlockID, a.ViewVersion = data.Handle, data.Selector, data.Pointer, data.Block, data.Version
		a.Offset += data.Offset
		a.View = "content"
		if data.Version == "receipt-json-v1" || data.Version == "json-value-v1" || data.Version == "knowledge-evidence-v1" {
			a.View = "json"
		}
	}
	return a
}

func resolveDataReadArgs(a dataReadArgs, ops []agentdata.Operation) (dataReadArgs, error) {
	if a.ContinueFrom != "" {
		id := a.ContinueFrom
		a.ContinueFrom = ""
		if a.Handle != "" || a.Cursor != "" || len(a.Fields) > 0 || a.Selector != "" || a.Offset != 0 || a.Limit != 0 || a.ViewVersion != "" || a.RunID != "" || a.ReceiptID != 0 || a.View != "" || a.Pointer != "" || a.BlockID != "" {
			return a, fmt.Errorf("continue_from must be passed alone")
		}
		found := false
		for _, op := range ops {
			if op.Key == "tool:"+id && op.Kind == "read_data" && op.Stage == "succeeded" {
				var out ToolResult
				var data struct {
					Cursor string `json:"next_cursor"`
				}
				if json.Unmarshal([]byte(op.ResultJSON), &out) != nil || json.Unmarshal(out.Data, &data) != nil {
					return a, fmt.Errorf("invalid prior read receipt")
				}
				if data.Cursor == "" {
					return a, fmt.Errorf("prior read has reached the end")
				}
				a = dataReadArgs{Cursor: data.Cursor}
				found = true
				break
			}
		}
		if !found {
			return a, fmt.Errorf("prior read not found in this Run")
		}
	}
	if a.Cursor != "" {
		if a.Handle != "" || len(a.Fields) > 0 || a.Selector != "" || a.Offset != 0 || a.Limit != 0 || a.ViewVersion != "" || a.RunID != "" || a.ReceiptID != 0 || a.View != "" || a.Pointer != "" || a.BlockID != "" {
			return a, fmt.Errorf("cursor must be passed alone, exactly as next_cursor returned")
		}
		decoded, err := cursorRoundTrip(a.Cursor)
		if err != nil {
			return a, toolContractFailure(fmt.Errorf("invalid read cursor: %w", err), "INVALID_READ_CURSOR", "not_started", map[string]any{"field": "cursor", "cause": err.Error(), "expected": "an unchanged next_cursor issued by read_data"}, ToolRecovery{Strategy: "use_observed_reference", Tool: "read_data", Instruction: "Copy the exact next_cursor from the saved structural read; do not construct or edit it. Alternatively use continue_from with that read's tool_call_id. If the read has no continuation, choose a different observed selector or produce the extractor."})
		}
		if decoded.Offset > 0 && decoded.ViewVersion == "" {
			return a, fmt.Errorf("cursor has no representation version; restart the source view")
		}
		a = decoded
		a.Cursor = ""
	}
	// A restored source or a structural reader alias refers to its original
	// source. Receipt/JSON views still inspect their own committed envelopes.
	_, handle, _ := readResource(ops, a.Handle, a.View == "dom" || a.View == "content")
	if handle != "" {
		a.Handle = handle
	}
	return a, nil
}

func readResource(ops []agentdata.Operation, handle string, followReader bool) (string, string, *sourceInspection) {
	byKey := map[string]agentdata.Operation{}
	for _, op := range ops {
		byKey[op.Key] = op
	}
	seen := map[string]bool{}
	for !seen[handle] {
		seen[handle] = true
		op, ok := byKey["tool:"+handle]
		if !ok {
			return "unresolved:" + handle, handle, nil
		}
		var out ToolResult
		if json.Unmarshal([]byte(op.ResultJSON), &out) != nil {
			return fmt.Sprintf("receipt:%d", op.ID), handle, nil
		}
		if out.ReplayOf != "" {
			handle = strings.TrimPrefix(out.ReplayOf, "tool:")
			continue
		}
		if op.Kind == "inspect_source" && op.Stage == "succeeded" && out.Error == "" {
			var page sourceInspection
			if json.Unmarshal(out.Data, &page) == nil && page.Source.Receipt.URL != "" {
				return "source:" + agentdata.Hash([]any{page.Source.Receipt.Method, page.Source.Receipt.URL, page.Source.Receipt.Status, page.Source.Body}), handle, &page
			}
		}
		if followReader && sourceReaderName(op.Kind) && op.Stage == "succeeded" && out.Error == "" {
			var data struct {
				Handle string `json:"handle"`
			}
			if json.Unmarshal(out.Data, &data) == nil && data.Handle != "" && data.Handle != handle {
				handle = data.Handle
				continue
			}
		}
		return fmt.Sprintf("receipt:%d", op.ID), handle, nil
	}
	return "unresolved:" + handle, handle, nil
}
