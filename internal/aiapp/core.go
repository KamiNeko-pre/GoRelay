package aiapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/engine"
	"go-lua-crawler/internal/knowledge"
	"net/url"
	"strings"
	"time"
)

var ErrCorePending = errors.New("learner core decision is not implemented")

type EvidenceReader = knowledge.EvidenceReader
type ToolResult struct {
	ReplayOf     string           `json:"replay_of,omitempty"`
	RepairOf     string           `json:"repair_of,omitempty"`
	RepairTarget string           `json:"repair_target,omitempty"`
	Handle       string           `json:"handle,omitempty"`
	CallID       string           `json:"tool_call_id"`
	Name         string           `json:"name"`
	Data         json.RawMessage  `json:"data,omitempty"`
	Error        string           `json:"error,omitempty"`
	Failure      *FailureInfo     `json:"failure,omitempty"`
	HTTP         *HTTPObservation `json:"http,omitempty"`
}

type HTTPObservation struct {
	Known    bool  `json:"known"`
	Requests int   `json:"requests"`
	Bytes    int64 `json:"bytes"`
}

// Validate the model call before delegating run/current-version checks to reader.
func ExecuteReadOnlyCall(ctx context.Context, call ToolCall, reader EvidenceReader) (ToolResult, error) {
	out := ToolResult{CallID: call.ID, Name: call.Function.Name}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	if call.ID == "" || call.Type != "function" || call.Function.Name != "read_evidence" || reader == nil {
		return out, fmt.Errorf("only a named read_evidence function call is allowed")
	}
	var args struct {
		ID string `json:"evidence_id"`
	}
	if err := decodeArgs(call.Function.Arguments, &args); err != nil {
		return out, err
	}
	if strings.TrimSpace(args.ID) == "" {
		return out, fmt.Errorf("evidence_id required")
	}
	evidence, err := reader.ReadEvidence(ctx, args.ID)
	if err != nil {
		return out, err
	}
	return toolData(call, evidence)
}

type LoopState struct {
	Run             agentdata.Run
	Spec            *TaskSpec
	Messages        []Message
	Children        []agentdata.RunTask
	Operations      []agentdata.Operation
	Budget          BudgetSnapshot
	TaskResults     []ChildResult
	Result          *TaskResult
	Progress        TaskProgress
	EvidenceCatalog []map[string]any
	HostProgress    HostProgress
}
type NextAction struct {
	WakeAt       *time.Time
	HostStop     bool
	Kind         string
	Calls        []ToolCall
	Status       string
	Reason       string
	ModelAttempt int // explicit bounded retry ordinal; zero is the first attempt.
}

