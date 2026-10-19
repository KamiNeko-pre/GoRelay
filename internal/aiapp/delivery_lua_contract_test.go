package aiapp

import (
	"context"
	"encoding/json"
	"go-lua-crawler/internal/engine"
	"go-lua-crawler/internal/testdb"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDeliveryLuaEmptyOutputNamesContractAndActualOutput(t *testing.T) {
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	spec := TaskSpec{Goal: "collect release title", SchemaVersion: "v1", RequiredFields: []FieldRequirement{{Name: "title", Type: "string"}}, JoinKeys: []string{"title"}}
	r, err := s.Create(context.Background(), CreateRunRequest{RequestKey: "empty-trial", Goal: spec.Goal, Spec: &spec})
	if err != nil {
		t.Fatal(err)
	}
	r, err = s.Data.Claim(context.Background(), r.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(RunnerResponse{Result: &engine.ProgramResult{Collection: map[string]any{"items": []any{}}}})
	}))
	defer server.Close()
	s.Runner, _ = NewRunnerClient(server.URL, "fixture")
	call := ToolCall{ID: "empty-program", Type: "function", Function: FunctionCall{Name: "validate_program", Arguments: `{"kind":"adapter","source_id":"release-source","purpose":"collect title","url":"https://example.com/releases","code":"return true,{items={}}"}`}}
	reply, err := s.executeDurableTool(context.Background(), r, call)
	if err == nil || reply.Failure == nil || reply.Failure.Code != "PROGRAM_OUTPUT_CONTRACT_ERROR" {
		t.Fatalf("output diagnosis missing: %+v %v", reply, err)
	}
	raw := string(mustJSON(reply.Failure))
	if !strings.Contains(raw, "required_fields") || !strings.Contains(raw, "record_count") || reply.Failure.ExecutionStatus != "failed" {
		t.Fatalf("expected/actual or executed status lost: %s", raw)
	}
}
