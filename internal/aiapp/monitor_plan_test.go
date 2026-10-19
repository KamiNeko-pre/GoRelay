package aiapp

import (
	"context"
	"encoding/json"
	"fmt"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/repository"
	"go-lua-crawler/internal/task"
	"go-lua-crawler/internal/testdb"
	"testing"
	"time"
)

// The first completed acquisition is an explicit database fixture, not evidence
// that a real model autonomously found a source or produced this program.
func seedMonitorPlan(t *testing.T, semantic bool) (*Service, Monitor, agentdata.ProgramArtifact) {
	t.Helper()
	db := testdb.Open(t)
	s := NewService(db)
	if e := s.Migrate(); e != nil {
		t.Fatal(e)
	}
	ctx := context.Background()
	spec := monitorSpec()
	spec.Entities = []string{"a"}
	spec.RequireSummary = semantic
	m, e := s.CreateMonitor(ctx, CreateMonitorRequest{RequestKey: "plan-watch", Goal: spec.Goal, Spec: &spec})
	if e != nil {
		t.Fatal(e)
	}
	r, e := s.RefreshMonitor(ctx, m.ID, "initial")
	if e != nil {
		t.Fatal(e)
	}
	// The real submit_acquisition path binds the admitted Run contract, including
	// its host contract version, rather than the caller's pre-admission copy.
	spec, e = specOf(r)
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(spec)
	a, e := s.Data.SaveArtifact(ctx, agentdata.ProgramArtifact{Code: "return {items={}} -- fixture adapter", Kind: "adapter", PolicyVersion: "public-read-v1", OutputSchema: string(raw), ScopeJSON: `["example.com"]`, InputSchema: `{"type":"object","properties":{"entity":{"type":"string"}},"required":["entity"],"additionalProperties":false}`})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Data.Publish(ctx, agentdata.SourceVersion{SourceID: "public", ArtifactHash: a.Hash, ValidatorVersion: "typed-receipt-v1", ProofJSON: `{"fixture":true}`}, "", 0); e != nil {
		t.Fatal(e)
	}
	r, e = s.Data.Claim(ctx, r.ID, time.Minute)
	if e != nil {
		t.Fatal(e)
	}
	params, _ := json.Marshal(childParameters{Spec: spec, SourceID: "public", Params: map[string]any{"entity": "a"}})
	link, e := s.Data.SubmitChild(ctx, r.ID, r.Token, "initial-child", agentdata.Hash("initial"), a, "https://example.com/data", string(params), nil)
	if e != nil {
		t.Fatal(e)
	}
	fixtureMonitorResult(t, s, r.ID, link.TaskID, 4500, false)
	if e = s.Data.SaveRun(ctx, r.ID, r.Token, map[string]any{"status": "completed"}); e != nil {
		t.Fatal(e)
	}
	if e = s.ProcessMonitorCheck(ctx, r.ID); e != nil {
		t.Fatal(e)
	}
	m, e = s.Monitor(ctx, m.ID)
	if e != nil {
		t.Fatal(e)
	}
	return s, m, a
}

func fixtureMonitorResult(t *testing.T, s *Service, runID string, taskID uint64, amount any, stale bool) {
	t.Helper()
	r, e := s.Data.Run(context.Background(), runID)
	if e != nil {
		t.Fatal(e)
	}
	spec, e := specOf(r)
	if e != nil {
		t.Fatal(e)
	}
	fetched := time.Now().UTC()
	if stale {
		fetched = fetched.Add(-2 * time.Hour)
	}
	records := []Record{{Key: fmt.Sprintf("task-%d", taskID), SchemaVersion: spec.SchemaVersion, Fields: map[string]any{"id": "a", "amount": amount, "unit": "CNY", "labels": []string{"zh"}}, Provenance: Provenance{SourceID: "public", URL: "https://example.com/data", FetchedAt: fetched, DataVersion: "fixture", TaskID: taskID}}}
	raw, _ := json.Marshal(AcquisitionResult{Records: records})
	if e = s.Data.DB.Create(&repository.CrawlResult{TaskID: taskID, Payload: string(raw)}).Error; e != nil {
		t.Fatal(e)
	}
	if e = s.Data.DB.Model(&task.CrawlTask{}).Where("id = ?", taskID).Update("status", task.StatusSucceeded).Error; e != nil {
		t.Fatal(e)
	}
}

