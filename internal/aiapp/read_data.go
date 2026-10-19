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

type dataReadArgs struct {
	ContinueFrom string   `json:"continue_from,omitempty"`
	Handle       string   `json:"handle,omitempty"`
	Cursor       string   `json:"cursor,omitempty"`
	Fields       []string `json:"fields,omitempty"`
	Selector     string   `json:"selector,omitempty"`
	Offset       int      `json:"offset,omitempty"`
	Limit        int      `json:"limit,omitempty"`
	ViewVersion  string   `json:"view_version,omitempty"`
	RunID        string   `json:"run_id,omitempty"`
	ReceiptID    uint64   `json:"receipt_id,omitempty"`
	View         string   `json:"view,omitempty"`
	Pointer      string   `json:"pointer,omitempty"`
	BlockID      string   `json:"block_id,omitempty"`
}

// nextReadCursor encodes the exact continuation of one read.
//
// The cursor is base64 of the whole request, so every field it carries is paid
// for out of the same observation budget as the text it continues. Run id,
// receipt id and handle are already implied by the handle the caller repeats, so
// they are omitted. The offset, the selected view and its version must travel,
// because the reader refuses an offset into HTML that does not declare which
// projection produced it: dropping the version turned a valid continuation into
// "unversioned HTML offset; restart reading" and made the model re-read from the
// beginning.
func nextReadCursor(args dataReadArgs, offset int) string {
	trimmed := dataReadArgs{Offset: offset, Limit: args.Limit, View: args.View, ViewVersion: args.ViewVersion}
	if args.BlockID != "" || args.Pointer != "" || args.Selector != "" {
		trimmed.BlockID = args.BlockID
		trimmed.Pointer = args.Pointer
		trimmed.Selector = args.Selector
	}
	if args.Handle != "" {
		trimmed.Handle = args.Handle
	}
	raw, _ := json.Marshal(trimmed)
	return base64.RawURLEncoding.EncodeToString(raw)
}

// cursorRoundTrip is the shared decode used by both the reader and its tests.
func cursorRoundTrip(cursor string) (dataReadArgs, error) {
	var out dataReadArgs
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return out, err
	}
	err = json.Unmarshal(raw, &out)
	return out, err
}

