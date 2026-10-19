package aiapp

import (
	"context"
	"encoding/json"
	"errors"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/testdb"
	"gorm.io/gorm"
	"sync/atomic"
	"testing"
	"time"
)

type loopChatFixture struct{ calls int }

func TestLoopReportCountsReusedToolExecutionWithoutModel(t *testing.T) {
	traces := []agentdata.Trace{}
	for i, action := range []string{"monitor_submit", "tools", "monitor_finish", "monitor_replan", "model"} {
		raw, _ := json.Marshal(LoopRound{RoundID: action, Stage: "finished", Action: action, ModelAttempt: i})
		traces = append(traces, agentdata.Trace{Stage: "loop_round", DataJSON: string(raw)})
	}
	view := BuildLoopReport(traces)
	if view.Summary.ToolAdvances != 2 || view.Summary.ModelAdvances != 1 || view.Summary.CompletedAdvances != 5 {
		t.Fatal("reuse counted as a model or omitted its tool execution", view.Summary)
	}
}

func (f *loopChatFixture) Chat(_ context.Context, _ ChatRequest) (ChatResponse, error) {
	f.calls++
	call := ToolCall{ID: "plan", Type: "function", Function: FunctionCall{Name: "plan_task", Arguments: `{"goal":"collect release facts","schema_version":"v1","required_fields":[{"name":"name","type":"string"}]}`}}
	if f.calls > 1 {
		call = ToolCall{ID: "search", Type: "function", Function: FunctionCall{Name: "discover_sources", Arguments: `{"query":"public release facts"}`}}
	}
	return ChatResponse{Message: Message{Role: "assistant", ToolCalls: []ToolCall{call}}, Usage: &Usage{TotalTokens: 10}}, nil
}

