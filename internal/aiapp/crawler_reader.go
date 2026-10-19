package aiapp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"go-lua-crawler/internal/agentdata"
	"golang.org/x/net/html"
)

func crawlerRun(r agentdata.Run) bool {
	var budget BudgetSnapshot
	json.Unmarshal([]byte(r.BudgetJSON), &budget)
	return budget.LoopVersion >= 5
}

func crawlerReadFailure(a dataReadArgs, pages ...*sourceInspection) error {
	view := "dom"
	instruction := "Choose an observed selector from the structural catalog. DOM cursors page selector entries or matched nodes only. Use field_selectors or Lua with that locator to extract and store complete fields; do not read or page article text."
	if len(pages) > 0 && pages[0] != nil && pages[0].JSONStructure != nil {
		view = "json"
		instruction = "Use read_data with the supplied read_with arguments, without DOM selectors, text offsets or HTML view_version. JSON nodes expose value json_pointer and object key key_locator.json_key_pointer. Use those exact locator forms for extraction; an object key is not the value at its pointer. Declare produced_fields including join keys when a source supplies a subset."
	}
	return toolContractFailure(fmt.Errorf("crawler reads expose structure only; prose, text offsets and evidence reading belong to the separate knowledge pipeline"), "CRAWLER_STRUCTURE_ONLY", "not_started", map[string]any{"field": "view", "accepted": []string{view, "receipt metadata"}, "read_with": map[string]any{"handle": a.Handle, "view": view}}, ToolRecovery{Strategy: "inspect_and_correct", Tool: "read_data", Instruction: instruction})
}

