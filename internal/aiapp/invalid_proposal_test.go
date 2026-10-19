package aiapp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go-lua-crawler/internal/testdb"
)

func TestReceivedInvalidCodexProposalIsKnownFailureWithUsage(t *testing.T) {
	for _, proposal := range []string{`{broken`, `{"content":"","tool_calls":[{"name":"invented","arguments":"{}"}]}`, `{"content":"","tool_calls":[{"name":"inspect_source","arguments":"[]"}]}`} {
		s := NewService(testdb.Open(t))
		if err := s.Migrate(); err != nil {
			t.Fatal(err)
		}
		text, _ := json.Marshal(proposal)
		p := &codexRefusalChat{raw: `{"type":"thread.started","thread_id":"known-response"}
{"type":"item.completed","item":{"type":"agent_message","text":` + string(text) + `}}
{"type":"turn.completed","usage":{"input_tokens":7,"output_tokens":3}}`}
		s.Model = p
		r, err := s.Create(context.Background(), CreateRunRequest{RequestKey: "invalid-proposal", Goal: "public facts"})
		if err != nil {
			t.Fatal(err)
		}
		// Preserve the frozen V4 provider-refusal contract. V5 has a separate
		// bounded proposal-correction test through the full HTTP chain.
		var budget BudgetSnapshot
		_ = json.Unmarshal([]byte(r.BudgetJSON), &budget)
		budget.LoopVersion = 4
		rawBudget, _ := json.Marshal(budget)
		if err := s.Data.DB.Model(&r).Update("budget_json", string(rawBudget)).Error; err != nil {
			t.Fatal(err)
		}
		_ = s.Advance(context.Background(), r.ID)
		ops, err := s.Data.Operations(context.Background(), r.ID)
		if err != nil || len(ops) != 1 || ops[0].Stage != "failed" {
			t.Fatal("received proposal misclassified as unknown outcome", ops, err)
		}
		var response ChatResponse
		if err := json.Unmarshal([]byte(ops[0].ResultJSON), &response); err != nil {
			t.Fatal(err)
		}
		if response.Failure == nil || response.Failure.Kind != "invalid_proposal" || response.Failure.RetryAt != nil || response.Usage == nil || response.Usage.TotalTokens != 10 {
			t.Fatal("invalid proposal or billing facts lost", response)
		}
		restored := NewService(s.Data.DB)
		restored.Model = p
		for n := 0; n < 2; n++ {
			_ = restored.Advance(context.Background(), r.ID)
		}
		if p.calls != 1 {
			t.Fatal("invalid response repeated as transport retry", p.calls)
		}
	}
}

func TestReceivedHTTPChoiceShapeIsInvalidProposalNotUnknownTransport(t *testing.T) {
	for _, choices := range []string{`[]`, `[{"message":{"role":"assistant"}},{"message":{"role":"assistant"}}]`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"choices":` + choices + `,"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`))
		}))
		p, err := NewModelProvider(ProviderConfig{BaseURL: server.URL, APIKey: "fixture", Model: "fixture"})
		if err != nil {
			server.Close()
			t.Fatal(err)
		}
		out, err := p.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "fixture"}}})
		server.Close()
		if !errors.Is(err, ErrInvalidProposal) || out.Usage == nil || out.Usage.TotalTokens != 10 {
			t.Fatal("received HTTP shape/usage misclassified", out, err)
		}
	}
}

func TestValidCodexProposalAndUnknownTransportRemainDistinct(t *testing.T) {
	request := ChatRequest{Tools: []ToolDefinition{{Function: FunctionDefinition{Name: "inspect_source"}}}}
	valid := `{"type":"thread.started","thread_id":"valid"}
{"type":"item.completed","item":{"type":"agent_message","text":"{\"content\":\"inspect source\",\"tool_calls\":[{\"name\":\"inspect_source\",\"arguments\":\"{\\\"url\\\":\\\"https://fixture.example\\\"}\"}]}"}}
{"type":"turn.completed","usage":{"input_tokens":7,"output_tokens":3}}`
	out, err := parseCodexInference(strings.NewReader(valid), request, "fixture-model")
	if err != nil || len(out.Message.ToolCalls) != 1 || out.Usage.TotalTokens != 10 {
		t.Fatal("valid proposal changed", out, err)
	}
	_, err = parseCodexInference(strings.NewReader(`{"type":"thread.started","thread_id":"incomplete"}`), request, "fixture-model")
	f := captureFailure(err, 0, true)
	if f == nil || f.Kind != "unknown_model_outcome" {
		t.Fatal("transport uncertainty assigned invalid-proposal fact", f)
	}
}
