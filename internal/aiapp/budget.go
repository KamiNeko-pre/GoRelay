package aiapp

import (
	"encoding/json"
	"fmt"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/engine"
	"os"
	"strconv"
	"sync"
	"time"
)

type CallCost struct {
	ModelCalls     int64 `json:"model_calls"`
	RetrievalCalls int64 `json:"retrieval_calls,omitempty"`
	SearchCalls    int64 `json:"search_calls"`
	ToolCalls      int64 `json:"tool_calls"`
	HTTPRequests   int64 `json:"http_requests"`
	HTTPBytes      int64 `json:"http_bytes"`
}
type BudgetLimits struct {
	StagnationModelCalls       int64 `json:"stagnation_model_calls,omitempty"`
	ModelCalls                 int64 `json:"model_calls"`
	RetrievalCalls             int64 `json:"retrieval_calls,omitempty"`
	SearchCalls                int64 `json:"search_calls"`
	ToolCalls                  int64 `json:"tool_calls"`
	HTTPRequests               int64 `json:"http_requests"`
	HTTPBytes                  int64 `json:"http_bytes"`
	CodeCandidatesPerOperation int   `json:"code_candidates_per_operation"`
}

// DefaultBudgetLimits are the frozen per-run ceilings.
//
// StagnationModelCalls is the number of consecutive model rounds that may pass
// without new acquired material, accepted records, a validated capability or
// current evidence before the host ends the run. It is a liveness bound, not a
// cost bound: it exists so a run cannot repeat ineffective work forever.
//
// The value is calibrated against observed task shape, not guessed. A bounded
// multi-source acquisition needs at least: locate the listing, read it, follow
// the detail links, read each detail page, then write and trial the program that
// delivers the frozen fields. Across eleven recorded real-model runs the model
// consistently used 12 to 50 tool rounds and was stopped at the sixth
// unproductive one before it could deliver, in every case. Six was below the
// floor the task actually requires, so the bound now reflects the observed work
// while still bounding it: the counter still resets only on real progress, and
// a run that repeats itself is still stopped.
const defaultStagnationModelCalls = 20

func DefaultBudgetLimits() BudgetLimits {
	return BudgetLimits{StagnationModelCalls: defaultStagnationModelCalls, ModelCalls: 12, RetrievalCalls: 12, SearchCalls: 3, ToolCalls: 30, HTTPRequests: 100, HTTPBytes: 16 << 20, CodeCandidatesPerOperation: 2}
}

func configuredBudgetLimits() (BudgetLimits, error) {
	l := DefaultBudgetLimits()
	for _, setting := range []struct {
		name   string
		target *int64
	}{{"GORELAY_RUN_MODEL_CALLS", &l.ModelCalls}, {"GORELAY_RUN_SEARCH_CALLS", &l.SearchCalls}, {"GORELAY_RUN_STAGNATION_CALLS", &l.StagnationModelCalls}} {
		raw := os.Getenv(setting.name)
		if raw == "" {
			continue
		}
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value <= 0 {
			return l, fmt.Errorf("%s must be a positive integer", setting.name)
		}
		*setting.target = value
	}
	return l, nil
}

type BudgetSnapshot struct {
	ProgramLimits  *engine.ProgramLimits  `json:"program_limits,omitempty"`
	PromptPolicy   *PromptPolicy          `json:"prompt_policy,omitempty"`
	WorkDeadline   time.Time              `json:"work_deadline,omitempty"`
	Spend          *agentdata.SpendBudget `json:"spend,omitempty"`
	LoopVersion    int                    `json:"loop_version,omitempty"` // Host-frozen; legacy runs keep their original protocol.
	RetryNotBefore *time.Time             `json:"retry_not_before,omitempty"`
	Limits         BudgetLimits           `json:"limits"`
	Spent          CallCost               `json:"spent"`
	ProgressBase   CallCost               `json:"progress_base,omitempty"` // Host-renewed work window; Spent remains the lifetime ledger.
	Deadline       time.Time              `json:"deadline"`
	Cancelled      bool                   `json:"cancelled"`
	Candidates     map[string]int         `json:"candidates"`
	Unlimited      bool                   `json:"unlimited,omitempty"` // Host-only local experiment; counters persist.
}

// Admission and recovery must use the same host-renewed work window.
func (s BudgetSnapshot) workSpent() CallCost {
	if s.LoopVersion < 4 {
		return s.Spent
	}
	c, b := s.Spent, s.ProgressBase
	return CallCost{ModelCalls: c.ModelCalls - b.ModelCalls, RetrievalCalls: c.RetrievalCalls - b.RetrievalCalls, SearchCalls: c.SearchCalls - b.SearchCalls, ToolCalls: c.ToolCalls - b.ToolCalls, HTTPRequests: c.HTTPRequests - b.HTTPRequests, HTTPBytes: c.HTTPBytes - b.HTTPBytes}
}

type RunBudget struct {
	mu       sync.Mutex
	snapshot BudgetSnapshot
}

func (b BudgetSnapshot) MarshalJSON() ([]byte, error) {
	type plain BudgetSnapshot
	var deadline *time.Time
	if !b.Deadline.IsZero() {
		deadline = &b.Deadline
	}
	return json.Marshal(struct {
		plain
		Deadline *time.Time `json:"deadline"`
	}{plain: plain(b), Deadline: deadline})
}