func (s *Service) readCrawlerData(ctx context.Context, r agentdata.Run, call ToolCall, a dataReadArgs, ops []agentdata.Operation) (ToolResult, error) {
	_, _, feedbackSource := readResource(ops, a.Handle, false)
	readFailure := func() error { return crawlerReadFailure(a, feedbackSource) }
	if a.RunID != "" && a.RunID != r.ID || a.Offset < 0 || a.Limit < 0 || a.Limit > 2000 {
		return ToolResult{}, readFailure()
	}
	if a.View == "content" || a.BlockID != "" || len(a.Fields) > 0 || strings.HasPrefix(a.Handle, "ev_") {
		return ToolResult{}, readFailure()
	}
	_, handle, page := readResource(ops, a.Handle, a.View == "dom" || a.View == "")
	if a.View == "" {
		a.View = "dom"
	}
	a.Handle = handle
	if a.View != "dom" && a.View != "json" && a.View != "receipt" {
		return ToolResult{}, readFailure()
	}
	if a.View == "receipt" && (a.Offset != 0 || a.Limit != 0) {
		return ToolResult{}, readFailure()
	}
	if page == nil {
		if a.View != "receipt" || a.Pointer != "" || a.Selector != "" {
			return ToolResult{}, readFailure()
		}
		for _, op := range ops {
			if op.Key == "tool:"+handle && (op.Stage == "succeeded" || op.Stage == "failed") {
				var saved ToolResult
				if err := json.Unmarshal([]byte(op.ResultJSON), &saved); err != nil {
					return ToolResult{}, err
				}
				spec, _ := specOf(r)
				saved = modelObservationForRun(r, saved, &spec)
				return toolData(call, map[string]any{"handle": handle, "receipt": saved, "view_version": "crawler-receipt-v1", "new_http_requests": 0})
			}
		}
		return ToolResult{}, toolContractFailure(fmt.Errorf("handle has no saved receipt in this Run"), "SOURCE_HANDLE_UNAVAILABLE", "not_started", map[string]any{"field": "handle"}, ToolRecovery{Strategy: "use_existing_receipt", Instruction: "Use a handle returned by an actual inspection in this Run; a renamed handle does not create a new source."})
	}
	if a.View == "receipt" {
		if a.Pointer != "" || a.Selector != "" {
			return ToolResult{}, readFailure()
		}
		return toolData(call, map[string]any{"handle": handle, "receipt": page.Source.Receipt, "view_version": "fetch-metadata-v1", "new_http_requests": 0})
	}
	if page.JSONStructure != nil {
		if a.Selector != "" || a.ViewVersion != "" && a.ViewVersion != "json-structure-v1" {
			return ToolResult{}, readFailure()
		}
		var value any
		if err := json.Unmarshal([]byte(page.Source.Body), &value); err != nil {
			return ToolResult{}, err
		}
		if a.Pointer != "" {
			var err error
			value, err = jsonPointer(value, a.Pointer)
			if err != nil {
				return ToolResult{}, err
			}
		}
		container := true
		switch value.(type) {
		case map[string]any, []any:
		default:
			container = false
			if a.Offset != 0 || a.Limit != 0 {
				return ToolResult{}, readFailure()
			}
		}
		raw, _ := json.Marshal(value)
		nodes, total := crawlerJSONNodes(value, a.Pointer, a.Offset, a.Limit)
		data := map[string]any{"handle": handle, "pointer": a.Pointer, "json_structure": crawlerJSONInspection(string(raw)), "nodes": nodes, "total_nodes": total, "view_version": "json-structure-v1", "new_http_requests": 0, "next_step": "Use nodes' observed json_pointer for values and key_locator.json_key_pointer for object keys, with short field samples for Lua/json_decode and _evidence={field={json_pointer=...}}, or field_selectors. Open a container pointer for its members; next_read pages members/array entries only. Long text has no readable preview or text pagination."}
		if container && a.Offset+len(nodes) < total {
			limit := a.Limit
			if limit == 0 {
				limit = 64
			}
			data["next_read"] = dataReadArgs{Handle: handle, View: "json", Pointer: a.Pointer, Offset: a.Offset + len(nodes), Limit: limit, ViewVersion: "json-structure-v1"}
		}
		return toolData(call, data)
	}
	if a.View != "dom" || a.Pointer != "" {
		return ToolResult{}, readFailure()
	}
	var op agentdata.Operation
	for _, candidate := range ops {
		if candidate.Key == "tool:"+handle {
			op = candidate
			break
		}
	}
	var saved ToolResult
	if err := json.Unmarshal([]byte(op.ResultJSON), &saved); err != nil {
		return ToolResult{}, err
	}
	out, err := s.readDetailedData(ctx, r, call, op, a, saved)
	if err != nil {
		return out, err
	}
	var data map[string]any
	if err = json.Unmarshal(out.Data, &data); err != nil {
		return out, err
	}
	if catalog, ok := data["selector_catalog"]; ok {
		raw, _ := json.Marshal(catalog)
		var entries []DOMBlock
		json.Unmarshal(raw, &entries)
		data["selector_catalog"] = crawlerCatalog(page.Source.Body, entries)
	}
	if a.Selector != "" {
		doc, _ := goquery.NewDocumentFromReader(strings.NewReader(page.Source.Body))
		selection := doc.Find(a.Selector)
		data["html_sample"], data["sample_truncated"] = crawlerDOMSample(selection.First())
		if nodes, ok := data["nodes"].([]any); ok {
			for _, raw := range nodes {
				if n, ok := raw.(map[string]any); ok {
					index, _ := n["index"].(float64)
					sample := crawlerNodeSample(selection.Eq(int(index)))
					n["text"], n["text_truncated"] = sample, true
				}
			}
		}
	}
	data["narrow_with"] = "Use observed selectors and row links to write an extractor; next_read continues selector entries or matched nodes, never article text."
	return toolData(call, data)
}

func crawlerNodeSample(n *goquery.Selection) string {
	if n.Length() == 0 {
		return ""
	}
	name := n.Nodes[0].Data
	// A row that contains a link still carries its own inline fields beside it.
	// The real listing row is
	//   <li><p><span>[2026-10-15]</span><a href="info/…">标题</a></p></li>
	// so returning only the anchor's wording drops the date the row publishes.
	// A measured real run read `.block > li`, matched 15 rows, and then re-read
	// the same selector seven times with its own reasoning saying "Item text
	// doesn't include date" — the required published_date was in the row and the
	// projection threw it away. The whole row's visible text is what a caller
	// needs; the row's targets are published separately as nested links.
	if n.Find("a[href]").Length() > 0 {
		if full := normalizeEvidenceText(visibleHTMLText(n.Nodes[0])); full != "" {
			return fitObservationText(full, 180)
		}
		return fitObservationText(normalizeEvidenceText(n.Find("a[href]").First().Text()), 180)
	}
	// A bounded table row pairs its label with its value. Clearing the whole
	// row while allowing individual cells loses that relation and forces more
	// reads; long rows remain unreadable under the same field-sample limit.
	if strings.Contains("|h1|h2|h3|h4|h5|h6|title|time|span|label|dt|dd|tr|th|td|option|a|", "|"+name+"|") {
		text := normalizeEvidenceText(n.Text())
		if len([]rune(text)) <= 180 {
			return text
		}
	}
	return ""
}

