package aiapp

import (
	"context"
	"encoding/json"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/testdb"
	"strings"
	"testing"
	"time"
)

func TestAllLargeObservationsHaveBoundedSamplesAndRecoverableHandles(t *testing.T) {
	for _, name := range []string{"get_task_results", "evaluate_result", "search_knowledge"} {
		rows := []any{}
		for i := 0; i < 25; i++ {
			rows = append(rows, map[string]any{"record_key": "record-identity", "fields": map[string]any{"body": strings.Repeat("正文", 10000)}})
		}
		raw, _ := json.Marshal(map[string]any{"records": rows})
		got := boundedObservation(ToolResult{Name: name, CallID: "original", Data: raw})
		if len(got.Data) > observationBytes || !strings.Contains(string(got.Data), "original") || !strings.Contains(string(got.Data), "read_observation") {
			t.Fatalf("unbounded/lost handle: %d", len(got.Data))
		}
		if len(raw) < 100000 {
			t.Fatal("fixture not large")
		}
	}
	small := ToolResult{Data: json.RawMessage(`{"version":1}`)}
	if string(boundedObservation(small).Data) != string(small.Data) {
		t.Fatal("small result changed")
	}
}

func TestReadObservationUsesOriginalRunReceiptWithoutExternalCalls(t *testing.T) {
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r, err := s.Create(ctx, CreateRunRequest{RequestKey: "read-observation", Goal: "read acquired records"})
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Repeat("正文", 2500)
	data, _ := json.Marshal(map[string]any{"records": []any{map[string]any{"fields": map[string]any{"body": text}}}})
	receipt, _ := json.Marshal(ToolResult{CallID: "original", Name: "get_task_results", Data: data})
	if err := s.Data.DB.Create(&agentdata.Operation{RunID: r.ID, Key: "tool:original", Kind: "get_task_results", Stage: "succeeded", InputHash: "input", ResultJSON: string(receipt)}).Error; err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"tool_call_id": "original", "pointer": "/data/records/0/fields/body", "offset": 2000}
	raw, _ := json.Marshal(args)
	call := ToolCall{ID: "read", Type: "function", Function: FunctionCall{Name: "read_observation", Arguments: string(raw)}}
	out, err := s.readObservation(ctx, r, call)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Text     string `json:"text"`
		Next     int    `json:"next_offset"`
		Requests int    `json:"new_http_requests"`
	}
	if err := json.Unmarshal(out.Data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Next <= 2000 || got.Text != string([]rune(text)[2000:got.Next]) || got.Requests != 0 {
		t.Fatal("original text/page changed")
	}
	foreign, err := s.Data.CreateRun(ctx, "foreign", "other goal", "{}", "{}", time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.readObservation(ctx, foreign, call); err == nil {
		t.Fatal("foreign receipt read")
	}
}

func TestObservationPointerPreservesTextAndRejectsInvalidPaths(t *testing.T) {
	value := map[string]any{"data": map[string]any{"records": []any{map[string]any{"a/b": "full unchanged text"}}}}
	got, err := jsonPointer(value, "/data/records/0/a~1b")
	if err != nil || got != "full unchanged text" {
		t.Fatal(got, err)
	}
	for _, path := range []string{"data", "/data/records/9", "/data/missing"} {
		if _, err := jsonPointer(value, path); err == nil {
			t.Fatal("invalid path accepted", path)
		}
	}
}

func TestReadObservationRecoversCommittedFailureButNotUnknownAttempt(t *testing.T) {
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r, err := s.Create(ctx, CreateRunRequest{RequestKey: "failed-receipts", Goal: "read execution feedback"})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"validate_program", "discover_sources"} {
		receipt, _ := json.Marshal(ToolResult{CallID: name, Name: name, Error: "original committed failure", Data: json.RawMessage(`{"feedback":"complete failure detail"}`)})
		if err = s.Data.DB.Create(&agentdata.Operation{RunID: r.ID, Key: "tool:" + name, Kind: name, InputHash: name, Stage: "failed", ResultJSON: string(receipt)}).Error; err != nil {
			t.Fatal(err)
		}
		args, _ := json.Marshal(map[string]any{"tool_call_id": name, "pointer": "/error"})
		call := ToolCall{ID: "read-" + name, Function: FunctionCall{Name: "read_observation", Arguments: string(args)}}
		out, err := s.readObservation(ctx, r, call)
		if err != nil {
			t.Fatalf("committed %s failure cannot be read: %v", name, err)
		}
		var page struct {
			Text     string
			Requests int `json:"new_http_requests"`
		}
		if err = json.Unmarshal(out.Data, &page); err != nil || page.Text != "original committed failure" || page.Requests != 0 {
			t.Fatalf("failure changed: %s %v", out.Data, err)
		}
	}
	if err = s.Data.DB.Create(&agentdata.Operation{RunID: r.ID, Key: "tool:unknown", Kind: "discover_sources", InputHash: "unknown", Stage: "unknown_external", ResultJSON: `{}`}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err = s.readObservation(ctx, r, ToolCall{Function: FunctionCall{Arguments: `{"tool_call_id":"unknown","pointer":""}`}}); err == nil {
		t.Fatal("unknown external outcome treated as a committed observation")
	}
}

func TestContextOffersReadersOnlyForCommittedReceipts(t *testing.T) {
	facts, err := contextFacts(LoopState{Operations: []agentdata.Operation{
		{Key: "tool:failed", Kind: "validate_program", Stage: "failed", ResultJSON: `{"error":"failed"}`},
		{Key: "tool:unknown", Kind: "discover_sources", Stage: "unknown_external"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var view struct {
		Observations []struct {
			Stage     string
			Arguments map[string]any `json:"read_arguments"`
		} `json:"recent_observations"`
	}
	if err = json.Unmarshal(facts, &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Observations) != 2 || len(view.Observations[0].Arguments) == 0 || len(view.Observations[1].Arguments) != 0 {
		t.Fatalf("read contract does not match stored receipt stages: %s", facts)
	}
}

func TestToolReplayTreatsJSONKeyOrderAsSameInput(t *testing.T) {
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r, err := s.Create(ctx, CreateRunRequest{RequestKey: "argument-order", Goal: "read persisted evidence"})
	if err != nil {
		t.Fatal(err)
	}
	r, err = s.Data.Claim(ctx, r.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	receipt := `{"call_id":"original","name":"get_task_results","data":{"body":"complete original text"}}`
	if err = s.Data.DB.Create(&agentdata.Operation{RunID: r.ID, Key: "tool:original", InputHash: "seed", Kind: "get_task_results", Stage: "succeeded", ResultJSON: receipt}).Error; err != nil {
		t.Fatal(err)
	}
	call := ToolCall{ID: "read", Type: "function", Function: FunctionCall{Name: "read_observation", Arguments: `{"tool_call_id":"original","pointer":"/data/body","offset":0}`}}
	if _, err = s.executeDurableTool(ctx, r, call); err != nil {
		t.Fatal(err)
	}
	call.Function.Arguments = `{"offset":0,"pointer":"/data/body","tool_call_id":"original"}`
	if _, err = s.executeDurableTool(ctx, r, call); err != nil {
		t.Fatalf("semantically identical arguments conflicted: %v", err)
	}
	call.Function.Arguments = `{"offset":1,"pointer":"/data/body","tool_call_id":"original"}`
	if _, err = s.executeDurableTool(ctx, r, call); err == nil {
		t.Fatal("different input under the same call ID accepted")
	}
}