// The task Agent Loop selects model/tools/wait/finish from
// persisted observations and the model proposal. SQL, tool execution, feedback,
// tracing and recovery live in Service.Advance. Reason is a brief decision
// summary for review, not hidden model reasoning.
func RunTaskLoop(ctx context.Context, state LoopState) (NextAction, error) {
	if err := ctx.Err(); err != nil {
		return NextAction{}, err
	}
	if state.Run.IsTerminal() || state.Run.Cancelled || state.Budget.Cancelled {
		return NextAction{}, agentdata.ErrConflict
	}
	stop := func(reason string) (NextAction, error) {
		status := "failed"
		if state.Result != nil && state.Result.Coverage.PartialValidRecords > 0 {
			status = "partial"
		}
		return NextAction{Kind: "finish", Status: status, Reason: reason, HostStop: true}, nil
	}
	budget, err := NewRunBudget(state.Budget)
	if err != nil {
		return NextAction{}, err
	}
	if state.Budget.LoopVersion >= 5 && state.HostProgress.Recovery.Phase == "closed" {
		return stop("bounded recovery and one conclusion round exhausted; preserve supported results and gaps")
	}
	// A model proposal must receive its tool replies before another model request.
	if len(state.Messages) > 0 {
		last := state.Messages[len(state.Messages)-1]
		if last.Role == "assistant" && len(last.ToolCalls) > 0 {
			saved := map[string]bool{}
			for _, op := range state.Operations {
				saved[op.Key] = true
			}
			fresh := int64(0)
			for _, call := range last.ToolCalls {
				if !saved["tool:"+call.ID] {
					fresh++
				}
			}
			if err := budget.Reserve(CallCost{ToolCalls: fresh}, time.Now()); err != nil {
				return stop(err.Error())
			}
			return NextAction{Kind: "tools", Calls: last.ToolCalls, Reason: "execute the current model proposal"}, nil
		}
		// Only an executed, successful finish tool reply can propose completion.
		for n := len(state.Messages) - 1; n >= 0 && state.Messages[n].Role == "tool"; n-- {
			var result ToolResult
			if json.Unmarshal([]byte(state.Messages[n].Content), &result) != nil || result.Name != "finish_task" || result.Error != "" {
				continue
			}
			// Model feedback is a bounded view. Completion is owned by the
			// current Run's durable tool receipt and freshly evaluated results.
			for _, op := range state.Operations {
				if op.RunID == state.Run.ID && op.Kind == "finish_task" && op.Key == "tool:"+result.CallID && op.Stage == "succeeded" {
					var saved ToolResult
					if json.Unmarshal([]byte(op.ResultJSON), &saved) == nil && saved.CallID == result.CallID && saved.Name == result.Name {
						result = saved
					}
					break
				}
			}
			var finish struct {
				Status string `json:"status"`
				Reason string `json:"reason"`
			}
			if json.Unmarshal(result.Data, &finish) != nil {
				continue
			}
			valid := finish.Status == "failed" || (state.Result != nil && ((finish.Status == "completed" && state.Spec != nil && taskDeliverySatisfied(state.Result)) || (finish.Status == "partial" && state.Result.Coverage.PartialValidRecords > 0)))
			if valid {
				return NextAction{Kind: "finish", Status: finish.Status, Reason: finish.Reason}, nil
			}
		}
	}
	if state.Budget.LoopVersion >= 5 && state.HostProgress.Recovery.Phase == "concluding" {
		if failure := latestModelFailure(state.Operations); failure != nil && failure.stops() {
			return stop(failure.Message)
		}
		if err := budget.Reserve(CallCost{ModelCalls: 1}, time.Now()); err != nil {
			return stop(err.Error())
		}
		return NextAction{Kind: "model", Reason: "one bounded conclusion using only saved evidence"}, nil
	}
	if failure := latestModelFailure(state.Operations); failure != nil {
		if failure.stops() {
			return stop(failure.Message)
		}
		if failure.RetryAt != nil && time.Now().Before(*failure.RetryAt) {
			if !state.Run.Deadline.IsZero() && !failure.RetryAt.Before(state.Run.Deadline) {
				return stop("provider retry cannot fit remaining task deadline")
			}
			return NextAction{Kind: "wait", Reason: "await retry time from original model failure receipt", WakeAt: failure.RetryAt}, nil
		}
	}
	// Wait for unchanged pending work, but let the Agent consume newly finished
	// siblings without waiting for the slowest source. No model polling is needed
	// once that terminal snapshot has already been observed.
	for _, row := range state.TaskResults {
		if !row.Task.Status.IsTerminal() && !unobservedTerminalChildren(state) {
			return NextAction{Kind: "wait", Reason: "await submitted acquisition tasks"}, nil
		}
	}
	if state.Budget.RetryNotBefore != nil && time.Now().Before(*state.Budget.RetryNotBefore) {
		return NextAction{Kind: "wait", Reason: "await persisted provider retry backoff", WakeAt: state.Budget.RetryNotBefore}, nil
	}
	if len(state.Messages) > 0 {
		last := state.Messages[len(state.Messages)-1]
		if last.Role == "assistant" && len(last.ToolCalls) == 0 && len(toolCooldowns(state.Operations, time.Now())) > 0 {
			return NextAction{Kind: "wait", Reason: "model proposed no actions while tool provider is cooling down"}, nil
		}
	}
	// V4 separates monetary admission from liveness: Unlimited never excuses
	// repeated ineffective behaviour, while verified work renews the window.
	// This is deliberate: an unlimited diagnostic removes cost ceilings, not the
	// requirement to make progress.
	if _, count := repeatedObservationCycle(state.Operations); state.Budget.LoopVersion < 5 && count >= 3 {
		return stop("unchanged tool-call cycle repeated after recovery feedback; preserve supported results and disclose unmet requirements")
	}
	if state.Budget.LoopVersion >= 2 && state.Budget.LoopVersion < 5 && (!state.Budget.Unlimited || state.Budget.LoopVersion >= 4) && state.HostProgress.NoProgressModelRounds >= hostProgressLimit(state.Budget) {
		return stop("host progress: consecutive model rounds produced no new acquired material, accepted records, validated capability or current evidence")
	}
	if state.Budget.LoopVersion == 4 && state.HostProgress.ExplorationRounds >= 2*explorationReviewWindow(state) {
		return stop("host progress: initial investigation and autonomous recovery produced no new accepted records or useful capability; preserved results remain available, unmet requirements remain gaps")
	}
	if state.Budget.LoopVersion < 2 && repeatedObservationRounds(state.Operations) >= 4 {
		return stop("no progress: repeated the same unchanged observation after convergence feedback")
	}
	attempt := 0
	for n := len(state.Operations) - 1; n >= 0; n-- {
		op := state.Operations[n]
		if op.Kind != "model" {
			continue
		}
		if op.Stage == "succeeded" {
			break
		}
		attempt++
	}
	if attempt > 2 {
		return stop("model request failed after three bounded attempts")
	}
	if err := budget.Reserve(CallCost{ModelCalls: 1}, time.Now()); err != nil {
		return stop(err.Error())
	}
	return NextAction{Kind: "model", ModelAttempt: attempt, Reason: "ask model using persisted task and tool observations"}, nil
}

