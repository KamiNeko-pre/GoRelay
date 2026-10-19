package aiapp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"go-lua-crawler/internal/agentdata"
	"strings"
)

// Old reader offsets address another representation. Their immutable receipts
// remain auditable, but cannot be replayed as current semantic content.
func historyReaderView(state LoopState, receipt ToolResult, call ToolCall) ToolResult {
	if !sourceReaderName(receipt.Name) {
		return receipt
	}
	// A failed read is a diagnostic, not a successful unversioned page. Migrating
	// it replaced its source choices/correction data with legacy_reader_unresolved.
	if receipt.Error != "" {
		return receipt
	}
	// Plan history is a bounded model view, not the canonical receipt. Restore
	// only this run's successful reader before inspecting protocol metadata.
	for _, op := range state.Operations {
		if op.RunID != state.Run.ID || op.Key != "tool:"+receipt.CallID || op.Kind != receipt.Name || op.Stage != "succeeded" {
			continue
		}
		var original ToolResult
		if json.Unmarshal([]byte(op.ResultJSON), &original) == nil && original.CallID == receipt.CallID && original.Name == receipt.Name {
			receipt = original
		}
		break
	}
	unresolved := func() ToolResult {
		receipt.Data, _ = json.Marshal(map[string]any{"legacy_reader_unresolved": true, "reason": "Original reader proposal/source binding unavailable; saved representation cannot be safely replayed.", "new_http_requests": 0})
		return receipt
	}
	var data map[string]json.RawMessage
	if json.Unmarshal(receipt.Data, &data) != nil {
		return unresolved()
	}
	var version string
	_ = json.Unmarshal(data["view_version"], &version)
	if version != "" {
		return receipt
	}
	var args struct {
		Handle      string `json:"handle"`
		CallID      string `json:"inspection_call_id"`
		ToolID      string `json:"tool_call_id"`
		Cursor      string `json:"cursor"`
		Selector    string `json:"selector"`
		Pointer     string `json:"pointer"`
		ViewVersion string `json:"view_version"`
	}
	if json.Unmarshal([]byte(call.Function.Arguments), &args) != nil {
		return unresolved()
	}
	if args.Cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(args.Cursor)
		if err != nil || json.Unmarshal(raw, &args) != nil {
			return unresolved()
		}
	}
	var sourceID string
	switch receipt.Name {
	case "read_data":
		sourceID = args.Handle
	case "read_source_content":
		sourceID = args.CallID
	case "read_observation":
		sourceID = args.ToolID
	default:
		return receipt
	}
	if sourceID == "" {
		return unresolved()
	}
	for _, op := range state.Operations {
		if op.RunID != state.Run.ID || op.Key != "tool:"+sourceID || op.Kind != "inspect_source" || op.Stage != "succeeded" {
			continue
		}
		var original ToolResult
		var page sourceInspection
		if json.Unmarshal([]byte(op.ResultJSON), &original) != nil || json.Unmarshal(original.Data, &page) != nil {
			return unresolved()
		}
		info, err := buildHTMLInformation(page.Source.Body, page.Source.Receipt.URL, "")
		if err == nil && info == nil {
			return receipt
		} // JSON and plain text retain their offsets.
		// The history reference tells a caller how to reach this material again.
		// It named view=content, which read_data now refuses; the structural view
		// is the one that works.
		view := "dom"
		pointer := ""
		if receipt.Name == "read_observation" {
			view = "receipt"
			pointer = strings.TrimPrefix(args.Pointer, "/data")
		}
		ref := map[string]any{"handle": sourceID, "view": view, "offset": 0}
		if args.Selector != "" {
			ref["selector"] = args.Selector
		}
		if pointer != "" {
			ref["pointer"] = pointer
		}
		receipt.Data, _ = json.Marshal(map[string]any{"legacy_reader_requires_restart": true, "reason": "Saved offset belongs to an unversioned HTML representation; restart this run-owned source view.", "read_with": ref, "new_http_requests": 0})
		return receipt
	}
	return unresolved()
}

func sourceReaderName(name string) bool {
	return name == "read_data" || name == "read_source_content" || name == "read_observation"
}

// Resolves legacy representation ownership before any JSON path/named field
// can expose the saved reader output. The SQL original remains unchanged.
func (s *Service) readerReceiptView(ctx context.Context, run agentdata.Run, op agentdata.Operation, receipt ToolResult) (ToolResult, error) {
	if receipt.Error != "" {
		return receipt, nil
	}
	if !sourceReaderName(op.Kind) {
		return receipt, nil
	}
	var data map[string]json.RawMessage
	if json.Unmarshal(receipt.Data, &data) != nil {
		return ToolResult{}, fmt.Errorf("invalid reader receipt")
	}
	var version string
	_ = json.Unmarshal(data["view_version"], &version)
	if version != "" || data["legacy_reader_requires_restart"] != nil || data["legacy_reader_unresolved"] != nil {
		return receipt, nil
	}
	var messages []Message
	if run.PlanJSON != "" {
		if err := json.Unmarshal([]byte(run.PlanJSON), &messages); err != nil {
			return ToolResult{}, err
		}
	}
	ops, err := s.Data.Operations(ctx, run.ID)
	if err != nil {
		return ToolResult{}, err
	}
	var proposal ToolCall
	for _, m := range messages {
		for _, call := range m.ToolCalls {
			if "tool:"+call.ID == op.Key && call.Function.Name == op.Kind {
				proposal = call
			}
		}
	}
	return historyReaderView(LoopState{Run: run, Operations: ops}, receipt, proposal), nil
}
