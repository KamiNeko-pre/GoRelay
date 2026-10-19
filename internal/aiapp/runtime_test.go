package aiapp

import (
	"context"
	"encoding/json"
	"errors"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/knowledge"
	"go-lua-crawler/internal/testdb"
	"testing"
	"time"
)

func TestRuntimePersistsCoreBoundaryAndDoesNotSpendModelBudget(t *testing.T) {
	db := testdb.Open(t)
	data := agentdata.NewStore(db)
	service := NewService(db)
	if err := service.Migrate(); err != nil {
		t.Fatal(err)
	}
	// Retain the runtime's unfinished-decision boundary as a fault injection.
	service.Decide = func(context.Context, LoopState) (NextAction, error) { return NextAction{}, ErrCorePending }
	r, err := service.Create(context.Background(), CreateRunRequest{RequestKey: "r", Goal: "collect public release facts"})
	if err != nil {
		t.Fatal(err)
	}
	if err = service.Advance(context.Background(), r.ID); !errors.Is(err, ErrCorePending) {
		t.Fatalf("core gap hidden: %v", err)
	}
	stored, _ := data.Run(context.Background(), r.ID)
	var budget BudgetSnapshot
	json.Unmarshal([]byte(stored.BudgetJSON), &budget)
	if stored.Status != "blocked_core" || budget.Spent.ModelCalls != 0 {
		t.Fatalf("spent model budget or lost boundary: %+v %+v", stored, budget)
	}
}

func TestRuntimeUnlimitedBudgetIsFrozenByHostAndDoesNotAlterOldRuns(t *testing.T) {
	t.Setenv("GORELAY_EXPERIMENT_UNLIMITED_BUDGET", "1")
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r, err := s.Create(ctx, CreateRunRequest{RequestKey: "unlimited", Goal: "integration diagnostic"})
	if err != nil {
		t.Fatal(err)
	}
	var budget BudgetSnapshot
	if err := json.Unmarshal([]byte(r.BudgetJSON), &budget); err != nil {
		t.Fatal(err)
	}
	if !budget.Unlimited || !r.Deadline.IsZero() {
		t.Fatal("host diagnostic not frozen")
	}
	var count int64
	if err := s.Data.DB.Model(&agentdata.Run{}).Where("id = ? AND deadline IS NULL", r.ID).Count(&count).Error; err != nil || count != 1 {
		t.Fatal("unlimited deadline is not SQL NULL", err)
	}
	claimed, err := s.Data.Claim(ctx, r.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Data.SaveRun(ctx, r.ID, claimed.Token, map[string]any{"status": "waiting"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GORELAY_EXPERIMENT_UNLIMITED_BUDGET", "0")
	again, err := s.Create(ctx, CreateRunRequest{RequestKey: "unlimited", Goal: "integration diagnostic"})
	if err != nil {
		t.Fatal(err)
	}
	if again.BudgetJSON != r.BudgetJSON {
		t.Fatal("runtime config changed an admitted budget")
	}
	bounded, err := s.Create(ctx, CreateRunRequest{RequestKey: "bounded", Goal: "normal run"})
	if err != nil {
		t.Fatal(err)
	}
	var boundedBudget BudgetSnapshot
	if err := json.Unmarshal([]byte(bounded.BudgetJSON), &boundedBudget); err != nil {
		t.Fatal(err)
	}
	if boundedBudget.Unlimited || boundedBudget.LoopVersion != 5 || !bounded.Deadline.IsZero() {
		t.Fatal("new run did not freeze adaptive production policy")
	}
}

func TestRuntimeFixtureDriverResumesExistingChildWithoutDuplicate(t *testing.T) {
	db := testdb.Open(t)
	service := NewService(db)
	if err := service.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// Test-only decision driver, not a production substitute for RunTaskLoop.
	service.Decide = func(ctx context.Context, in LoopState) (NextAction, error) { return NextAction{Kind: "wait"}, nil }
	r, _ := service.Create(ctx, CreateRunRequest{RequestKey: "resume", Goal: "goal"})
	lease, _ := service.Data.Claim(ctx, r.ID, time.Minute)
	a := agentdata.ProgramArtifact{Code: "return true,{items={}}", PolicyVersion: "public-read-v1"}
	first, err := service.Data.SubmitChild(ctx, r.ID, lease.Token, "key", "hash", a, "https://example.com", `{}`, nil)
	if err != nil {
		t.Fatal(err)
	}
	service.Data.SaveRun(ctx, r.ID, lease.Token, map[string]any{"status": "waiting"})
	if err = service.Advance(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	children, _ := service.Data.Children(ctx, r.ID)
	if len(children) != 1 || children[0].TaskID != first.TaskID {
		t.Fatal("resume submitted duplicate child")
	}
}

func TestRAGRefusesForeignCitationAndKeepsSnapshots(t *testing.T) {
	db := testdb.Open(t)
	store := knowledge.NewStore(db)
	store.Migrate()
	client := staticChat{answer: `{"status":"answered","answer":"invented","citations":["ev_foreign"]}`}
	rag := RAGResponder{Model: client, Store: store}
	_, err := rag.Answer(context.Background(), "question", knowledge.EvidenceSet{RequestID: "request", Entries: []knowledge.Evidence{{ID: "ev_allowed", Text: "actual source"}}})
	if !errors.Is(err, knowledge.ErrInvalidReference) {
		t.Fatalf("foreign evidence accepted: %v", err)
	}
}

type staticChat struct{ answer string }

func (c staticChat) Chat(context.Context, ChatRequest) (ChatResponse, error) {
	return ChatResponse{Message: Message{Role: "assistant", Content: c.answer}}, nil
}