type AdmissionContext struct {
	Candidate        Candidate
	Current          agentdata.SourceHead
	PolicyVersion    string
	ValidatorVersion string
	Artifact         agentdata.ProgramArtifact
	Spec             *TaskSpec
	Scope            []string
	Mode             string // register or explicit rollback; rollback has no new Run spec.
}
type AdmissionDecision struct {
	Allowed bool
	Reason  string
	Issues  []AdmissionIssue `json:"issues,omitempty"`
}
type AdmissionIssue struct {
	Check   string `json:"check"`
	Message string `json:"message"`
}

// Admission uses host-persisted trial facts; semantic correctness remains an eval.
func RegisterValidatedSource(ctx context.Context, in AdmissionContext) (AdmissionDecision, error) {
	if err := ctx.Err(); err != nil {
		return AdmissionDecision{}, err
	}
	issues := []AdmissionIssue{}
	reject := func(check, reason string) { issues = append(issues, AdmissionIssue{Check: check, Message: reason}) }
	c, a := in.Candidate, in.Artifact
	if in.Mode != "register" && in.Mode != "rollback" {
		reject("mode", "unknown admission mode")
	}
	if a.Kind != "adapter" {
		reject("artifact_kind", fmt.Sprintf("candidate kind=%q cannot be published; register_source requires kind=adapter. Reuse the program with validate_program(kind=adapter, mode=acquisition), then register the new candidate and submit_acquisition", a.Kind))
	}
	if c.ID == "" || c.SourceID == "" || c.ArtifactHash == "" || c.ArtifactHash != a.Hash || a.Code == "" {
		reject("identity", "source candidate and adapter identity differ")
	}
	if c.PolicyVersion != in.PolicyVersion || a.PolicyVersion != in.PolicyVersion || c.ValidatorVersion != in.ValidatorVersion {
		reject("policy", "policy or validator version differs")
	}
	if in.Current.SourceID != "" && in.Current.SourceID != c.SourceID {
		reject("source_head", "foreign source head")
	}
	if c.ParentHash != in.Current.ArtifactHash || c.ParentEpoch != in.Current.Epoch {
		reject("parent", "candidate was validated against a different parent")
	}
	var output TaskSpec
	outputValid := json.Unmarshal([]byte(a.OutputSchema), &output) == nil && output.Validate() == nil
	if !outputValid {
		reject("output_contract", "invalid adapter output contract")
	}
	if in.Mode == "register" {
		if in.Spec == nil {
			reject("task_contract", "task requirements required")
		} else if outputValid {
			fields := make([]string, len(output.RequiredFields))
			for n, f := range output.RequiredFields {
				fields[n] = f.Name
			}
			expected, err := OutputContract(*in.Spec, fields)
			if err != nil || expected.SchemaVersion != output.SchemaVersion || agentdata.Hash(expected.RequiredFields) != agentdata.Hash(output.RequiredFields) || agentdata.Hash(expected.JoinKeys) != agentdata.Hash(output.JoinKeys) {
				reject("task_fields", "adapter does not match frozen task fields")
			}
		}
	}
	var artifactScope []string
	scopeValid := json.Unmarshal([]byte(a.ScopeJSON), &artifactScope) == nil
	if !scopeValid {
		reject("scope", "invalid adapter scope")
	}
	if in.Mode == "register" && len(in.Scope) > 0 {
		if len(artifactScope) == 0 {
			reject("scope", "unrestricted adapter exceeds task scope")
		}
		for _, domain := range artifactScope {
			if !admissionHostAllowed(domain, in.Scope) {
				reject("scope", "adapter scope exceeds task scope")
				break
			}
		}
	}
	var proof struct {
		Coverage CoverageReport `json:"coverage"`
	}
	var result engine.ProgramResult
	proofValid := json.Unmarshal([]byte(c.ChecksJSON), &proof) == nil && proof.Coverage.ValidRecords > 0 && len(proof.Coverage.Issues) == 0
	resultValid := json.Unmarshal([]byte(c.ResultJSON), &result) == nil && c.ResultHash == agentdata.Hash(result)
	if !proofValid || !resultValid {
		reject("trial_proof", "missing or inconsistent trial proof")
	}
	for _, receipt := range result.Requests {
		if in.Mode == "register" && len(in.Scope) > 0 {
			for _, raw := range []string{receipt.URL, receipt.RequestedURL} {
				if raw != "" {
					u, err := url.Parse(raw)
					if err != nil || !admissionHostAllowed(u.Hostname(), in.Scope) {
						reject("trial_scope", "trial fetched outside task scope")
					}
				}
			}
		}
	}
	if outputValid && resultValid {
		records, err := BindRecords(result, RecordBinding{SourceID: c.SourceID, DataVersion: a.Hash, TaskID: 1, SchemaVersion: output.SchemaVersion, KeyFields: output.JoinKeys})
		if err != nil || len(records) == 0 {
			reject("trial_records", "trial records lack successful fetch provenance")
		} else {
			if err := requireSupportedFields(output, records); err != nil {
				reject("field_evidence", err.Error())
			}
			if in.Spec != nil {
				if err := requireSupportedFields(*in.Spec, records); err != nil {
					reject("field_evidence", err.Error())
				}
			}
			coverage, err := EvaluateRecords(output, records, time.Now())
			if err != nil || coverage.ValidRecords != proof.Coverage.ValidRecords || len(coverage.Issues) > 0 {
				reject("trial_records", "trial no longer meets output contract")
			}
		}
	}
	if len(issues) > 0 {
		return AdmissionDecision{Reason: issues[0].Message, Issues: issues}, nil
	}
	return AdmissionDecision{Allowed: true, Reason: "validated public adapter with matching contract and parent"}, nil
}

func admissionHostAllowed(host string, domains []string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, domain := range domains {
		domain = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(domain), "."))
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}
