package aiapp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"go-lua-crawler/internal/agentdata"
)

func boundedUnifiedRead(t *testing.T, s *Service, run agentdata.Run, args any) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(args)
	out, err := s.readData(context.Background(), run, ToolCall{ID: "read", Function: FunctionCall{Name: "read_data", Arguments: string(raw)}})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Data) > observationBytes {
		t.Fatalf("reader hid oversized value behind another truncation: %d", len(out.Data))
	}
	var data map[string]any
	if err := json.Unmarshal(out.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data["new_http_requests"] != float64(0) {
		t.Fatal("read caused HTTP", data)
	}
	return data
}

func TestUnifiedReaderExactJSONPathAndLargeStringProgress(t *testing.T) {
	condition := strings.Repeat("每月按请求计费；", 1400) + "仅适用于 advanced 模式。"
	body, _ := json.Marshal(map[string]any{"plans": []any{map[string]any{"conditions": "unrelated basic condition"}, map[string]any{"conditions": condition, "unit": "per request"}}, "conditions": "root condition"})
	s, run, _ := storedHTMLReader(t, string(body))
	data := boundedUnifiedRead(t, s, run, map[string]any{"handle": "source", "view": "json", "pointer": "/plans/1/conditions"})
	text := ""
	pages := 0
	for {
		pages++
		text += data["text"].(string)
		if data["next_cursor"] == nil {
			break
		}
		if pages >= 16 {
			t.Fatal("reader made insufficient progress in frozen 16-page budget")
		}
		data = boundedUnifiedRead(t, s, run, map[string]any{"cursor": data["next_cursor"]})
	}
	if text != condition {
		t.Fatal("condition changed or wrong same-named field selected")
	}
	object := boundedUnifiedRead(t, s, run, map[string]any{"handle": "source", "view": "json", "pointer": "/plans/1"})
	fields := object["value"].(map[string]any)
	ref := fields["conditions"].(map[string]any)
	if ref["omitted"] != true || ref["read_with"] == nil || fields["unit"] != "per request" {
		t.Fatal("large object lost navigable field/unit", object)
	}
	child := boundedUnifiedRead(t, s, run, ref["read_with"])
	if !strings.HasPrefix(condition, child["text"].(string)) {
		t.Fatal("returned reference points elsewhere")
	}
	t.Logf("exact long condition reachable in %d pages with no HTTP", pages)
}

func TestUnifiedReaderHugeTableCellPreservesRowAndHeaders(t *testing.T) {
	condition := strings.Repeat("条件仅适用订阅模式；", 1000) + "按年结算"
	body := `<main><table id="plans"><caption>Per request, USD</caption><tr><th>Mode</th><th>Conditions</th></tr><tr><td>Basic</td><td>` + condition + `</td></tr></table><script type="application/json">{"limits":{"amount":30,"unit":"per minute"}}</script></main>`
	s, run, _ := storedHTMLReader(t, body)
	info, err := buildHTMLInformation(body, "https://fixture.example/docs", "")
	if err != nil {
		t.Fatal(err)
	}
	var id string
	for _, block := range info.Blocks {
		if block.Kind == "table_row" {
			id = block.ID
		}
	}
	if id == "" {
		t.Fatal("no row locator")
	}
	row := boundedUnifiedRead(t, s, run, map[string]any{"handle": "source", "view": "content", "block_id": id})
	encoded, _ := json.Marshal(row)
	if !strings.Contains(string(encoded), "Basic") || !strings.Contains(string(encoded), "Conditions") || !strings.Contains(string(encoded), "Per request, USD") {
		t.Fatal("row detached from entity/unit/headers", string(encoded))
	}
	data := boundedUnifiedRead(t, s, run, map[string]any{"handle": "source", "view": "content", "block_id": id, "pointer": "/cells/1/text"})
	text := ""
	pages := 0
	for {
		pages++
		text += data["text"].(string)
		if data["next_cursor"] == nil {
			break
		}
		if pages >= 16 {
			t.Fatal("table cell did not progress")
		}
		data = boundedUnifiedRead(t, s, run, map[string]any{"cursor": data["next_cursor"]})
	}
	if text != condition {
		t.Fatal("long table condition truncated")
	}
	embedded := boundedUnifiedRead(t, s, run, map[string]any{"handle": "source", "view": "json", "block_id": "j0", "pointer": "/limits"})
	if embedded["value"].(map[string]any)["unit"] != "per minute" {
		t.Fatal("embedded literal JSON unreachable", embedded)
	}
	dom := boundedUnifiedRead(t, s, run, map[string]any{"handle": "source", "view": "dom", "selector": "#plans"})
	domBytes, _ := json.Marshal(dom)
	if !strings.Contains(string(domBytes), "table") || dom["structural_only"] != true {
		t.Fatal("Code as Action lost explicit structure", dom)
	}
	t.Logf("oversized cell reachable in %d pages, row context retained", pages)
}

func TestUnifiedNamedFieldsKeepExactReferencesForLargeChildren(t *testing.T) {
	condition := strings.Repeat("bounded condition ", 1500)
	body, _ := json.Marshal(map[string]any{"plans": []any{map[string]any{"conditions": condition, "unit": "per request"}}})
	s, run, _ := storedHTMLReader(t, string(body))
	data := boundedUnifiedRead(t, s, run, map[string]any{"handle": "source", "fields": []string{"conditions", "unit"}})
	fields := data["matches"].([]any)[0].(map[string]any)["fields"].(map[string]any)
	ref, ok := fields["conditions"].(map[string]any)
	if !ok || ref["read_with"] == nil || fields["unit"] != "per request" {
		t.Fatal("named discovery discarded field relation", data)
	}
	child := boundedUnifiedRead(t, s, run, ref["read_with"])
	if !strings.HasPrefix(condition, child["text"].(string)) {
		t.Fatal("named field reference not bound to original")
	}
}

func TestUnifiedNamedFieldCollectionCursorContinuesDiscovery(t *testing.T) {
	items := []any{}
	for n := 0; n < 9; n++ {
		items = append(items, map[string]any{"unit": "per request", "conditions": "mode-specific condition"})
	}
	body, _ := json.Marshal(items)
	s, run, _ := storedHTMLReader(t, string(body))
	data := boundedUnifiedRead(t, s, run, map[string]any{"handle": "source", "fields": []string{"unit", "conditions"}})
	total := len(data["matches"].([]any))
	for data["next_cursor"] != nil {
		data = boundedUnifiedRead(t, s, run, map[string]any{"cursor": data["next_cursor"]})
		total += len(data["matches"].([]any))
	}
	if total != 9 {
		t.Fatal("matched object cursor lost rows", total)
	}
}
