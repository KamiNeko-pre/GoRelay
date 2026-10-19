package aiapp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"go-lua-crawler/internal/agentdata"
	"sort"
	"strconv"
	"strings"
)

const observationBytes = 6 << 10

// probeObservationBytes is the budget for a validation probe's feedback. A probe
// reports whether a generated program produced its declared fields, and the
// caller cannot correct a program it cannot see the output of. The value is
// larger than a page slice because probe output is one-shot diagnostic data and
// is not re-read through the document pager.
const probeObservationBytes = 24 << 10

// probeFeedback reports whether this tool's receipt is program-diagnostic
// feedback that the caller must see, rather than page content it can page.
func probeFeedback(name string) bool {
	return name == "validate_program" || name == "submit_acquisition" || name == "submit_extraction" || name == "register_source"
}

// SQL owns the original receipt. Paging limits payloads, never opaque identities.
func boundedObservation(out ToolResult) ToolResult {
	return observationWithLimit(out, observationBytes)
}

func observationWithLimit(out ToolResult, budget int, specs ...*TaskSpec) ToolResult {
	// A validation probe is diagnostic feedback, not page prose. It is the only
	// signal that tells the caller whether its generated program produced the
	// fields it declared, and it is produced once per attempt rather than paged
	// like a document. Charging it to the document budget meant that any probe
	// returning a long field (an article body, a wide table) degraded to a bare
	// reference with observation=null and total=0, so the caller saw no result at
	// all and rewrote the same program repeatedly — the recorded run spent 13
	// successful probes without ever learning what they returned.
	probeBudget := budget
	if probeFeedback(out.Name) {
		probeBudget = probeObservationBytes
	}
	return observationWithinBudget(out, probeBudget, specs...)
}

// Whole-context and recovery-memory owners may compact an old diagnostic.
// They must not silently inherit the larger budget of a fresh program result.
func observationWithinBudget(out ToolResult, probeBudget int, specs ...*TaskSpec) ToolResult {
	if len(out.Data) <= probeBudget {
		return out
	}
	var value any
	if json.Unmarshal(out.Data, &value) != nil {
		return out
	}
	if probeFeedback(out.Name) {
		if control, ok := workflowControlObservation(value, out.CallID); ok {
			out.Data, _ = json.Marshal(control)
			return out
		}
	}
	if view, ok := value.(map[string]any); ok && view["_receipt_view"] == "v1" {
		return observationReceiptHandle(out)
	}
	for page := 8; page >= 1; page /= 2 {
		reads := []observationRead{}
		sample := projectObservation(value, "/data", "", out.CallID, page, &reads, projectionFields(specs...))
		wrapped := map[string]any{"_receipt_view": "v1", "observation": sample, "observation_truncated": true, "receipt_tool_call_id": out.CallID, "reads": reads, "read_full_with": "Use the exact read_observation arguments in reads; offsets address the original persisted receipt."}
		raw, _ := json.Marshal(wrapped)
		if len(raw) <= probeBudget {
			out.Data = raw
			return out
		}
	}
	// An indivisible metadata item may exceed the view budget. Return its exact
	// container reader rather than producing a subtly corrupted identifier.
	return observationReceiptHandle(out)
}

// Bulk execution results can be reduced; executable continuations cannot.
// Preserve complete IDs and arguments instead of sending a crawler to a
// document reader that is absent from its tool menu.
func workflowControlObservation(value any, callID string) (map[string]any, bool) {
	root, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	if wrapped, ok := root["observation"].(map[string]any); ok && root["_receipt_view"] == "v1" {
		root = wrapped
	}
	if root["next_action"] == nil && root["artifact_hash"] == nil && root["task_id"] == nil {
		return nil, false
	}
	control := map[string]any{"observation_truncated": true, "receipt_handle": callID, "read_with": map[string]any{"handle": callID, "view": "receipt"}}
	for _, key := range []string{"id", "candidate_id", "artifact_hash", "source_id", "kind", "mode", "next_action", "publication", "task_id", "status", "rows", "produced_fields", "child_run_id"} {
		if v, ok := root[key]; ok {
			control[key] = v
		}
	}
	return control, true
}

