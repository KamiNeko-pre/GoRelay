package aiapp

import (
	"encoding/json"
	"time"

	"go-lua-crawler/internal/agentdata"
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
	HostProgress       any                   `json:"host_progress,omitempty"`
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