// Preserve JSON paths/types and short scalar navigation values. Long values
// carry no previews: paging them is a knowledge-reader operation, not crawling.
func crawlerJSONInspection(body string) *JSONInspection {
	var value any
	if json.Unmarshal([]byte(body), &value) != nil {
		return nil
	}
	var redact func(any) any
	redact = func(v any) any {
		switch x := v.(type) {
		case map[string]any:
			for k, item := range x {
				x[k] = redact(item)
			}
		case []any:
			for i, item := range x {
				x[i] = redact(item)
			}
		case string:
			if len([]rune(x)) > 180 {
				return ""
			}
		}
		return v
	}
	raw, _ := json.Marshal(redact(value))
	// Values longer than the crawler field limit were removed above. Preserve
	// every remaining scalar intact: a clipped cursor/identifier is a new,
	// invalid value, not a useful structural preview.
	return inspectJSONWithSampleLimit(string(raw), 180)
}

func crawlerCatalog(body string, in []DOMBlock) []DOMBlock {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(body))
	if err != nil {
		return nil
	}
	out := append([]DOMBlock{}, in...)
	for i := range out {
		out[i].Sample = crawlerNodeSample(doc.Find(out[i].Selector).First())
		out[i].HTMLSample = ""
	}
	return out
}

func crawlerDOMSample(n *goquery.Selection) (string, bool) {
	clone := n.Clone()
	clone.Find("*").AddSelection(clone).Each(func(_ int, node *goquery.Selection) {
		for child := node.Nodes[0].FirstChild; child != nil; child = child.NextSibling {
			if child.Type == html.TextNode {
				child.Data = ""
			}
		}
	})
	return structuralDOMSample(clone)
}

// Program execution and SQL retain complete records. Model feedback carries
// extraction counts, locators and bounded scalar fields, not source_text bodies.
func crawlerObservation(out ToolResult, spec *TaskSpec) ToolResult {
	if sourceReaderName(out.Name) && out.Error == "" {
		var saved struct{ Handle, Version string }
		var data map[string]json.RawMessage
		if json.Unmarshal(out.Data, &data) == nil {
			json.Unmarshal(data["handle"], &saved.Handle)
			json.Unmarshal(data["view_version"], &saved.Version)
			if saved.Version != "dom-structure-v1" && saved.Version != "json-structure-v1" && saved.Version != "fetch-metadata-v1" && saved.Version != "crawler-receipt-v1" {
				out.Data, _ = json.Marshal(map[string]any{"handle": saved.Handle, "legacy_reader_requires_restart": true, "read_with": map[string]any{"handle": saved.Handle, "view": "dom"}, "reason": "Crawler runs expose structural selectors only; saved prose readers cannot be resumed."})
				return out
			}
		}
	}
	var data any
	if json.Unmarshal(out.Data, &data) != nil {
		return out
	}
	full, fields := map[string]bool{}, map[string]bool{}
	if spec != nil {
		for _, field := range spec.RequiredFields {
			fields[field.Name] = true
			if field.Type == "source_text" {
				full[field.Name] = true
			}
		}
	}
	var prune func(any, string, bool) any
	prune = func(value any, key string, extracted bool) any {
		switch v := value.(type) {
		case map[string]any:
			for k, x := range v {
				// These containers are extractor-owned data. Their keys are
				// arbitrary, so declared field names cannot police this boundary.
				dataValue := extracted || k == "items" || k == "records" || k == "metadata"
				v[k] = prune(x, k, dataValue)
			}
		case []any:
			for i, x := range v {
				v[i] = prune(x, key, extracted)
			}
		case string:
			if full[key] || (fields[key] || extracted) && len([]rune(v)) > 180 {
				return map[string]any{"extracted": true, "type": "string", "total_runes": len([]rune(v)), "model_readable": false}
			}
		}
		return value
	}
	out.Data, _ = json.Marshal(prune(data, "", false))
	return out
}

func crawlerFailureObservation(r agentdata.Run, out ToolResult, spec *TaskSpec) ToolResult {
	if out.Failure == nil || out.Failure.Details == nil {
		return out
	}
	raw, err := json.Marshal(out.Failure.Details)
	if err != nil {
		return out
	}
	var details map[string]json.RawMessage
	if json.Unmarshal(raw, &details) != nil {
		return out
	}
	if original := details["original_receipt"]; original != nil {
		var receipt ToolResult
		if json.Unmarshal(original, &receipt) == nil {
			details["original_receipt"], _ = json.Marshal(modelObservationForRun(r, receipt, spec))
			copy := *out.Failure
			copy.Details = details
			out.Failure = &copy
		}
	}
	return out
}
