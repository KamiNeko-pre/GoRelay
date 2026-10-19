package aiapp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/testdb"
)

func storedHTMLReader(t *testing.T, body string) (*Service, agentdata.Run, string) {
	t.Helper()
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	r, err := s.Create(context.Background(), CreateRunRequest{RequestKey: "html-reader", Goal: "read source information"})
	if err != nil {
		t.Fatal(err)
	}
	inspection := inspectResponse(acquisition.Response{Receipt: acquisition.Receipt{Method: "GET", URL: "https://fixture.example/docs", Status: 200, FetchedAt: time.Now()}, Body: body}, acquisition.Stats{Requests: 1, Bytes: int64(len(body))})
	observed, _ := toolData(ToolCall{ID: "source", Function: FunctionCall{Name: "inspect_source"}}, inspection)
	raw, _ := json.Marshal(observed)
	if err := s.Data.DB.Create(&agentdata.Operation{RunID: r.ID, Key: "tool:source", Kind: "inspect_source", Stage: "succeeded", ResultJSON: string(raw)}).Error; err != nil {
		t.Fatal(err)
	}
	return s, r, string(raw)
}

func TestHTMLContentReaderVersionsAndSourceBoundary(t *testing.T) {
	body := `<main id="data"><pre>` + strings.Repeat("example = {}\n", 3000) + `</pre><p>Basic: 1 credit per request, only basic searches.</p><p>` + strings.Repeat("Long documented condition. ", 200) + `</p></main>`
	s, run, original := storedHTMLReader(t, body)
	ctx := context.Background()
	read := func(args any) (map[string]any, error) {
		raw, _ := json.Marshal(args)
		out, err := s.readSourceContent(ctx, run, ToolCall{ID: "read", Function: FunctionCall{Name: "read_source_content", Arguments: string(raw)}})
		data := map[string]any{}
		_ = json.Unmarshal(out.Data, &data)
		return data, err
	}
	first, err := read(map[string]any{"inspection_call_id": "source", "selector": "#data", "offset": 0})
	if err != nil || first["view_version"] != htmlInformationVersion || !strings.Contains(first["text"].(string), "1 credit per request") || strings.Contains(first["text"].(string), "example =") || first["new_http_requests"] != float64(0) {
		t.Fatal("raw/code noise still displaced main meaning", first, err)
	}
	if _, err := read(first["next_read"]); err != nil {
		t.Fatal("versioned continuation failed", err)
	}
	if _, err := read(map[string]any{"inspection_call_id": "source", "selector": "#data", "offset": 2000}); err == nil {
		t.Fatal("old raw HTML offset silently became a semantic offset")
	}
	if _, err := read(map[string]any{"inspection_call_id": "source", "offset": 0, "view_version": "missing-version"}); err == nil {
		t.Fatal("unknown representation silently accepted")
	}
	other := run
	other.ID = agentdata.Token()
	if _, err := s.readSourceContent(ctx, other, ToolCall{Function: FunctionCall{Arguments: `{"inspection_call_id":"source","offset":0}`}}); err == nil {
		t.Fatal("another Run borrowed the receipt")
	}
	var op agentdata.Operation
	if err := s.Data.DB.Where("run_id = ? AND `key` = ?", run.ID, "tool:source").First(&op).Error; err != nil || op.ResultJSON != original {
		t.Fatal("reading overwrote canonical original", err)
	}
}