// One model-facing reader. Receipt and evidence ownership checks remain at
// their original boundaries; merging the interface never merges permissions.
func (s *Service) readData(ctx context.Context, r agentdata.Run, call ToolCall) (ToolResult, error) {
	var a dataReadArgs
	if err := decodeArgs(call.Function.Arguments, &a); err != nil {
		return ToolResult{}, err
	}
	// Discover regions structurally; read their complete text with the same
	// selector/block contract that the host publishes in its observations.
	if !crawlerRun(r) && a.View == "content" && a.Selector == "" && a.BlockID == "" && a.Pointer == "" {
		return ToolResult{}, toolContractFailure(
			fmt.Errorf("view=content requires a selector, block_id or exact pointer"),
			"ARGUMENT_JSON_ERROR", "not_started",
			map[string]any{"field": "selector", "actual": "", "read_with": map[string]any{"handle": a.Handle, "view": "dom"}},
			ToolRecovery{
				Strategy: "correct_arguments",
				Instruction: "Choose a source region before reading its text. To read a listing with each row's own title " +
					"and target URL, use view=dom with the row selector this page's selector_menu gives you. To see " +
					"that inventory, call read_data with view=dom and no selector; to read what one row selector " +
					"matched, repeat view=dom with it. To read the complete text of an observed selector or block, use view=content with that selector or block_id.",
			})
	}

	ops, err := s.Data.Operations(ctx, r.ID)
	if err != nil {
		return ToolResult{}, err
	}
	a, err = resolveDataReadArgs(a, ops)
	if err != nil {
		return ToolResult{}, err
	}
	if crawlerRun(r) {
		return s.readCrawlerData(ctx, r, call, a, ops)
	}
	if a.RunID != "" && a.RunID != r.ID {
		return ToolResult{}, fmt.Errorf("cursor belongs to another Run")
	}
	if a.Handle == "" {
		return ToolResult{}, fmt.Errorf("handle is required: copy the source handle returned by inspect_source, or echo a prior next_read using continue_from")
	}
	if a.Offset < 0 {
		return ToolResult{}, fmt.Errorf("offset=%d is invalid; use a nonnegative rune offset from this view's next_read", a.Offset)
	}
	if a.Limit < 0 || a.Limit > 2000 {
		return ToolResult{}, fmt.Errorf("limit=%d exceeds the read contract: limit is 0 (default) or 1..2000 runes; use next_read for the remainder, or submit_extraction(field_selectors) to extract complete source_text without paging", a.Limit)
	}
	if len(a.Fields) > 16 {
		return ToolResult{}, fmt.Errorf("fields has %d entries; read at most 16 named fields or select a JSON pointer", len(a.Fields))
	}
	if a.Selector != "" && len(a.Fields) > 0 {
		return ToolResult{}, fmt.Errorf("choose JSON fields or HTML selector")
	}
	if strings.HasPrefix(a.Handle, "ev_") {
		if experimentControls(r).DisableKnowledgeReuse {
			return ToolResult{}, fmt.Errorf("knowledge reuse disabled for this run")
		}
		if a.Selector != "" || a.BlockID != "" || len(a.Fields) > 0 || (a.View != "" && a.View != "json") || a.ReceiptID != 0 {
			return ToolResult{}, fmt.Errorf("knowledge evidence supports JSON paths only")
		}
		args, _ := json.Marshal(map[string]string{"evidence_id": a.Handle})
		legacy := call
		legacy.Function = FunctionCall{Name: "read_evidence", Arguments: string(args)}
		out, err := s.dispatchTool(ctx, r, legacy)
		if err != nil {
			return ToolResult{}, err
		}
		const version = "knowledge-evidence-v1"
		if a.ViewVersion != "" && a.ViewVersion != version {
			return ToolResult{}, fmt.Errorf("evidence representation changed; restart reading")
		}
		a.RunID, a.View, a.ViewVersion = r.ID, "json", version
		var value any
		if err := json.Unmarshal(out.Data, &value); err != nil {
			return ToolResult{}, err
		}
		value, err = jsonPointer(value, a.Pointer)
		if err != nil {
			return ToolResult{}, err
		}
		meta := map[string]any{"handle": a.Handle, "pointer": a.Pointer, "view_version": version, "new_http_requests": 0}
		return informationValuePage(call, a, value, meta)
	}
	var op agentdata.Operation
	if err := s.Data.DB.WithContext(ctx).Where("run_id = ? AND `key` = ? AND stage IN ?", r.ID, "tool:"+a.Handle, []string{"succeeded", "failed"}).First(&op).Error; err != nil {
		return ToolResult{}, err
	}
	var receipt ToolResult
	if err := json.Unmarshal([]byte(op.ResultJSON), &receipt); err != nil {
		return ToolResult{}, err
	}
	receipt, err = s.readerReceiptView(ctx, r, op, receipt)
	if err != nil {
		return ToolResult{}, err
	}
	var readerData map[string]json.RawMessage
	_ = json.Unmarshal(receipt.Data, &readerData)
	if readerData["legacy_reader_requires_restart"] != nil || readerData["legacy_reader_unresolved"] != nil {
		receipt.CallID, receipt.Name = call.ID, call.Function.Name
		return receipt, nil
	}
	if a.ReceiptID != 0 && a.ReceiptID != uint64(op.ID) {
		return ToolResult{}, fmt.Errorf("cursor receipt changed; restart reading")
	}
	a.RunID, a.ReceiptID = r.ID, uint64(op.ID)
	fetchedPage := isFetchedInspection(receipt)
	if fetchedPage && a.View == "" && len(a.Fields) == 0 {
		// Default to the structural view, not the prose view.
		//
		// A recorded real run read an inspected page 14 times with
		// view=content and produced no field: what came back was two thousand
		// runes of page text, which contains every requested value and names
		// none of them, so the model kept reading and never submitted. The
		// structural view answers the question a crawler actually asks — which
		// selectors this page has and what each matched node holds — so it is
		// the correct default. The prose view stays available only through
		// host internals (extraction, RAG), never through a model-facing entry.
		a.View = "dom"
	}
	if a.View != "" || a.Pointer != "" || a.BlockID != "" {
		if len(a.Fields) > 0 {
			return ToolResult{}, fmt.Errorf("choose named fields or an exact view/path")
		}
		return s.readDetailedData(ctx, r, call, op, a, receipt)
	}
	if a.Selector != "" {
		// A selector with no view means "read this region's text", which is a host
		// choice, not a caller asking for whole-page prose. The caller door above
		// already refused an explicit view=content.
		a.View = "content"
		return s.readDetailedData(ctx, r, call, op, a, receipt)
	}
	if len(a.Fields) == 0 {
		var spec *TaskSpec
		if r.SpecJSON != "" {
			value, err := specOf(r)
			if err != nil {
				return ToolResult{}, err
			}
			spec = &value
		}
		view := modelObservationForRun(r, receipt, spec)
		out, err := boundedReadResult(call, map[string]any{"handle": a.Handle, "receipt": view, "view_version": "receipt-snapshot-v1", "new_http_requests": 0})
		if err == nil {
			return out, nil
		}
		a.View = "json"
		if fetchedPage {
			a.View = "receipt"
		}
		return s.readDetailedData(ctx, r, call, op, a, receipt)
	}
	var value any
	if err := json.Unmarshal(receipt.Data, &value); err != nil {
		return ToolResult{}, err
	}
	if fetchedPage {
		page, err := s.inspection(ctx, r, a.Handle)
		if err != nil {
			return ToolResult{}, err
		}
		if json.Unmarshal([]byte(page.Source.Body), &value) != nil {
			page, err = sourceReadingView(page)
			if err != nil {
				return ToolResult{}, err
			}
			if page.ViewVersion == htmlInformationVersion && a.Offset > 0 && a.ViewVersion == "" {
				return ToolResult{}, fmt.Errorf("unversioned HTML offset; restart at offset 0")
			}
			if a.ViewVersion != "" && a.ViewVersion != page.ViewVersion {
				return ToolResult{}, fmt.Errorf("reader version changed; restart at offset 0")
			}
			a.ViewVersion = page.ViewVersion
			if len(a.Fields) != 1 || a.Fields[0] != "body" {
				return ToolResult{}, fmt.Errorf("HTML/plain content fields require body; use content blocks for structured reads")
			}
			a.View = "content"
			a.Fields = nil
			return s.readDetailedData(ctx, r, call, op, a, receipt)
		}
	}
	a.View = "json"
	version := "receipt-json-v1"
	if fetchedPage {
		version = "json-value-v1"
	}
	if a.ViewVersion != "" && a.ViewVersion != version {
		return ToolResult{}, fmt.Errorf("reader version changed; restart reading")
	}
	a.ViewVersion = version
	matches := []map[string]any{}
	wanted := map[string]bool{}
	for _, field := range a.Fields {
		if field == "" || len(field) > 128 {
			return ToolResult{}, fmt.Errorf("nonempty field names required")
		}
		wanted[field] = true
	}
	collectNamedFieldsAt(value, wanted, "", &matches)
	if len(matches) == 0 {
		return ToolResult{}, fmt.Errorf("requested fields absent in this receipt")
	}
	if len(a.Fields) == 1 && len(matches) == 1 {
		field := a.Fields[0]
		pointer := matches[0]["pointer"].(string) + "/" + pointerKey(field)
		selected := matches[0]["fields"].(map[string]any)[field]
		exact := a
		exact.Fields = nil
		exact.Pointer = pointer
		meta := map[string]any{"handle": a.Handle, "field": field, "pointer": pointer, "view_version": version, "new_http_requests": 0}
		if items, ok := matches[0]["fields"].(map[string]any)[a.Fields[0]].([]any); ok {
			out, err := informationValuePage(call, exact, items, meta)
			if err != nil {
				return out, err
			}
			var data map[string]any
			_ = json.Unmarshal(out.Data, &data)
			data["items"] = data["value"]
			delete(data, "value")
			return boundedReadResult(call, data)
		}
		if _, ok := selected.(string); ok {
			return informationValuePage(call, exact, selected, meta)
		}
	}
	if a.Offset > len(matches) {
		return ToolResult{}, fmt.Errorf("offset outside matched objects")
	}
	count := a.Limit
	if count == 0 {
		count = 4
	}
	count = min(count, 8)
	for n, match := range matches {
		pointer := match["pointer"].(string)
		fields := map[string]any{}
		for key, v := range match["fields"].(map[string]any) {
			exact := a
			exact.Fields = nil
			fields[key] = informationChild(v, exact, pointer+"/"+pointerKey(key))
		}
		matches[n] = map[string]any{"pointer": pointer, "fields": fields}
	}
	// Discovery cursors keep named-field mode. Child references above select an
	// exact JSON path instead; these are different request shapes.
	a.View = ""
	for {
		end := min(a.Offset+count, len(matches))
		var cursor any
		if end < len(matches) {
			cursor = nextReadCursor(a, end)
		}
		data := map[string]any{"handle": a.Handle, "matches": matches[a.Offset:end], "total_matches": len(matches), "next_cursor": cursor, "view_version": version, "new_http_requests": 0}
		out, err := boundedReadResult(call, data)
		if err == nil {
			return out, nil
		}
		if count <= 1 {
			return ToolResult{}, fmt.Errorf("named match exceeds budget; use exact JSON pointer %q (explicit gap)", matches[a.Offset]["pointer"])
		}
		count = max(1, count/2)
	}
}

func collectNamedFields(value any, wanted map[string]bool, out *[]map[string]any) {
	collectNamedFieldsAt(value, wanted, "", out)
}
func collectNamedFieldsAt(value any, wanted map[string]bool, pointer string, out *[]map[string]any) {
	switch v := value.(type) {
	case map[string]any:
		fields := map[string]any{}
		keys := []string{}
		for key, val := range v {
			keys = append(keys, key)
			if wanted[key] {
				fields[key] = val
			}
		}
		if len(fields) > 0 {
			*out = append(*out, map[string]any{"fields": fields, "pointer": pointer})
		}
		sort.Strings(keys)
		for _, key := range keys {
			collectNamedFieldsAt(v[key], wanted, pointer+"/"+pointerKey(key), out)
		}
	case []any:
		for n, item := range v {
			collectNamedFieldsAt(item, wanted, pointer+"/"+strconv.Itoa(n), out)
		}
	}
}