func TestLoopTracePersistsDecisionFeedbackAndResume(t *testing.T) {
	db := testdb.Open(t)
	s := NewService(db)
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r, err := s.Create(ctx, CreateRunRequest{RequestKey: "loop", Goal: "collect release facts"})
	if err != nil {
		t.Fatal(err)
	}
	s.Model = &loopChatFixture{}
	// Fixture decisions drive the production runtime only in this test. The
	// learner's RunTaskLoop remains unchanged and no live Agent result is claimed.
	s.Decide = func(_ context.Context, state LoopState) (NextAction, error) {
		if len(state.Messages) > 0 {
			last := state.Messages[len(state.Messages)-1]
			if last.Role == "assistant" && len(last.ToolCalls) > 0 {
				return NextAction{Kind: "tools", Calls: last.ToolCalls, Reason: "execute current proposal"}, nil
			}
			if last.Role == "tool" && last.ToolCallID == "search" {
				return NextAction{Kind: "wait", Reason: "search unavailable; preserve observations"}, nil
			}
		}
		return NextAction{Kind: "model", Reason: "ask model using current observations"}, nil
	}
	for n := 0; n < 4; n++ {
		if err := s.Advance(ctx, r.ID); err != nil {
			t.Fatal(err)
		}
		if n == 1 {
			pending, err := s.Data.Run(ctx, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.ApproveContract(ctx, r.ID, agentdata.Hash(json.RawMessage(pending.ContractProposalJSON))); err != nil {
				t.Fatal(err)
			}
		}
	}
	// A fresh Service demonstrates the view is built from SQL, not in-memory UI state.
	resumed := NewService(db)
	resumed.Decide = s.Decide
	if err := resumed.Advance(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	traces, err := resumed.Data.Traces(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	view := BuildLoopReport(traces)
	if len(view.Rounds) != 5 || view.Summary.CompletedAdvances != 5 || view.Summary.ModelAdvances != 2 || view.Summary.ToolAdvances != 2 || view.Summary.WaitAdvances != 1 {
		t.Fatalf("missing durable loop history: %+v", view)
	}
	if view.Summary.ToolSuccesses != 1 || view.Summary.ToolFailures != 1 {
		t.Fatalf("failed observation hidden: %+v", view.Summary)
	}
	if len(view.Rounds[0].ProposedTools) != 1 || view.Rounds[0].ProposedTools[0].Name != "plan_task" || len(view.Rounds[2].ProposedTools) != 1 || view.Rounds[2].ProposedTools[0].Name != "discover_sources" {
		t.Fatal("model proposals are not tied to their persisted loop step")
	}
	last := view.Rounds[len(view.Rounds)-1]
	if last.Action != "wait" || last.StatusAfter != "waiting" || last.Reason == "" || last.BudgetAfter == nil || last.BudgetAfter.ModelCalls != 2 || last.BudgetAfter.SearchCalls != 1 {
		t.Fatalf("lost decision/budget/state: %+v", last)
	}
	if view.Rounds[3].Tools[0].Error == "" || view.Rounds[3].CoverageAfter == nil || view.Rounds[3].CoverageAfter.Satisfied {
		t.Fatal("unknown search or missing facts presented as success")
	}
}

func TestLoopTraceRecordsRejectedCompletionAndCoreGap(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(map[bool]string{false: "rejected", true: "core_pending"}[pending], func(t *testing.T) {
			db := testdb.Open(t)
			s := NewService(db)
			if err := s.Migrate(); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			spec := TaskSpec{Goal: "collect", SchemaVersion: "v1", RequiredFields: []FieldRequirement{{Name: "name", Type: "string"}}}
			r, err := s.Create(ctx, CreateRunRequest{RequestKey: "boundary", Goal: spec.Goal, Spec: &spec})
			if err != nil {
				t.Fatal(err)
			}
			if pending {
				s.Decide = func(context.Context, LoopState) (NextAction, error) { return NextAction{}, ErrCorePending }
			} else {
				s.Decide = func(context.Context, LoopState) (NextAction, error) {
					return NextAction{Kind: "finish", Status: "completed", Reason: "model proposed completion"}, nil
				}
			}
			err = s.Advance(ctx, r.ID)
			if err == nil || (pending && !errors.Is(err, ErrCorePending)) {
				t.Fatalf("boundary hidden: %v", err)
			}
			traces, _ := s.Data.Traces(ctx, r.ID)
			view := BuildLoopReport(traces)
			if len(view.Rounds) != 1 || view.Rounds[0].Error == "" || view.Rounds[0].StatusAfter == "completed" {
				t.Fatalf("false completion: %+v", view)
			}
			if pending && view.Rounds[0].StatusAfter != "blocked_core" {
				t.Fatal("learner gap bypassed")
			}
			if !pending && (!view.Rounds[0].CompletionRejected || view.Summary.CompletionRejections != 1 || view.Rounds[0].CoverageAfter.Satisfied) {
				t.Fatal("hard completion rejection not observable")
			}
		})
	}
}

func TestLoopReportLeavesInterruptedAdvanceUnknown(t *testing.T) {
	started, _ := json.Marshal(LoopRound{RoundID: "interrupted", Stage: "started", StatusBefore: "planning"})
	view := BuildLoopReport([]agentdata.Trace{{Stage: "loop_round", DataJSON: string(started)}, {Stage: "unrelated", DataJSON: `{}`}})
	if len(view.Rounds) != 1 || view.Summary.IncompleteAdvances != 1 || view.Summary.CompletedAdvances != 0 || view.Rounds[0].DurationMS != nil || view.Rounds[0].StatusAfter != "" {
		t.Fatalf("unfinished advance fabricated an outcome: %+v", view)
	}
}

func TestLoopTraceSnapshotPrecedesNextClaim(t *testing.T) {
	for _, action := range []string{"wait", "model"} {
		t.Run(action, func(t *testing.T) {
			db := testdb.Open(t)
			s := NewService(db)
			if err := s.Migrate(); err != nil {
				t.Fatal(err)
			}
			ctx := context.WithValue(context.Background(), struct{}{}, "primary advance")
			r, err := s.Create(ctx, CreateRunRequest{RequestKey: "trace-order", Goal: "collect release facts"})
			if err != nil {
				t.Fatal(err)
			}
			var armed, launched atomic.Bool
			nextDone := make(chan error, 1)
			s.Model = &loopChatFixture{}
			s.Decide = func(context.Context, LoopState) (NextAction, error) {
				armed.Store(true)
				return NextAction{Kind: action}, nil
			}
			// Force a different claimant exactly before the non-locking end snapshot.
			// The fixed implementation holds the SQL row lock through this read;
			// the claimant must wait until both state and trace commit together.
			err = db.Callback().Query().Before("gorm:query").Register("test:loop-snapshot-order", func(tx *gorm.DB) {
				// Advance now derives a cancellable context for its lease heartbeat.
				// Match the inherited marker rather than the context object's identity.
				if tx.Statement.Context.Value(struct{}{}) != "primary advance" || !armed.Load() || tx.Statement.Schema == nil || tx.Statement.Schema.Table != "agent_runs" {
					return
				}
				if _, locked := tx.Statement.Clauses["FOR"]; locked || !launched.CompareAndSwap(false, true) {
					return
				}
				go func() {
					background := context.Background()
					lease, err := s.Data.Claim(background, r.ID, time.Minute)
					if err == nil {
						_, _, err = s.Data.ReserveOperation(background, r.ID, lease.Token, "next-action", "next-action", "model", func(raw string) (string, error) {
							var budget BudgetSnapshot
							if err := json.Unmarshal([]byte(raw), &budget); err != nil {
								return "", err
							}
							budget.Spent.ModelCalls += 7
							data, err := json.Marshal(budget)
							return string(data), err
						})
					}
					if err == nil {
						err = s.Data.SaveRun(background, r.ID, lease.Token, map[string]any{"status": "waiting"})
					}
					nextDone <- err
				}()
				select {
				case err := <-nextDone:
					nextDone <- err
				case <-time.After(300 * time.Millisecond):
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Advance(ctx, r.ID); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-nextDone:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("next claimant did not finish")
			}
			traces, err := s.Data.Traces(context.Background(), r.ID)
			if err != nil {
				t.Fatal(err)
			}
			view := BuildLoopReport(traces)
			if len(view.Rounds) != 1 {
				t.Fatalf("missing trace: %+v", view)
			}
			wantCalls, wantStatus := int64(0), "waiting"
			if action == "model" {
				wantCalls, wantStatus = 1, "reviewing"
			}
			if got := view.Rounds[0]; got.BudgetAfter == nil || got.BudgetAfter.ModelCalls != wantCalls || got.StatusAfter != wantStatus {
				t.Fatalf("next claimant corrupted earlier snapshot: %+v", got)
			}
		})
	}
}