// An opt-in local development replay; never count it as held-out or autonomous.
func TestSavedHTMLFailureInformationReplay(t *testing.T) {
	directory := os.Getenv("GORELAY_HTML_REPLAY_DIR")
	if directory == "" {
		t.Skip("saved HTML replay: GORELAY_HTML_REPLAY_DIR not configured")
	}
	raw, err := os.ReadFile(filepath.Join(directory, "inspection-receipt.json"))
	if err != nil {
		t.Fatal(err)
	}
	var receipt ToolResult
	var original sourceInspection
	if err := json.Unmarshal(raw, &receipt); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(receipt.Data, &original); err != nil {
		t.Fatal(err)
	}
	observations, err := os.ReadFile(filepath.Join(directory, "observations-before.json"))
	if err != nil {
		t.Fatal(err)
	}
	var old []struct {
		Receipt struct {
			Data struct {
				Selector string `json:"selector"`
			} `json:"data"`
		} `json:"Receipt"`
	}
	if err := json.Unmarshal(observations, &old); err != nil || len(old) != 3 {
		t.Fatal("saved three-page sequence unavailable", err)
	}
	selector := old[0].Receipt.Data.Selector
	view, err := buildHTMLInformation(original.Source.Body, original.Source.Receipt.URL, selector)
	if err != nil || view == nil {
		t.Fatal("saved DOM region unavailable", err)
	}
	s, run, _ := storedHTMLReader(t, original.Source.Body)
	var op agentdata.Operation
	if err := s.Data.DB.Where("run_id = ? AND `key` = ?", run.ID, "tool:source").First(&op).Error; err != nil {
		t.Fatal(err)
	}
	// Use the exact original URL/receipt in the test ledger, not a live GET.
	receipt.Data, _ = json.Marshal(original)
	receiptRaw, _ := json.Marshal(receipt)
	if err := s.Data.DB.Model(&op).Update("result_json", string(receiptRaw)).Error; err != nil {
		t.Fatal(err)
	}
	pages := []map[string]any{}
	args := map[string]any{"inspection_call_id": "source", "selector": selector, "offset": 0}
	readText := ""
	for n := 0; n < 3; n++ {
		arguments, _ := json.Marshal(args)
		out, err := s.readSourceContent(context.Background(), run, ToolCall{ID: "replay", Function: FunctionCall{Name: "read_source_content", Arguments: string(arguments)}})
		if err != nil {
			t.Fatal(err)
		}
		var data map[string]any
		if err := json.Unmarshal(out.Data, &data); err != nil {
			t.Fatal(err)
		}
		if data["new_http_requests"] != float64(0) || data["url"] != original.Source.Receipt.URL {
			t.Fatal("replay escaped original receipt", data)
		}
		readText += data["text"].(string)
		pages = append(pages, data)
		if data["next_read"] == nil {
			break
		}
		args = data["next_read"].(map[string]any)
	}
	if !strings.Contains(strings.ToLower(readText), "credit") || strings.Contains(readText, "<svg") || strings.Contains(readText, "<style") {
		t.Fatalf("same three-page budget still lost relevant information: %.500s", readText)
	}
	retained := map[string]bool{}
	for key, required := range map[string]string{"basic_credit_and_unit": "basic, fast, ultra-fast: 1 API Credit", "advanced_credit_and_unit": "advanced: 2 API Credits", "mode_field": "search_depth"} {
		retained[key] = strings.Contains(readText, required)
		if !retained[key] {
			t.Fatal("saved field/unit relation not reachable in same page budget", key)
		}
	}
	for key, required := range map[string]string{"automatic_upgrade_condition": "search_depth may be automatically set to advanced", "per_request_unit": "2 API credits per request", "override_condition": "explicitly set search_depth to basic"} {
		retained[key] = strings.Contains(view.Text(), required)
		if !retained[key] {
			t.Fatal("projection silently dropped a billing condition", key)
		}
	}
	inspection := inspectResponse(original.Source, original.ActualStats)
	observed, _ := toolData(ToolCall{ID: "source", Function: FunctionCall{Name: "inspect_source"}}, inspection)
	projected := modelObservation(observed)
	if len(projected.Data) > observationBytes || inspection.Source.Body != original.Source.Body {
		t.Fatal("model observation exceeded budget or original changed")
	}
	measurements := map[string]any{"mode": "offline_development_replay", "original_bytes": len(original.Source.Body), "projection_bytes": len(view.Text()), "projection_runes": len([]rune(view.Text())), "blocks": view.TotalBlocks, "embedded_json": len(view.Embedded), "model_observation_bytes": len(projected.Data), "page_count": len(pages), "new_http_requests": 0, "model_calls": 0, "credit_text_reachable": true, "semantic_correctness_scored": false, "selector": selector}
	measurements["retained_relations"] = retained
	for name, value := range map[string]any{"information-measurements.json": measurements, "pages-after.json": pages, "inspection-model-after.json": projected} {
		data, _ := json.MarshalIndent(value, "", "  ")
		if err := os.WriteFile(filepath.Join(directory, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(directory, "selected-information-after.txt"), []byte(view.Text()), 0600); err != nil {
		t.Fatal(err)
	}
	t.Logf("replay measurements: %v", measurements)
}
