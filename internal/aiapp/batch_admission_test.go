package aiapp

import (
	"context"
	"encoding/json"
	"errors"
	"go-lua-crawler/internal/testdb"
	"testing"
)

type batchAdmissionModel struct{ calls []ToolCall }

func (m batchAdmissionModel) Chat(context.Context, ChatRequest) (ChatResponse, error) {
	return ChatResponse{Message: Message{Role: "assistant", ToolCalls: m.calls}, Usage: &Usage{TotalTokens: 1}}, nil
}

func TestTaskReviewKeepsDeliveryReserveButOnlyAdvisesCheckpoint(t *testing.T) {
	state := decisionState()
	state.Budget.LoopVersion = 2
	state.Spec = &TaskSpec{Goal: "collect facts"}
	state.Progress = TaskProgress{Version: 2, FactsSignature: "prior-facts"}
	if review := taskReview(state); !review.Required || review.DeliveryRequired {
		t.Fatal("checkpoint advisory became a resource stop", review)
	}
	state.Budget.Spent.ModelCalls = state.Budget.Limits.ModelCalls - 1
	if !taskReview(state).DeliveryRequired {
		t.Fatal("delivery decision reserve was removed")
	}
}

type batchAdmissionSearch struct {
	calls     int
	failFirst bool
}

func (s *batchAdmissionSearch) Search(context.Context, string, int) (SearchResponse, error) {
	s.calls++
	if s.failFirst && s.calls == 1 {
		return SearchResponse{}, errors.New("source discovery unavailable")
	}
	return SearchResponse{Candidates: []SearchCandidate{}}, nil
}

func TestBatchAdmissionKeepsIndependentProposalsUntilFeedback(t *testing.T) {
	for _, first := range []string{"discover_sources", "inspect_source"} {
		t.Run(first, func(t *testing.T) {
			s := NewService(testdb.Open(t))
			if err := s.Migrate(); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			spec := TaskSpec{Goal: "investigate two public sources", SchemaVersion: "v1", RequiredFields: []FieldRequirement{{Name: "body", Type: "string"}}}
			r, err := s.Create(ctx, CreateRunRequest{RequestKey: "batch", Goal: spec.Goal, Spec: &spec})
			if err != nil {
				t.Fatal(err)
			}
			args := `{"query":"first candidate"}`
			if first == "inspect_source" {
				args = `{"url":"http://127.0.0.1/private"}`
			}
			calls := []ToolCall{
				{ID: "first", Type: "function", Function: FunctionCall{Name: first, Arguments: args}},
				{ID: "second", Type: "function", Function: FunctionCall{Name: "discover_sources", Arguments: `{"query":"independent second candidate"}`}},
			}
			search := &batchAdmissionSearch{failFirst: first == "discover_sources"}
			s.Model = batchAdmissionModel{calls: calls}
			s.Search = search
			if err := s.Advance(ctx, r.ID); err != nil {
				t.Fatal(err)
			}
			// Reconstruct the Service before tool execution to require SQL-owned admission.
			restored := NewService(s.Data.DB)
			restored.Search = search
			if err := restored.Advance(ctx, r.ID); err != nil {
				t.Fatal(err)
			}
			ops, err := s.Data.Operations(ctx, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			stages := map[string]string{}
			for _, op := range ops {
				stages[op.Key] = op.Stage
			}
			if stages["tool:first"] != "failed" || stages["tool:second"] != "succeeded" {
				t.Fatalf("independent pending call rejected inside original batch: %+v", stages)
			}
			current, _ := s.Data.Run(ctx, r.ID)
			state, _ := s.state(ctx, current)
			if !taskReview(state).Required {
				t.Fatal("next decision lost the required failure assessment")
			}
			before := search.calls
			restored.Decide = func(context.Context, LoopState) (NextAction, error) {
				return NextAction{Kind: "tools", Calls: []ToolCall{{ID: "unproposed", Type: "function", Function: FunctionCall{Name: "discover_sources", Arguments: `{"query":"unassessed extra source"}`}}}}, nil
			}
			err = restored.Advance(ctx, r.ID)
			if err == nil || search.calls != before {
				t.Fatal("unrelated/unassessed call inherited prior admission")
			}
			if first == "inspect_source" {
				for _, op := range ops {
					if op.Key == "tool:first" {
						var receipt ToolResult
						_ = json.Unmarshal([]byte(op.ResultJSON), &receipt)
						if receipt.HTTP != nil && receipt.HTTP.Requests != 0 {
							t.Fatal("private source became an external request")
						}
					}
				}
			}
		})
	}
}
