package aiapp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"go-lua-crawler/internal/testdb"
)

func TestMissingParameterReachesTheNextActualModelRequest(t *testing.T) {
	for _, field := range []string{"purpose", "url"} {
		t.Run(field, func(t *testing.T) {
			s := NewService(testdb.Open(t))
			if err := s.Migrate(); err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			var received ChatRequest
			model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req ChatRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				n := calls.Add(1)
				message := Message{Role: "assistant"}
				if n == 1 {
					args := map[string]any{"code": "return true,{items={}}", "kind": "adapter", "source_id": "proposed-capability", "purpose": "collect requested titles", "url": "https://public.example/list"}
					delete(args, field)
					raw, _ := json.Marshal(args)
					message.ToolCalls = []ToolCall{{ID: "missing-parameter", Type: "function", Function: FunctionCall{Name: "validate_program", Arguments: string(raw)}}}
				} else {
					received = req
					message.ToolCalls = []ToolCall{{ID: "honest-conclusion", Type: "function", Function: FunctionCall{Name: "finish_task", Arguments: `{"status":"failed","reason":"no acquired records","gaps":["unmet title acquisition"]}`}}}
				}
				json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": message, "finish_reason": "tool_calls"}}})
			}))
			defer model.Close()
			var err error
			s.Model, err = NewModelProvider(ProviderConfig{BaseURL: model.URL, APIKey: "test-only", Model: "feedback-mock"})
			if err != nil {
				t.Fatal(err)
			}
			spec := TaskSpec{Goal: "collect public titles", SchemaVersion: "v1", RequiredFields: []FieldRequirement{{Name: "title", Type: "string"}}}
			r, err := s.Create(context.Background(), CreateRunRequest{RequestKey: "missing-" + field, Goal: spec.Goal, Spec: &spec})
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 6 && calls.Load() < 2; i++ {
				if err := s.Advance(context.Background(), r.ID); err != nil {
					t.Fatal(err)
				}
			}
			if calls.Load() < 2 {
				t.Fatal("no correction opportunity sent to model")
			}
			receipts, _ := auditVisibleReceipts(received)
			out := receipts["validate_program"]
			if out.Failure == nil || out.Failure.Code != "MISSING_REQUIRED_PARAMETER" || out.Failure.ExecutionStatus != "not_started" {
				t.Fatalf("not actionable in actual request: %+v", out.Failure)
			}
			raw, _ := json.Marshal(out.Failure.Details)
			for _, want := range []string{`"missing"`, `"` + field + `"`, `"accepted"`, `"example"`} {
				if !strings.Contains(string(raw), want) {
					t.Errorf("feedback omitted %s: %s", want, raw)
				}
			}
			if out.Failure.Recovery == nil || !strings.Contains(out.Failure.Recovery.Instruction, field) {
				t.Fatal(fmt.Sprintf("model not told what to supply: %+v", out.Failure))
			}
		})
	}
}
