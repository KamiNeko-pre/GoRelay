package aiapp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// Regression for A0-attempt-3 round 11: the detail page body (1,848 runes)
// came back as {"omitted":true,"read_with":{cursor}} and the model never
// followed the cursor. The continuation path must be reachable: read the
// block, then follow the returned cursor to recover the full body.
func TestHTMLDetailBodyOmittedCursorRecoversFullText(t *testing.T) {
	longBody := strings.Repeat("公开采购项目，预算与适用条件必须完整保留。", 300)
	body := `<html><body><div class="v_news_content"><p>` + longBody + `</p></div></body></html>`
	s, run, original := storedHTMLReader(t, body)
	var receipt ToolResult
	_ = json.Unmarshal([]byte(original), &receipt)
	// Block identity comes from the initial inspection; content reads return
	// bounded text/value pages, rather than repeating the inspection envelope.
	var inspection sourceInspection
	_ = json.Unmarshal(receipt.Data, &inspection)
	if inspection.Information == nil || len(inspection.Information.Blocks) == 0 {
		t.Fatal("initial source inspection has no blocks")
	}
	id := inspection.Information.Blocks[0].ID
	args := map[string]any{"handle": "source", "view": "content", "block_id": id}
	joined := ""
	for n := 0; n < 40; n++ {
		raw, _ := json.Marshal(args)
		out, err := s.readData(context.Background(), run, ToolCall{ID: "read", Function: FunctionCall{Name: "read_data", Arguments: string(raw)}})
		if err != nil {
			t.Fatal(err)
		}
		if len(out.Data) > observationBytes {
			t.Fatal("unbounded continuation")
		}
		var data map[string]any
		_ = json.Unmarshal(out.Data, &data)
		if value, ok := data["value"].(map[string]any); ok {
			if ref, ok := value["text"].(map[string]any); ok {
				if readWith, ok := ref["read_with"].(map[string]any); ok {
					args = readWith
					continue
				}
			}
			if text, ok := value["text"].(string); ok {
				joined += text
			}
		}
		if text, ok := data["text"].(string); ok {
			joined += text
		}
		if cursor, ok := data["next_cursor"].(string); ok && cursor != "" {
			args = map[string]any{"cursor": cursor}
			continue
		}
		if normalizeEvidenceText(joined) != normalizeEvidenceText(longBody) {
			t.Fatal("complete original text was not recovered", len([]rune(joined)), len([]rune(longBody)))
		}
		return
	}
	t.Fatal("continuation did not converge")
}
