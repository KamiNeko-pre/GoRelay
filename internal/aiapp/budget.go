package aiapp

import (
	"encoding/json"
	"fmt"
	"go-lua-crawler/internal/engine"
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

// Historical quota fields remain decodable. This default starts correction
// review; only unchanged post-feedback behavior without new evidence stops work.
// The caller requires correction after unproductive rounds without progress.
const defaultStagnationModelCalls = 2

func DefaultBudgetLimits() BudgetLimits {
	return BudgetLimits{StagnationModelCalls: defaultStagnationModelCalls}
}

func configuredBudgetLimits() (BudgetLimits, error) { return DefaultBudgetLimits(), nil }

type BudgetSnapshot struct {
	ProgramLimits  *engine.ProgramLimits `json:"program_limits,omitempty"`
	PromptPolicy   *PromptPolicy         `json:"prompt_policy,omitempty"`
	WorkDeadline   time.Time             `json:"work_deadline,omitempty"`
	LoopVersion    int                   `json:"loop_version,omitempty"` // Host-owned; record-only history upgrades to the current crawler protocol.
	RetryNotBefore *time.Time            `json:"retry_not_before,omitempty"`
	Limits         BudgetLimits          `json:"limits"`
	Spent          CallCost              `json:"spent"`
	ProgressBase   CallCost              `json:"progress_base,omitempty"` // Host-renewed work window; Spent remains the lifetime ledger.
	Deadline       time.Time             `json:"deadline"`
	Cancelled      bool                  `json:"cancelled"`
	Candidates     map[string]int        `json:"candidates"`
	Unlimited      bool                  `json:"unlimited,omitempty"` // Legacy snapshot field; no longer an execution mode.
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

// Usage is persisted with the Operation before dispatch. Only caller cancellation
// and an explicit caller deadline affect admission here.
func NewRunBudget(snapshot BudgetSnapshot) (*RunBudget, error) {
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
	for _, requested := range []int64{cost.ModelCalls, cost.RetrievalCalls, cost.SearchCalls, cost.ToolCalls, cost.HTTPRequests, cost.HTTPBytes} {
		if requested < 0 {
			return fmt.Errorf("negative reservation")
		}
	}
	c := b.snapshot.Spent
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
	b.snapshot.Candidates[adaptationKey]++
	return nil
}
