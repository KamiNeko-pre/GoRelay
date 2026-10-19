package aiapp

import (
	"context"
	"encoding/json"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/knowledge"
	"time"
)

// One Advance is a persisted scheduling step, not necessarily one paid model
// request. Model operations and their Usage remain the billing source of truth.
type LoopRound struct {
	RoundID            string                `json:"round_id"`
	Stage              string                `json:"stage"`
	StartedAt          time.Time             `json:"started_at"`
	FinishedAt         *time.Time            `json:"finished_at,omitempty"`
	DurationMS         *int64                `json:"duration_ms,omitempty"`
	Action             string                `json:"action,omitempty"`
	Reason             string                `json:"reason,omitempty"`
	RequestedStatus    string                `json:"requested_status,omitempty"`
	ModelAttempt       int                   `json:"model_attempt"`
	StatusBefore       string                `json:"status_before"`
	StatusAfter        string                `json:"status_after,omitempty"`
	BudgetBefore       *CallCost             `json:"reserved_budget_before,omitempty"`
	BudgetAfter        *CallCost             `json:"reserved_budget_after,omitempty"`
	CoverageBefore     *LoopCoverage         `json:"coverage_before,omitempty"`
	CoverageAfter      *LoopCoverage         `json:"coverage_after,omitempty"`
	Tools              []LoopToolObservation `json:"tools"`
	ProposedTools      []LoopToolObservation `json:"proposed_tools,omitempty"`
	CompletionRejected bool                  `json:"completion_rejected"`
	Error              string                `json:"error,omitempty"`
	HostProgress       *HostProgress         `json:"host_progress,omitempty"`
}

type LoopToolObservation struct {
	CallID string `json:"call_id"`
	Name   string `json:"name"`
	Error  string `json:"error,omitempty"`
}

type LoopCoverage struct {
	Satisfied       bool     `json:"satisfied"`
	ValidRecords    int      `json:"valid_records"`
	PartialRecords  int      `json:"partial_records"`
	MissingFields   []string `json:"missing_fields"`
	MissingEntities []string `json:"missing_entities"`
	Conflicts       int      `json:"conflicts"`
}

func loopCoverage(result *TaskResult) *LoopCoverage {
	if result == nil {
		return nil
	}
	view := &LoopCoverage{Satisfied: result.Coverage.Satisfied, ValidRecords: result.Coverage.ValidRecords, PartialRecords: result.Coverage.PartialValidRecords, MissingFields: []string{}, MissingEntities: result.Coverage.MissingEntities, Conflicts: len(result.Conflicts)}
	for _, field := range result.Coverage.Fields {
		if len(field.Records) == 0 {
			view.MissingFields = append(view.MissingFields, field.Field)
		}
	}
	return view
}

// Keep the run row locked until the transition and its end snapshot commit.
// A later claimant must not supply this round's budget, plan or status facts.
func (s *Service) saveLoopRun(ctx context.Context, r agentdata.Run, updates map[string]any, round *LoopRound, cause error) error {
	if status, ok := updates["status"].(string); ok && status != "waiting" {
		updates["next_wake_at"] = nil
	}
	return s.Data.WithOwnedRun(ctx, r.ID, r.Token, func(store *agentdata.Store) error {
		if err := store.SaveRun(ctx, r.ID, r.Token, updates); err != nil {
			return err
		}
		scoped := *s
		scoped.Data = store
		scoped.Knowledge = knowledge.NewStore(store.DB)
		return scoped.finishLoopRound(ctx, r.ID, round, cause)
	})
}

