package aiapp

import (
	"context"
	"encoding/json"
	"go-lua-crawler/internal/agentdata"
	task "go-lua-crawler/internal/task"
	"go-lua-crawler/internal/testdb"
	"testing"
	"time"
)

func TestMultiSourceRecoveryRetainsSuccessfulFactsFailureAndBudget(t *testing.T) {
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	spec := TaskSpec{Goal: "merge complementary public facts", SchemaVersion: "v1", RequiredFields: []FieldRequirement{{Name: "name", Type: "string"}, {Name: "version", Type: "string"}}, JoinKeys: []string{"name"}}
	r, err := s.Create(ctx, CreateRunRequest{RequestKey: "multi-recovery", Goal: spec.Goal, Spec: &spec})
	if err != nil {
		t.Fatal(err)
	}
	r, err = s.Data.Claim(ctx, r.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	params, _ := json.Marshal(childParameters{Spec: spec})
	// Deliberate infrastructure fixtures, not generated/validated live programs.
	a := agentdata.ProgramArtifact{Code: "return true,{items={}}", Kind: "composition", PolicyVersion: "fixture"}
	first, err := s.Data.SubmitChild(ctx, r.ID, r.Token, "A", "A", a, "https://a.example/", string(params), nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Data.SubmitChild(ctx, r.ID, r.Token, "B", "B", a, "https://b.example/", string(params), nil)
	if err != nil {
		t.Fatal(err)
	}
	var child task.CrawlTask
	if err = s.Data.DB.First(&child, first.TaskID).Error; err != nil {
		t.Fatal(err)
	}
	record := Record{Key: "Alpha", SchemaVersion: "v1", Fields: map[string]any{"name": "Alpha"}, Provenance: Provenance{SourceID: "A", URL: child.URL, DataVersion: child.ProgramHash, TaskID: child.ID, FetchedAt: time.Now()}}
	if err = s.persistAcquisition(child, AcquisitionResult{Records: []Record{record}})(s.Data.DB); err != nil {
		t.Fatal(err)
	}
	s.Data.DB.Model(&child).Update("status", task.StatusSucceeded)
	s.Data.DB.Model(&task.CrawlTask{}).Where("id = ?", second.TaskID).Updates(map[string]any{"status": task.StatusFailed, "last_error": "fixture source B unavailable"})
	budget := BudgetSnapshot{Limits: DefaultBudgetLimits(), Deadline: r.Deadline, Candidates: map[string]int{"b.example/adapter": 2}, Spent: CallCost{ModelCalls: 3, ToolCalls: 7, HTTPRequests: 10}}
	encoded, _ := json.Marshal(budget)
	if err = s.Data.SaveRun(ctx, r.ID, r.Token, map[string]any{"status": "waiting", "budget_json": string(encoded)}); err != nil {
		t.Fatal(err)
	}
	resumed := NewService(s.Data.DB)
	resumed.Decide = func(_ context.Context, state LoopState) (NextAction, error) {
		if len(state.TaskResults) != 2 || state.TaskResults[0].Task.ID != first.TaskID || state.TaskResults[1].Task.Status != task.StatusFailed || state.Budget.Spent.ModelCalls != 3 || state.Budget.Candidates["b.example/adapter"] != 2 {
			t.Fatal("recovery lost child IDs/failure/candidate limit")
		}
		if state.Result == nil || state.Result.Coverage.Satisfied || state.Result.Coverage.PartialValidRecords != 1 {
			t.Fatal("partial result became complete or disappeared")
		}
		return NextAction{Kind: "wait", Reason: "fixture driver observes A retained and B failed"}, nil
	}
	if err = resumed.Advance(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	stored, _ := resumed.Data.Run(ctx, r.ID)
	if stored.BudgetJSON != string(encoded) {
		t.Fatal("resume reset or charged budget unexpectedly")
	}
	children, _ := resumed.Data.Children(ctx, r.ID)
	if len(children) != 2 || children[0].TaskID != first.TaskID || children[1].TaskID != second.TaskID {
		t.Fatal("resume recreated children")
	}
}
