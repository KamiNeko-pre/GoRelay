package aiapp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/agentdata"
)

func TestHistoricalHTMLReaderRequiresBoundRestartInsteadOfRawReplay(t *testing.T) {
	for _, name := range []string{"read_data", "read_source_content", "read_observation"} {
		body := `<main><style>RAW_HISTORY_NOISE</style><p>1 credit per request.</p></main>`
		source, _ := toolData(ToolCall{ID: "source", Function: FunctionCall{Name: "inspect_source"}}, inspectResponse(acquisition.Response{Body: body, Receipt: acquisition.Receipt{URL: "https://fixture.example/", Status: 200}}, acquisition.Stats{}))
		sourceRaw, _ := json.Marshal(source)
		state := LoopState{Run: agentdata.Run{ID: "run", Goal: "read public rates"}, Budget: BudgetSnapshot{LoopVersion: 3}, Operations: []agentdata.Operation{{ID: 1, RunID: "run", Key: "tool:source", Kind: "inspect_source", Stage: "succeeded", ResultJSON: string(sourceRaw)}}}
		args := map[string]any{"handle": "source", "selector": "main", "offset": 2000}
		if name == "read_source_content" {
			args = map[string]any{"inspection_call_id": "source", "selector": "main", "offset": 2000}
		}
		if name == "read_observation" {
			args = map[string]any{"tool_call_id": "source", "pointer": "/data/source/body", "offset": 2000}
		}
		arguments, _ := json.Marshal(args)
		old, _ := toolData(ToolCall{ID: "old-read", Function: FunctionCall{Name: name}}, map[string]any{"text": body, "offset": 2000, "url": "https://fixture.example/", "next_offset": 4000})
		oldRaw, _ := json.Marshal(old)
		messages := []Message{{Role: "assistant", ToolCalls: []ToolCall{{ID: "old-read", Function: FunctionCall{Name: name, Arguments: string(arguments)}}}}, {Role: "tool", ToolCallID: "old-read", Content: string(oldRaw)}}
		got, err := CompactContext(state, messages)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, m := range got {
			if m.Role == "tool" && m.ToolCallID == "old-read" {
				if strings.Contains(m.Content, "RAW_HISTORY_NOISE") {
					t.Fatal("old source reader bypassed representation boundary", name)
				}
				var receipt ToolResult
				_ = json.Unmarshal([]byte(m.Content), &receipt)
				var data map[string]any
				_ = json.Unmarshal(receipt.Data, &data)
				ref, ok := data["read_with"].(map[string]any)
				if !ok || ref["handle"] != "source" || ref["offset"] != float64(0) {
					t.Fatal("legacy HTML continuation lacks bound restart", name, m.Content)
				}
				found = true
			}
		}
		if !found {
			t.Fatal("reader reply/pairing disappeared", name)
		}
		if state.Operations[0].ResultJSON != string(sourceRaw) {
			t.Fatal("canonical source was overwritten")
		}
	}
}

func TestSavedModelRequestsRespectCurrentInformationBoundary(t *testing.T) {
	directory := os.Getenv("GORELAY_HTML_REPLAY_DIR")
	if directory == "" {
		t.Skip("saved request replay not configured")
	}
	raw, err := os.ReadFile(filepath.Join(directory, "api-snapshot-before.json"))
	if err != nil {
		t.Fatal(err)
	}
	var captured struct {
		Run        agentdata.Run         `json:"run"`
		Operations []agentdata.Operation `json:"operations"`
	}
	if err := json.Unmarshal(raw, &captured); err != nil {
		t.Fatal(err)
	}
	state := LoopState{Run: captured.Run, Operations: captured.Operations}
	if err := json.Unmarshal([]byte(captured.Run.BudgetJSON), &state.Budget); err != nil {
		t.Fatal(err)
	}
	var spec TaskSpec
	if err := json.Unmarshal([]byte(captured.Run.SpecJSON), &spec); err != nil {
		t.Fatal(err)
	}
	state.Spec = &spec
	paths, err := filepath.Glob(filepath.Join(directory, "model-requests-before", "*.request.json"))
	if err != nil || len(paths) != 8 {
		t.Fatal("frozen request inventory changed", len(paths), err)
	}
	measurements := []map[string]any{}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var request ChatRequest
		if err := json.Unmarshal(raw, &request); err != nil {
			t.Fatal(err)
		}
		if len(request.Messages) < 2 {
			measurements = append(measurements, map[string]any{"capture": filepath.Base(path), "kind": "auxiliary_readiness_request", "not_a_business_request": true})
			continue
		}
		// Each request keeps its actual pending call/reply order. The persisted
		// source ledger supplies originals, never replacement gold or URLs.
		got, err := CompactContext(state, request.Messages)
		if err != nil {
			t.Fatal(filepath.Base(path), err)
		}
		if contextRequestBytes(state, got) > 64<<10 {
			t.Fatal("whole request budget exceeded")
		}
		for _, message := range got {
			if message.Role != "tool" {
				continue
			}
			if strings.Contains(message.Content, `\u003csvg`) || strings.Contains(message.Content, `\u003cstyle`) {
				t.Fatal("saved model request still receives raw HTML", filepath.Base(path))
			}
		}
		packed, _ := json.Marshal(ChatRequest{Messages: got, Tools: runToolDefinitions(state.Run), MaxCompletionTokens: 4096})
		measurements = append(measurements, map[string]any{"capture": filepath.Base(path), "before_bytes": len(raw), "after_bytes": len(packed), "new_http_requests": 0, "model_calls": 0, "semantic_correctness_scored": false})
	}
	output, _ := json.MarshalIndent(measurements, "", "  ")
	if err := os.WriteFile(filepath.Join(directory, "model-request-replay-measurements.json"), output, 0600); err != nil {
		t.Fatal(err)
	}
	t.Logf("replayed frozen request inventory: %d files; no HTTP/models", len(paths))
}

