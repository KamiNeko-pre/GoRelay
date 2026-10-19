package aiapp

import (
	"context"
	"encoding/json"
	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/agentdata"
	"os"
	"strings"
	"testing"
	"time"
)

func TestInspectionProjectionKeepsReadableLocatedBlocks(t *testing.T) {
	fixture, err := os.ReadFile("testdata/inspection-large-list.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{"Procurement opportunity", "Game discount"} {
		body := `<nav>` + strings.Repeat(`<div><a href="/navigation">Navigation entry with extra descriptive context</a></div>`, 80) + `</nav><main><h1>Catalog</h1><ul>` + strings.Repeat(`<li><a href="/detail">`+label+`</a></li>`, 80) + `</ul></main>`
		if label == "Procurement opportunity" {
			body = string(fixture)
			label = "采购公告"
		}
		p := inspectResponse(acquisition.Response{Receipt: acquisition.Receipt{Method: "GET", URL: "https://fixture.example/list", Status: 200, FetchedAt: time.Now()}, Body: body}, acquisition.Stats{})
		out, _ := toolData(ToolCall{ID: "source", Function: FunctionCall{Name: "inspect_source"}}, p)
		budget, _ := json.Marshal(BudgetSnapshot{LoopVersion: 5})
		got := modelObservationForRun(agentdata.Run{BudgetJSON: string(budget)}, out, nil)
		var visible struct {
			Information *HTMLInformation `json:"information"`
		}
		_ = json.Unmarshal(got.Data, &visible)
		if visible.Information == nil || len(visible.Information.Blocks) == 0 {
			t.Fatalf("located blocks hidden behind another receipt read: %s", got.Data)
		}
		for _, b := range visible.Information.Blocks {
			if b.Selector == "" || b.ReadWith["block_id"] != b.ID {
				t.Fatal("unusable locator", b)
			}
		}
		if len(got.Data) > readerObservationBytes {
			t.Fatal("oversize inspection")
		}
		if !strings.Contains(string(got.Data), label) || !strings.Contains(string(got.Data), `"selector"`) {
			t.Fatalf("readable source and locator vanished: %s", got.Data)
		}
		if strings.Contains(string(got.Data), `<li>`) {
			t.Fatal("raw HTML exposed")
		}
	}
}

func TestLocatedInspectionReferencesExecuteAgainstCanonicalSource(t *testing.T) {
	body, err := os.ReadFile("testdata/inspection-large-list.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, html := range []string{string(body), strings.ReplaceAll(string(body), "采购公告", "Game catalog")} {
		s, r, raw := storedHTMLReader(t, html)
		r.BudgetJSON = `{"loop_version":5}`
		var original ToolResult
		_ = json.Unmarshal([]byte(raw), &original)
		visible := modelObservationForRun(r, original, nil)
		var data struct{ Information HTMLInformation }
		if err := json.Unmarshal(visible.Data, &data); err != nil {
			t.Fatal(err)
		}
		for _, b := range data.Information.Blocks {
			args, _ := json.Marshal(b.ReadWith)
			out, err := s.readData(context.Background(), r, ToolCall{ID: "read", Function: FunctionCall{Name: "read_data", Arguments: string(args)}})
			if err != nil {
				t.Fatal("advertised block read failed", err)
			}
			var read map[string]any
			_ = json.Unmarshal(out.Data, &read)
			if read["selector"] != b.Selector {
				t.Fatal("locator changed across representation boundary")
			}
			other := r
			other.ID = "foreign"
			if _, err := s.readData(context.Background(), other, ToolCall{Function: FunctionCall{Arguments: string(args)}}); err == nil {
				t.Fatal("cross-run access accepted")
			}
		}
	}
}

func TestReaderFeedbackKeepsSourceAndContinuationUnderMemoryPressure(t *testing.T) {
	for _, text := range []string{strings.Repeat("Notice text ", 1000), strings.Repeat("Game description ", 1000)} {
		raw, _ := json.Marshal(map[string]any{"handle": "page", "text": text, "offset": 0, "view_version": htmlInformationVersion, "next_read": map[string]any{"continue_from": "read-one"}})
		out := ToolResult{CallID: "read-one", Name: "read_data", Data: raw}
		budget, _ := json.Marshal(BudgetSnapshot{LoopVersion: 5})
		full, _ := json.Marshal(out)
		state := LoopState{Run: agentdata.Run{ID: "run", BudgetJSON: string(budget)}, Budget: BudgetSnapshot{LoopVersion: 5}, Operations: []agentdata.Operation{{RunID: "run", Key: "tool:read-one", Kind: "read_data", Stage: "succeeded", ResultJSON: string(full)}}}
		feedback := recentToolFeedback(state)
		if len(feedback) != 1 {
			t.Fatal("missing feedback")
		}
		var got struct {
			Handle   string
			Text     string
			NextRead map[string]any `json:"next_read"`
		}
		_ = json.Unmarshal(feedback[0].Data, &got)
		if got.Handle != "page" || got.Text == "" || got.NextRead["continue_from"] != "read-one" {
			t.Fatalf("memory lost source identity/content/continuation: %s", feedback[0].Data)
		}
	}
}
