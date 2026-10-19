package aiapp

import (
	"encoding/json"
	"strings"
	"testing"
)

// A validation probe must report its result to the caller. The recorded run
// produced 13 successful probes whose receipts had degraded to
// observation=null with reads[0].total=0, so the caller never learned what its
// program returned and rewrote the same program until the watchdog cancelled it.
func TestProbeFeedbackKeepsItsObservation(t *testing.T) {
	// A probe whose result carries a long field, the shape that used to be
	// degraded: L3 needs an article body, so its output exceeds the page budget.
	body := strings.Repeat("正文内容。", 900) // ~5.4k runes, well past 6 KiB raw
	payload, _ := json.Marshal(map[string]any{
		"data": map[string]any{
			"items": []any{
				map[string]any{"title": "公告一", "body": body},
				map[string]any{"title": "公告二", "body": body},
			},
		},
	})
	out := ToolResult{CallID: "call_probe_1", Name: "validate_program", Data: payload}

	view := observationWithLimit(out, observationBytes, &TaskSpec{
		RequiredFields: []FieldRequirement{{Name: "title", Type: "string"}, {Name: "body", Type: "source_text"}},
	})

	var got map[string]any
	if err := json.Unmarshal(view.Data, &got); err != nil {
		t.Fatal(err)
	}
	sample, hasSample := got["observation"]
	if !hasSample || sample == nil {
		t.Fatalf("probe receipt lost its observation: %s", trunc(view.Data))
	}
	// The caller must be able to see the field names it declared.
	raw, _ := json.Marshal(sample)
	if !strings.Contains(string(raw), "title") {
		t.Fatalf("probe sample does not show the declared field: %s", trunc(raw))
	}
	// And the receipt must not claim the container is empty.
	if reads, ok := got["reads"].([]any); ok && len(reads) > 0 {
		if entry, ok := reads[0].(map[string]any); ok {
			if total, ok := entry["total"].(float64); ok && total == 0 {
				t.Fatalf("probe receipt reports total=0 while carrying records: %s", trunc(view.Data))
			}
		}
	}
	t.Logf("probe receipt %d bytes, observation present", len(view.Data))
}

// A page read keeps the smaller document budget: it is re-read through the
// pager, so letting it grow would make every round more expensive.
func TestPageReadKeepsDocumentBudget(t *testing.T) {
	body := strings.Repeat("页面正文。", 900)
	payload, _ := json.Marshal(map[string]any{
		"handle": "call_read_1", "text": body, "total_runes": len([]rune(body)),
	})
	out := ToolResult{CallID: "call_read_1", Name: "read_data", Data: payload}
	view := observationWithLimit(out, observationBytes)
	if len(view.Data) > observationBytes {
		t.Fatalf("a page read grew past its document budget: %d bytes", len(view.Data))
	}
	if !probeFeedback("validate_program") {
		t.Fatal("validate_program must count as probe feedback")
	}
	if probeFeedback("read_data") || probeFeedback("inspect_source") {
		t.Fatal("page readers must not receive the probe budget")
	}
}

func trunc(b []byte) string {
	if len(b) > 300 {
		return string(b[:300])
	}
	return string(b)
}
