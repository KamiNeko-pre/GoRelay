package aiapp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/knowledge"
)

func TestKnowledgeHandleReadsExactPathAndBoundedContinuation(t *testing.T) {
	s, run, _ := storedHTMLReader(t, `<main><p>source</p></main>`)
	text := strings.Repeat("需保留的适用条件；", 900)
	reads := 0
	s.ReadOnly = func(ctx context.Context, call ToolCall, reader EvidenceReader) (ToolResult, error) {
		var args struct {
			ID string `json:"evidence_id"`
		}
		_ = json.Unmarshal([]byte(call.Function.Arguments), &args)
		if args.ID != "ev_owned" {
			return ToolResult{}, knowledge.ErrInvalidReference
		}
		reads++
		return toolData(call, map[string]any{"id": args.ID, "text": text, "unit": "request", "url": "https://fixture.example/docs"})
	}
	args := map[string]any{"handle": "ev_owned", "view": "json", "pointer": "/text"}
	joined := ""
	for page := 0; ; page++ {
		raw, _ := json.Marshal(args)
		out, err := s.readData(context.Background(), run, ToolCall{ID: "read", Type: "function", Function: FunctionCall{Name: "read_data", Arguments: string(raw)}})
		if err != nil {
			t.Fatal(err)
		}
		if len(out.Data) > observationBytes {
			t.Fatal("knowledge path ignored byte bound", len(out.Data))
		}
		var data map[string]any
		_ = json.Unmarshal(out.Data, &data)
		part, ok := data["text"].(string)
		if !ok {
			t.Fatal("knowledge pointer ignored", string(out.Data))
		}
		joined += part
		if data["next_cursor"] == nil {
			break
		}
		if page > 15 {
			t.Fatal("knowledge pagination makes no progress")
		}
		args = map[string]any{"cursor": data["next_cursor"]}
	}
	if joined != text || reads < 2 {
		t.Fatal("knowledge condition not completely recovered")
	}
	for _, a := range []string{`{"handle":"ev_foreign","view":"json","pointer":"/text"}`, `{"handle":"ev_owned","run_id":"other","view":"json","pointer":"/text"}`} {
		if _, err := s.readData(context.Background(), run, ToolCall{ID: "foreign", Type: "function", Function: FunctionCall{Name: "read_data", Arguments: a}}); err == nil {
			t.Fatal("evidence ownership bypass", a)
		}
	}
}

func TestLegacyObservationLargeChildHasReachableBoundedReference(t *testing.T) {
	s, run, _ := storedHTMLReader(t, `<main><p>source</p></main>`)
	text := strings.Repeat("大型嵌套计费条件；", 900)
	receipt, _ := toolData(ToolCall{ID: "feedback", Function: FunctionCall{Name: "get_task_results"}}, map[string]any{"entity": "Alpha", "details": map[string]any{"unit": "request", "condition": text}})
	raw, _ := json.Marshal(receipt)
	if err := s.Data.DB.Create(&agentdata.Operation{RunID: run.ID, Key: "tool:feedback", Kind: "get_task_results", Stage: "succeeded", ResultJSON: string(raw)}).Error; err != nil {
		t.Fatal(err)
	}
	out, err := s.readObservation(context.Background(), run, ToolCall{ID: "legacy", Function: FunctionCall{Name: "read_observation", Arguments: `{"tool_call_id":"feedback","pointer":"/data"}`}})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Data) > observationBytes {
		t.Fatal("legacy object child exceeds byte bound", len(out.Data))
	}
	var data struct {
		Value map[string]json.RawMessage `json:"value"`
	}
	_ = json.Unmarshal(out.Data, &data)
	var ref struct {
		ReadWith map[string]any `json:"read_with"`
	}
	_ = json.Unmarshal(data.Value["details"], &ref)
	if ref.ReadWith == nil {
		t.Fatal("large child has no reference")
	}
	args, _ := json.Marshal(ref.ReadWith)
	child, err := s.readData(context.Background(), run, ToolCall{Function: FunctionCall{Name: "read_data", Arguments: string(args)}})
	if err != nil {
		t.Fatal("returned reference unusable", err)
	}
	if !strings.Contains(string(child.Data), "request") {
		t.Fatal(fmt.Sprintf("unit unreachable: %s", child.Data))
	}
	root, err := s.readObservation(context.Background(), run, ToolCall{Function: FunctionCall{Name: "read_observation", Arguments: `{"tool_call_id":"feedback"}`}})
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Value map[string]json.RawMessage `json:"value"`
	}
	_ = json.Unmarshal(root.Data, &envelope)
	_ = json.Unmarshal(envelope.Value["data"], &ref)
	if ref.ReadWith == nil {
		t.Fatal("envelope data lost its reference")
	}
	args, _ = json.Marshal(ref.ReadWith)
	if _, err := s.readData(context.Background(), run, ToolCall{Function: FunctionCall{Name: "read_data", Arguments: string(args)}}); err != nil {
		t.Fatal("envelope child path not rebound to data", err)
	}
}
