package aiapp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"go-lua-crawler/internal/agentdata"
)

func TestSourceSelectorReadsBoundSerializedBytesAndResumeExactText(t *testing.T) {
	text := strings.Repeat("需要保留的计费条件；", 900)
	s, run, _ := storedHTMLReader(t, `<main id="source"><p>`+text+`</p></main>`)
	for _, name := range []string{"read_data", "read_source_content"} {
		args := map[string]any{"handle": "source", "selector": "#source"}
		if name == "read_source_content" {
			args = map[string]any{"inspection_call_id": "source", "selector": "#source", "offset": 0}
		}
		got := ""
		pages := 0
		for {
			arguments, _ := json.Marshal(args)
			call := ToolCall{ID: "read", Function: FunctionCall{Name: name, Arguments: string(arguments)}}
			var out ToolResult
			var err error
			if name == "read_data" {
				out, err = s.readData(context.Background(), run, call)
			} else {
				out, err = s.readSourceContent(context.Background(), run, call)
			}
			if err != nil {
				t.Fatal(name, err)
			}
			if len(out.Data) > observationBytes {
				t.Fatal("selector reader exceeded serialized limit", name, len(out.Data))
			}
			var data map[string]any
			_ = json.Unmarshal(out.Data, &data)
			got += data["text"].(string)
			pages++
			if data["new_http_requests"] != float64(0) {
				t.Fatal("read caused HTTP")
			}
			if name == "read_data" {
				if data["next_cursor"] == nil {
					break
				}
				args = map[string]any{"cursor": data["next_cursor"]}
			} else {
				if data["next_read"] == nil {
					break
				}
				args = data["next_read"].(map[string]any)
			}
			if pages > 16 {
				t.Fatal("selector failed to make progress")
			}
		}
		info, _ := buildHTMLInformation(`<main id="source"><p>`+text+`</p></main>`, "https://fixture.example/docs", "#source")
		if got != info.Text() {
			t.Fatal("adaptive byte bound lost text", name)
		}
	}
}

func TestDefaultReceiptReadIncludesEnvelopeInByteBudget(t *testing.T) {
	s, run, _ := storedHTMLReader(t, `<main><p>visible source</p></main>`)
	data, _ := json.Marshal(map[string]any{"padding": strings.Repeat("x", 5990)})
	out := ToolResult{CallID: "record-feedback", Name: "get_task_results", Data: data}
	raw, _ := json.Marshal(out)
	if err := s.Data.DB.Create(&agentdata.Operation{RunID: run.ID, Key: "tool:record-feedback", Kind: "get_task_results", Stage: "succeeded", ResultJSON: string(raw)}).Error; err != nil {
		t.Fatal(err)
	}
	run.BudgetJSON = `{"loop_version":3}`
	result, err := s.readData(context.Background(), run, ToolCall{Function: FunctionCall{Name: "read_data", Arguments: `{"handle":"record-feedback"}`}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Data) > observationBytes {
		t.Fatal("default wrapper escaped the budget", len(result.Data))
	}
	if !strings.Contains(string(result.Data), "record-feedback") {
		t.Fatal("receipt reference lost")
	}
}