func observationReceiptHandle(out ToolResult) ToolResult {
	// Report the real element count. A hardcoded 0 tells the caller the receipt
	// holds nothing, which reads as "your program produced no output" and sends it
	// back to rewrite a program that may have been correct.
	total := 0
	var value any
	if json.Unmarshal(out.Data, &value) == nil {
		if root, ok := value.(map[string]any); ok {
			if data, ok := root["data"].(map[string]any); ok {
				for _, key := range []string{"items", "records", "tasks", "collection"} {
					if list, ok := data[key].([]any); ok && len(list) > 0 {
						total = len(list)
						break
					}
				}
				if total == 0 {
					total = len(data)
				}
			} else if len(root) > 0 {
				total = len(root)
			}
		}
	}
	read := newObservationRead(out.CallID, "/data", 0, "value", total)
	out.Data, _ = json.Marshal(map[string]any{"_receipt_view": "v1", "observation_truncated": true, "receipt_tool_call_id": out.CallID, "reads": []observationRead{read}, "read_full_with": "read_observation"})
	return out
}

type observationRead struct {
	Kind      string         `json:"kind"`
	Total     int            `json:"total"`
	Arguments map[string]any `json:"arguments"`
}

func newObservationRead(callID, pointer string, offset int, kind string, total int) observationRead {
	return observationRead{Kind: kind, Total: total, Arguments: map[string]any{"tool_call_id": callID, "pointer": pointer, "offset": offset, "limit": 1}}
}

func referenceField(key string) bool {
	return key == "selector" || key == "id" || key == "handle" || key == "url" || key == "revision" || key == "generation" || key == "status" || key == "pointer" || key == "cursor" || strings.HasSuffix(key, "_cursor") || strings.HasSuffix(key, "_id") || strings.HasSuffix(key, "_hash") || strings.HasSuffix(key, "_url")
}

func pointerKey(key string) string {
	return strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
}

func projectObservation(value any, pointer, key, callID string, page int, reads *[]observationRead, priority map[string]bool) any {
	switch v := value.(type) {
	case string:
		runes := []rune(v)
		if len(runes) > 160 && !referenceField(key) {
			read := newObservationRead(callID, pointer, 0, "text", len(runes))
			read.Arguments["limit"] = 1500
			*reads = append(*reads, read)
			return map[string]any{"preview": string(runes[:160]), "truncated": true}
		}
		return v
	case []any:
		out := []any{}
		for n, item := range v[:min(page, len(v))] {
			out = append(out, projectObservation(item, pointer+"/"+strconv.Itoa(n), "", callID, page, reads, priority))
		}
		if len(v) > len(out) {
			*reads = append(*reads, newObservationRead(callID, pointer, len(out), "array", len(v)))
		}
		return out
	case map[string]any:
		out := map[string]any{}
		keys := []string{}
		for k := range v {
			keys = append(keys, k)
		}
		sortProjectionKeys(keys, priority)
		// Preserve normal typed objects in full; unusually wide objects are paged.
		count := min(len(keys), max(16, page))
		if key == "field_types" {
			count = len(keys)
		} // Observed schema names/types remain visible.
		for _, k := range keys[:count] {
			out[k] = projectObservation(v[k], pointer+"/"+pointerKey(k), k, callID, page, reads, priority)
		}
		if len(keys) > count {
			*reads = append(*reads, newObservationRead(callID, pointer, count, "object", len(keys)))
		}
		return out
	default:
		return value
	}
}

func projectionFields(specs ...*TaskSpec) map[string]bool {
	out := map[string]bool{}
	for _, spec := range specs {
		if spec == nil {
			continue
		}
		for _, f := range spec.RequiredFields {
			out[f.Name] = true
		}
		for _, key := range spec.JoinKeys {
			out[key] = true
		}
		if spec.EntityField != "" {
			out[spec.EntityField] = true
		}
	}
	return out
}
func sortProjectionKeys(keys []string, priority map[string]bool) {
	rank := func(k string) int {
		if priority[k] {
			return 0
		}
		if referenceField(k) {
			return 1
		}
		return 2
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := rank(keys[i]), rank(keys[j])
		if a != b {
			return a < b
		}
		return keys[i] < keys[j]
	})
}