func (s *Service) finishLoopRound(ctx context.Context, id string, round *LoopRound, cause error) error {
	current, err := s.Data.Run(ctx, id)
	if err != nil {
		return err
	}
	var budget BudgetSnapshot
	if err := json.Unmarshal([]byte(current.BudgetJSON), &budget); err != nil {
		return err
	}
	round.BudgetAfter = &budget.Spent
	round.StatusAfter = current.Status
	state, err := s.state(ctx, current)
	if err != nil {
		return err
	}
	host := s.advanceHostProgress(ctx, state.HostProgress, state)
	if budget.LoopVersion >= 4 && state.HostProgress.Signature != "" && host.Signature != state.HostProgress.Signature {
		budget.ProgressBase = budget.Spent
		rawBudget, _ := json.Marshal(budget)
		if err := s.Data.DB.WithContext(ctx).Model(&agentdata.Run{}).Where("id = ?", id).Update("budget_json", string(rawBudget)).Error; err != nil {
			return err
		}
	}
	rawHost, _ := json.Marshal(host)
	if err := s.Data.DB.WithContext(ctx).Model(&agentdata.Run{}).Where("id = ?", id).Update("host_progress_json", string(rawHost)).Error; err != nil {
		return err
	}
	round.HostProgress = &host
	if round.Action == "model" && cause == nil {
		var messages []Message
		if err := json.Unmarshal([]byte(current.PlanJSON), &messages); err != nil {
			return err
		}
		if len(messages) > 0 && messages[len(messages)-1].Role == "assistant" {
			for _, call := range messages[len(messages)-1].ToolCalls {
				round.ProposedTools = append(round.ProposedTools, LoopToolObservation{CallID: call.ID, Name: call.Function.Name})
			}
		}
	}
	if state.Result != nil {
		round.CoverageAfter = loopCoverage(state.Result)
	}
	end := time.Now()
	duration := end.Sub(round.StartedAt).Milliseconds()
	round.FinishedAt, round.DurationMS, round.Stage = &end, &duration, "finished"
	if cause != nil {
		round.Error = cause.Error()
	}
	return s.Data.AddTrace(ctx, id, "loop_round", round)
}

type LoopSummary struct {
	Scope                string `json:"scope"`
	CompletedAdvances    int    `json:"finished_advances"`
	IncompleteAdvances   int    `json:"incomplete_advances"`
	ModelAdvances        int    `json:"model_advances"`
	ToolAdvances         int    `json:"tool_advances"`
	WaitAdvances         int    `json:"wait_advances"`
	CompletionRejections int    `json:"completion_rejections"`
	ToolSuccesses        int    `json:"tool_successes"`
	ToolFailures         int    `json:"tool_failures"`
}

type LoopReport struct {
	Rounds           []LoopRound `json:"rounds"`
	Summary          LoopSummary `json:"summary"`
	HistoryTruncated bool        `json:"history_truncated"`
}

// A start without a finish remains unknown after interruption. Finished rows
// contain a complete snapshot, so they are readable even in a paginated window.
func BuildLoopReport(traces []agentdata.Trace) LoopReport {
	view := LoopReport{Rounds: []LoopRound{}, Summary: LoopSummary{Scope: "returned_trace_window"}}
	positions := map[string]int{}
	for _, trace := range traces {
		if trace.Stage != "loop_round" {
			continue
		}
		var round LoopRound
		if json.Unmarshal([]byte(trace.DataJSON), &round) != nil || round.RoundID == "" {
			continue
		}
		position, exists := positions[round.RoundID]
		if !exists {
			positions[round.RoundID] = len(view.Rounds)
			view.Rounds = append(view.Rounds, round)
		} else if round.Stage == "finished" {
			view.Rounds[position] = round
		}
	}
	for _, round := range view.Rounds {
		if round.Stage != "finished" {
			view.Summary.IncompleteAdvances++
			continue
		}
		view.Summary.CompletedAdvances++
		switch round.Action {
		case "model":
			view.Summary.ModelAdvances++
		case "tools", "monitor_submit":
			view.Summary.ToolAdvances++
		case "wait":
			view.Summary.WaitAdvances++
		}
		if round.CompletionRejected {
			view.Summary.CompletionRejections++
		}
		for _, tool := range round.Tools {
			if tool.Error == "" {
				view.Summary.ToolSuccesses++
			} else {
				view.Summary.ToolFailures++
			}
		}
	}
	return view
}