func TestPersistedHistoricalReaderCannotRestoreRawHTML(t *testing.T) {
	for _, body := range []string{`<main><style>OLD_RAW_STYLE</style><p>1 credit</p></main>`, `<article><script>OLD_RAW_SCRIPT</script><p>2 requests</p></article>`, `{"literal":"<p>JSON literal remains unchanged</p>"}`} {
		s, run, _ := storedHTMLReader(t, body)
		for _, name := range []string{"read_data", "read_source_content", "read_observation"} {
			args := map[string]any{"handle": "source", "offset": 2000}
			if name == "read_source_content" {
				args = map[string]any{"inspection_call_id": "source", "offset": 2000}
			}
			if name == "read_observation" {
				args = map[string]any{"tool_call_id": "source", "pointer": "/data/source/body", "offset": 2000}
			}
			arguments, _ := json.Marshal(args)
			originalCall := ToolCall{ID: name, Function: FunctionCall{Name: name, Arguments: string(arguments)}}
			messages := []Message{{Role: "assistant", ToolCalls: []ToolCall{originalCall}}}
			rawPlan, _ := json.Marshal(messages)
			run.PlanJSON = string(rawPlan)
			old, _ := toolData(originalCall, map[string]any{"text": body, "offset": 2000})
			rawOld, _ := json.Marshal(old)
			if err := s.Data.DB.Create(&agentdata.Operation{RunID: run.ID, Key: "tool:" + name, Kind: name, Stage: "succeeded", ResultJSON: string(rawOld)}).Error; err != nil {
				t.Fatal(err)
			}
			for _, request := range []map[string]any{{"handle": name}, {"handle": name, "view": "json"}, {"handle": name, "fields": []string{"text"}}} {
				a, _ := json.Marshal(request)
				out, err := s.readData(context.Background(), run, ToolCall{Function: FunctionCall{Name: "read_data", Arguments: string(a)}})
				if err != nil {
					t.Fatal(name, err)
				}
				if strings.Contains(string(out.Data), "OLD_RAW_") {
					t.Fatal("persisted reader bypasses view boundary", name, string(a))
				}
				if strings.HasPrefix(body, "{") && !strings.Contains(string(out.Data), "JSON literal remains unchanged") {
					t.Fatal("JSON value lost", name)
				}
			}
		}
	}
}

func TestHistoricalJSONReaderRetainsLiteralMarkupAsJSONData(t *testing.T) {
	data, _ := toolData(ToolCall{ID: "source", Function: FunctionCall{Name: "inspect_source"}}, inspectResponse(acquisition.Response{Body: `{"literal":"<p>raw markup is a JSON value</p>"}`, Receipt: acquisition.Receipt{URL: "https://fixture.example/"}}, acquisition.Stats{}))
	raw, _ := json.Marshal(data)
	state := LoopState{Run: agentdata.Run{ID: "run", Goal: "read literal"}, Budget: BudgetSnapshot{LoopVersion: 3}, Operations: []agentdata.Operation{{RunID: "run", Key: "tool:source", Kind: "inspect_source", Stage: "succeeded", ResultJSON: string(raw)}}}
	old, _ := toolData(ToolCall{ID: "read", Function: FunctionCall{Name: "read_data"}}, map[string]any{"text": "<p>raw markup is a JSON value</p>"})
	oldRaw, _ := json.Marshal(old)
	got, err := CompactContext(state, []Message{{Role: "assistant", ToolCalls: []ToolCall{{ID: "read", Function: FunctionCall{Name: "read_data", Arguments: `{"handle":"source","fields":["literal"]}`}}}}, {Role: "tool", ToolCallID: "read", Content: string(oldRaw)}})
	if err != nil {
		t.Fatal(err)
	}
	if got[len(got)-1].Content != string(oldRaw) {
		t.Fatal("JSON data mistaken for document HTML")
	}
}