func (s *Service) readObservation(ctx context.Context, r agentdata.Run, call ToolCall) (ToolResult, error) {
	var a struct {
		CallID      string `json:"tool_call_id"`
		Pointer     string `json:"pointer"`
		Offset      int    `json:"offset"`
		Limit       int    `json:"limit"`
		ViewVersion string `json:"view_version,omitempty"`
	}
	if err := decodeArgs(call.Function.Arguments, &a); err != nil {
		return ToolResult{}, err
	}
	var op agentdata.Operation
	if err := s.Data.DB.WithContext(ctx).Where("run_id = ? AND `key` = ? AND stage IN ?", r.ID, "tool:"+a.CallID, []string{"succeeded", "failed"}).First(&op).Error; err != nil {
		return ToolResult{}, err
	}
	var value any
	receiptJSON := []byte(op.ResultJSON)
	if sourceReaderName(op.Kind) {
		var receipt ToolResult
		if err := json.Unmarshal(receiptJSON, &receipt); err != nil {
			return ToolResult{}, err
		}
		view, err := s.readerReceiptView(ctx, r, op, receipt)
		if err != nil {
			return ToolResult{}, err
		}
		receiptJSON, _ = json.Marshal(view)
	}
	if op.Kind == "inspect_source" {
		var receipt ToolResult
		var page sourceInspection
		if err := json.Unmarshal(receiptJSON, &receipt); err != nil {
			return ToolResult{}, err
		}
		if err := json.Unmarshal(receipt.Data, &page); err != nil {
			return ToolResult{}, err
		}
		view, err := sourceReadingView(page)
		if err != nil {
			return ToolResult{}, err
		}
		if a.ViewVersion != "" && a.ViewVersion != view.ViewVersion {
			return ToolResult{}, fmt.Errorf("reader version changed; restart at offset 0")
		}
		if view.ViewVersion == htmlInformationVersion && a.Pointer == "/data/source/body" && a.Offset > 0 && a.ViewVersion == "" {
			return ToolResult{}, fmt.Errorf("unversioned HTML offset; restart at offset 0")
		}
		a.ViewVersion = view.ViewVersion
		receipt.Data, _ = json.Marshal(view)
		receiptJSON, _ = json.Marshal(receipt)
	}
	if err := json.Unmarshal(receiptJSON, &value); err != nil {
		return ToolResult{}, err
	}
	value, err := jsonPointer(value, a.Pointer)
	if err != nil {
		return ToolResult{}, err
	}
	if a.Offset < 0 || a.Limit < 0 || a.Limit > 2000 {
		return ToolResult{}, fmt.Errorf("invalid observation page")
	}
	version := "receipt-json-v1"
	view := "json"
	if op.Kind == "inspect_source" {
		view, version = "receipt", "inspection-receipt-v1"
	}
	exact := dataReadArgs{Handle: a.CallID, RunID: r.ID, ReceiptID: op.ID, View: view, ViewVersion: version, Pointer: strings.TrimPrefix(a.Pointer, "/data"), Offset: a.Offset, Limit: a.Limit}
	if a.Pointer == "" {
		// Legacy root addresses the ToolResult envelope, while the unified JSON
		// reader addresses its Data. Rebind the Data child before paging.
		if envelope, ok := value.(map[string]any); ok {
			if data, exists := envelope["data"]; exists {
				envelope["data"] = informationChild(data, exact, "")
			}
		}
	}
	if a.ViewVersion == "" {
		a.ViewVersion = version
	}
	meta := map[string]any{"pointer": a.Pointer, "receipt_tool_call_id": a.CallID, "view_version": a.ViewVersion, "new_http_requests": 0, "next_offset": nil}
	out, err := informationValuePage(call, exact, value, meta)
	if err != nil {
		return ToolResult{}, err
	}
	var data map[string]any
	_ = json.Unmarshal(out.Data, &data)
	if cursor, ok := data["next_cursor"].(string); ok {
		raw, _ := base64.RawURLEncoding.DecodeString(cursor)
		var next dataReadArgs
		_ = json.Unmarshal(raw, &next)
		data["next_offset"] = next.Offset
	}
	return boundedReadResult(call, data)
}

func jsonPointer(value any, pointer string) (any, error) {
	if pointer == "" {
		return value, nil
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, fmt.Errorf("JSON pointer must start with /")
	}
	parent := ""
	for _, key := range strings.Split(pointer[1:], "/") {
		key = strings.ReplaceAll(strings.ReplaceAll(key, "~1", "/"), "~0", "~")
		switch v := value.(type) {
		case map[string]any:
			var ok bool
			value, ok = v[key]
			if !ok {
				keys := make([]string, 0, len(v))
				for actual := range v {
					keys = append(keys, actual)
				}
				sort.Strings(keys)
				return nil, fmt.Errorf("observation path %q does not exist at parent %q; observed keys: %v", key, parent, keys[:min(16, len(keys))])
			}
		case []any:
			index, err := strconv.Atoi(key)
			if err != nil || index < 0 || index >= len(v) || strconv.Itoa(index) != key {
				return nil, fmt.Errorf("observation index %q outside array at %q; valid indexes are 0..%d", key, parent, len(v)-1)
			}
			value = v[index]
		default:
			return nil, fmt.Errorf("observation path crosses a %T scalar at %q; cannot select %q", value, parent, key)
		}
		parent += "/" + pointerKey(key)
	}
	return value, nil
}