func configuredRunTimeout() (time.Duration, error) {
	timeout, err := time.ParseDuration(envDefault("GORELAY_RUN_TIMEOUT", "0s"))
	if err != nil || timeout < 0 {
		return 0, fmt.Errorf("GORELAY_RUN_TIMEOUT must be a nonnegative duration; zero means no task timebox")
	}
	return timeout, nil
}

// This is a deterministic enforcement component, not a durable Run store.
// The future SQL Operation transaction must persist reservation before dispatch.
// Failed external attempts keep reservations; restart loads the existing snapshot.
func NewRunBudget(snapshot BudgetSnapshot) (*RunBudget, error) {
	l := snapshot.Limits
	if l.ModelCalls <= 0 || l.SearchCalls <= 0 || l.ToolCalls <= 0 || l.HTTPRequests <= 0 || l.HTTPBytes <= 0 || l.CodeCandidatesPerOperation <= 0 || snapshot.Deadline.IsZero() && !snapshot.Unlimited && snapshot.LoopVersion < 4 {
		return nil, fmt.Errorf("run budgets and deadline must be set")
	}
	c := snapshot.Spent
	if c.ModelCalls < 0 || c.RetrievalCalls < 0 || c.SearchCalls < 0 || c.ToolCalls < 0 || c.HTTPRequests < 0 || c.HTTPBytes < 0 {
		return nil, fmt.Errorf("negative spent budget")
	}
	candidates := map[string]int{}
	for k, v := range snapshot.Candidates {
		if k == "" || v < 0 {
			return nil, fmt.Errorf("invalid adaptation candidate counter")
		}
		candidates[k] = v
	}
	snapshot.Candidates = candidates
	return &RunBudget{snapshot: snapshot}, nil
}
func (b *RunBudget) Snapshot() BudgetSnapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.snapshot
	out.Candidates = map[string]int{}
	for k, v := range b.snapshot.Candidates {
		out.Candidates[k] = v
	}
	return out
}
func (b *RunBudget) Cancel() { b.mu.Lock(); defer b.mu.Unlock(); b.snapshot.Cancelled = true }
func (b *RunBudget) check(now time.Time) error {
	if b.snapshot.Cancelled {
		return fmt.Errorf("run is cancelled")
	}
	if !b.snapshot.Deadline.IsZero() && !now.Before(b.snapshot.Deadline) {
		return fmt.Errorf("run deadline exceeded")
	}
	return nil
}
func (b *RunBudget) Reserve(cost CallCost, now time.Time) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.check(now); err != nil {
		return err
	}
	c, l := b.snapshot.Spent, b.snapshot.Limits
	window := b.snapshot.workSpent()
	amounts := []struct {
		name                    string
		spent, requested, limit int64
	}{{"model_calls", window.ModelCalls, cost.ModelCalls, l.ModelCalls}, {"retrieval_calls", window.RetrievalCalls, cost.RetrievalCalls, l.RetrievalCalls}, {"search_calls", window.SearchCalls, cost.SearchCalls, l.SearchCalls}, {"tool_calls", window.ToolCalls, cost.ToolCalls, l.ToolCalls}, {"http_requests", window.HTTPRequests, cost.HTTPRequests, l.HTTPRequests}, {"http_bytes", window.HTTPBytes, cost.HTTPBytes, l.HTTPBytes}}
	for _, a := range amounts {
		if a.requested < 0 {
			return fmt.Errorf("negative reservation")
		}
		if !b.snapshot.Unlimited && b.snapshot.PromptPolicy == nil && (a.spent > a.limit || a.requested > a.limit-a.spent) {
			cause := fmt.Errorf("run budget exhausted: %s spent=%d requested=%d limit=%d", a.name, a.spent, a.requested, a.limit)
			return &ToolContractError{Cause: cause, Failure: FailureInfo{Kind: "tool", Category: "policy", Code: "RUN_BUDGET_EXHAUSTED", Message: cause.Error(), ExecutionStatus: "not_started", Details: map[string]any{"resource": a.name, "spent": a.spent, "requested": a.requested, "limit": a.limit}, Recovery: &ToolRecovery{Strategy: "deliver_or_stop", Instruction: "This host resource window cannot admit the operation. Use already accepted results for honest partial/failed delivery; changing call IDs, handles or guessed parameters cannot increase the budget. Do not propose more acquisition without a host-proven resource renewal."}}}
		}
	}
	c.ModelCalls += cost.ModelCalls
	c.RetrievalCalls += cost.RetrievalCalls
	c.SearchCalls += cost.SearchCalls
	c.ToolCalls += cost.ToolCalls
	c.HTTPRequests += cost.HTTPRequests
	c.HTTPBytes += cost.HTTPBytes
	b.snapshot.Spent = c
	return nil
}
func (b *RunBudget) ReserveCandidate(adaptationKey string, now time.Time) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.check(now); err != nil {
		return err
	}
	if adaptationKey == "" {
		return fmt.Errorf("stable source/parent/operation key is required")
	}
	if !b.snapshot.Unlimited && b.snapshot.LoopVersion < 4 && b.snapshot.Candidates[adaptationKey] >= b.snapshot.Limits.CodeCandidatesPerOperation {
		return fmt.Errorf("adaptation candidate budget exhausted")
	}
	b.snapshot.Candidates[adaptationKey]++
	return nil
}