func TestMonitorPlanCapturesSuccessfulWorkAndReusesWithoutModel(t *testing.T) {
	s, m, a := seedMonitorPlan(t, false)
	ctx := context.Background()
	if m.PlanJSON == "" {
		t.Fatal("successful acquisition did not produce a reusable plan")
	}
	var plan MonitorPlan
	if e := json.Unmarshal([]byte(m.PlanJSON), &plan); e != nil {
		t.Fatal(e)
	}
	if len(plan.Bindings) != 1 || plan.Bindings[0].ArtifactHash != a.Hash || plan.Bindings[0].Params["entity"] != "a" {
		t.Fatal("did not retain actual successful work", plan)
	}
	r, e := s.RefreshMonitor(ctx, m.ID, "reuse")
	if e != nil {
		t.Fatal(e)
	}
	if e = s.Advance(ctx, r.ID); e != nil {
		t.Fatal("deterministic submission", e)
	}
	children, e := s.ChildResults(ctx, r.ID)
	if e != nil || len(children) != 1 || children[0].Task.Status != task.StatusQueued {
		t.Fatal(children, e)
	}
	var count int64
	s.Data.DB.Model(&task.TaskOutbox{}).Where("task_id = ?", children[0].Task.ID).Count(&count)
	if count != 1 {
		t.Fatal("reuse bypassed Outbox")
	}
	if e = s.Advance(ctx, r.ID); e != nil {
		t.Fatal("wait", e)
	}
	children, _ = s.ChildResults(ctx, r.ID)
	if len(children) != 1 {
		t.Fatal("wait duplicated child")
	}
	fixtureMonitorResult(t, s, r.ID, children[0].Task.ID, 4000, false)
	if e = s.Advance(ctx, r.ID); e != nil {
		t.Fatal("deterministic delivery", e)
	}
	stored, e := s.Data.Run(ctx, r.ID)
	if e != nil || stored.Status != "completed" {
		t.Fatal(stored, e)
	}
	var budget BudgetSnapshot
	json.Unmarshal([]byte(stored.BudgetJSON), &budget)
	if budget.Spent.ModelCalls != 0 || budget.Spent.SearchCalls != 0 || budget.Spent.ToolCalls == 0 {
		t.Fatal("reuse charged a model or bypassed tool accounting", budget)
	}
	var result TaskResult
	json.Unmarshal([]byte(stored.ResultJSON), &result)
	if len(result.Records) != 1 || fmt.Sprint(result.Records[0].Fields["amount"]) != "4000" {
		t.Fatal("returned historical data instead of refreshed facts", result)
	}
}

func TestMonitorPlanInvalidSourceAndBadResultReturnToAgentLoop(t *testing.T) {
	for _, reason := range []string{"quarantined", "stale-result", "invalid-result"} {
		t.Run(reason, func(t *testing.T) {
			s, m, _ := seedMonitorPlan(t, false)
			ctx := context.Background()
			r, e := s.RefreshMonitor(ctx, m.ID, "replan")
			if e != nil {
				t.Fatal(e)
			}
			if reason == "quarantined" {
				if e = s.Data.DB.Model(&agentdata.SourceHead{}).Where("source_id = ?", "public").Update("status", "quarantined").Error; e != nil {
					t.Fatal(e)
				}
			}
			if e = s.Advance(ctx, r.ID); e != nil {
				t.Fatal(e)
			}
			if reason != "quarantined" {
				children, _ := s.ChildResults(ctx, r.ID)
				if len(children) != 1 {
					t.Fatal(children)
				}
				var amount any = 4500
				if reason == "invalid-result" {
					amount = nil
				}
				fixtureMonitorResult(t, s, r.ID, children[0].Task.ID, amount, reason == "stale-result")
				if e = s.Advance(ctx, r.ID); e != nil {
					t.Fatal(e)
				}
			}
			var check MonitorCheck
			s.Data.DB.Where("run_id = ?", r.ID).First(&check)
			stored, _ := s.Data.Run(ctx, r.ID)
			if check.Mode != "agent" || stored.IsTerminal() || stored.PlanJSON == "" {
				t.Fatal("invalid reuse claimed completion or lost feedback", check, stored)
			}
			var budget BudgetSnapshot
			json.Unmarshal([]byte(stored.BudgetJSON), &budget)
			if budget.Spent.ModelCalls != 0 {
				t.Fatal("fallback itself invoked paid model")
			}
		})
	}
}

func TestMonitorPlanDoesNotAutomateSemanticJudgments(t *testing.T) {
	_, m, _ := seedMonitorPlan(t, true)
	if m.PlanJSON != "" {
		t.Fatal("summary task acquired a zero-model completion shortcut")
	}
}

func TestMonitorPlanConfigurationInvalidatesOnlyAcquisitionInputs(t *testing.T) {
	for _, change := range []string{"condition", "interval", "field", "scope"} {
		t.Run(change, func(t *testing.T) {
			s, m, _ := seedMonitorPlan(t, false)
			ctx := context.Background()
			config, err := m.Config()
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "condition":
				config.Conditions = []MonitorCondition{{Field: "amount", Operator: "lte", Value: json.RawMessage(`3000`)}}
			case "interval":
				config.IntervalSeconds = 7200
			case "field":
				config.Spec.RequiredFields = append(config.Spec.RequiredFields, FieldRequirement{"extra", "string"})
			case "scope":
				config.SourceScope = []string{"other.example.com"}
			}
			updated, err := s.UpdateMonitor(ctx, m.ID, UpdateMonitorRequest{Config: &config})
			if err != nil {
				t.Fatal(err)
			}
			if updated.Version != m.Version+1 {
				t.Fatal("config revision not updated")
			}
			preserves := change == "condition" || change == "interval"
			if (updated.PlanJSON != "") != preserves {
				t.Fatal("acquisition invalidation differs from request inputs", change, updated.PlanJSON)
			}
			if preserves {
				var plan MonitorPlan
				if err = json.Unmarshal([]byte(updated.PlanJSON), &plan); err != nil {
					t.Fatal(err)
				}
				if plan.ConfigVersion != updated.Version {
					t.Fatal("plan revision not rebound")
				}
			}
			run, err := s.RefreshMonitor(ctx, m.ID, "after-config")
			if err != nil {
				t.Fatal(err)
			}
			var check MonitorCheck
			if err = s.Data.DB.Where("run_id = ?", run.ID).First(&check).Error; err != nil {
				t.Fatal(err)
			}
			if (check.Mode == "plan") != preserves {
				t.Fatal("refresh selected wrong mode", change, check.Mode)
			}
		})
	}
}
