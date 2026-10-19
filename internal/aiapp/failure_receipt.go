package aiapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/knowledge"
	"strings"
	"time"
)

func (s *Service) replyAfterProviderStop(ctx context.Context, r agentdata.Run, call ToolCall) (ToolResult, error) {
	op, fresh, err := s.Data.ReserveOperation(ctx, r.ID, r.Token, "tool:"+call.ID, canonicalToolInput(call.Function), call.Function.Name, nil, agentdata.Hash(call.Function))
	if err != nil {
		return ToolResult{}, err
	}
	if !fresh {
		var out ToolResult
		if (op.Stage == "succeeded" || op.Stage == "failed") && json.Unmarshal([]byte(op.ResultJSON), &out) == nil {
			if out.Error != "" {
				if out.Failure != nil {
					return out, out.Failure.cause()
				}
				return out, errors.New(out.Error)
			}
			return out, nil
		}
		return ToolResult{}, errors.New("prior external outcome remains unknown after provider stop; no repeat dispatched")
	}
	out := ToolResult{CallID: call.ID, Name: call.Function.Name, Error: "not dispatched: an earlier proposal received a permanent provider or spend refusal"}
	out.Failure = &FailureInfo{Kind: "batch_stopped", Message: out.Error, Category: "upstream", Code: "BATCH_NOT_DISPATCHED", ExecutionStatus: "not_started", Recovery: &ToolRecovery{Strategy: "stop", Instruction: out.Error}}
	raw, _ := json.Marshal(out)
	if err = s.Data.CompleteOperation(ctx, r.ID, r.Token, op.ID, "failed", string(raw), ""); err != nil {
		return out, err
	}
	return out, errors.New(out.Error)
}

// Recovery consumes structured transport facts, never matches source/error text.
type FailureInfo struct {
	Kind            string         `json:"kind"`
	Message         string         `json:"message"`
	Provider        *ProviderError `json:"provider,omitempty"`
	RetryAt         *time.Time     `json:"retry_at,omitempty"`
	Category        string         `json:"category,omitempty"`
	Code            string         `json:"code,omitempty"`
	ExecutionStatus string         `json:"execution_status,omitempty"`
	Retryable       bool           `json:"retryable"`
	Details         any            `json:"details,omitempty"`
	Recovery        *ToolRecovery  `json:"recovery,omitempty"`
}

type ToolRecovery struct {
	RepairOf    string             `json:"repair_of,omitempty"`
	Strategy    string             `json:"strategy"`
	Instruction string             `json:"instruction"`
	Avoid       []string           `json:"avoid,omitempty"`
	Tool        string             `json:"tool,omitempty"`
	Steps       []RecoveryStep     `json:"steps,omitempty"`
	ResolvedBy  *RecoveryCondition `json:"resolved_by,omitempty"`
}

// Host-issued completion facts for a cross-tool recovery stage. This is not a
// model-supplied repair_of and cannot authorize replaying an operation.
type RecoveryCondition struct {
	Tool   string            `json:"tool"`
	Fields map[string]string `json:"fields"`
}

type RecoveryStep struct {
	Action          string         `json:"action"`
	Tool            string         `json:"tool,omitempty"`
	Instruction     string         `json:"instruction,omitempty"`
	ArgumentChanges map[string]any `json:"argument_changes,omitempty"`
}

// Every durable tool path, including a failure before a ledger row can be
// reserved, returns the same model-visible envelope. Persistence may fail, but
// that must not erase which tool failed or leave an unclassified error.
func toolFailureReceipt(call ToolCall, out ToolResult, cause error) ToolResult {
	if cause == nil {
		return out
	}
	out.CallID, out.Name, out.Error = call.ID, call.Function.Name, cause.Error()
	if out.Handle == "" {
		out.Handle = call.ID
	}
	if out.Failure == nil || out.Failure.Message != out.Error {
		out.Failure = captureFailure(cause, 0, false)
	}
	if out.Failure.Code == "" {
		out.Failure.Code = "TOOL_EXECUTION_ERROR"
	}
	if out.Failure.Recovery == nil {
		out.Failure.Recovery = &ToolRecovery{Strategy: "inspect_and_correct", Instruction: out.Error + ". Use the saved diagnostic and actual source contract; do not guess or repeat unchanged arguments."}
	}
	if out.Failure.Recovery.Tool == "" {
		out.Failure.Recovery.Tool = call.Function.Name
	}
	bindFailureRecovery(call, &out)
	return out
}

func captureFailure(err error, attempt int, model bool) *FailureInfo {
	if err == nil {
		return nil
	}
	f := &FailureInfo{Message: err.Error()}
	var p *ProviderError
	var cooldown *providerCooldown
	var contract *ToolContractError
	switch {
	case errors.As(err, &contract):
		copy := contract.Failure
		return &copy
	case errors.As(err, &cooldown):
		f.Kind = "cooldown"
		f.RetryAt = &cooldown.Until
		f.Category, f.Code, f.ExecutionStatus, f.Retryable = "upstream", "PROVIDER_COOLDOWN", "not_started", true
		f.Recovery = &ToolRecovery{Strategy: "retry_after_delay", Instruction: f.Message}
		return f
	case !model && errors.Is(err, agentdata.ErrConflict):
		f.Kind, f.Category, f.Code, f.ExecutionStatus = "tool", "state", "TOOL_STATE_CONFLICT", "unknown"
		f.Recovery = &ToolRecovery{Strategy: "stop", Instruction: "The host could not confirm ownership or the saved operation version. Do not guess new tool parameters or replay side effects; the host must reload its Run/operation state."}
		return f
	case errors.Is(err, ErrProviderQuota):
		f.Kind = "quota"
	case errors.Is(err, agentdata.ErrSpendAdmission):
		f.Kind = "spend_admission"
	case errors.Is(err, ErrInferenceBoundary):
		f.Kind = "inference_boundary"
	case errors.Is(err, ErrInvalidProposal):
		f.Kind = "invalid_proposal"
	case errors.As(err, &p):
		copy := *p
		f.Kind = "provider"
		f.Provider = &copy
	case model:
		f.Kind = "unknown_model_outcome"
	default:
		// A tool error is feedback for the next decision, not a reason to repeat
		// the same proposal. Unknown execution must remain unknown until the
		// execution boundary or receipt proves otherwise.
		f.Kind, f.Category, f.Code, f.ExecutionStatus = "tool", "execution", "TOOL_EXECUTION_ERROR", "unknown"
		f.Recovery = &ToolRecovery{Strategy: "inspect_or_stop", Instruction: "Execution failed: " + err.Error() + ". No parameter or program fix has been established for this unclassified failure. Inspect the saved diagnostic and observed source contract; if they provide no supported correction, report this blocker instead of guessing new arguments.", Avoid: []string{"Do not repeat unchanged arguments after the same error; inspect the stored receipt or actual source structure before correcting."}}
		var params *ParameterError
		var arguments *ArgumentDecodeError
		var syntax *json.SyntaxError
		var mismatch *json.UnmarshalTypeError
		var runner *RunnerError
		switch {
		case errors.As(err, &arguments):
			f.Category, f.Code, f.ExecutionStatus = "validation", "ARGUMENT_JSON_ERROR", "not_started"
			f.Details = arguments
			f.Recovery.Strategy = "correct_arguments"
			f.Recovery.Instruction = "Correct the argument field/path and expected type shown in details, or repair the JSON at the reported byte offset. Use the current tool schema; do not rename the call or invent missing resource IDs. Original parser cause: " + err.Error()
		case errors.Is(err, knowledge.ErrInvalidReference):
			f.Category, f.Code, f.ExecutionStatus = "validation", "EVIDENCE_REFERENCE_UNAVAILABLE", "failed"
			f.Recovery = &ToolRecovery{Strategy: "refresh_and_retrieve", Tool: "search_knowledge", Instruction: "The requested evidence reference is unavailable in the current run/version. Search knowledge again and use only the fresh evidence IDs returned; do not repeat the old reference."}
		case errors.As(err, &params):
			f.Category, f.Code = "validation", "PARAMETER_CONTRACT_ERROR"
			f.Details = params
			f.Recovery.Strategy = "correct_arguments"
			f.Recovery.Instruction = "Supply params matching details.required and details.accepted_parameters; details.example shows the accepted value shape. Bind actual values from the task or saved receipts, then change only the reported invalid or missing parameters."
		case errors.As(err, &syntax), errors.As(err, &mismatch):
			f.Category, f.Code = "validation", "ARGUMENT_JSON_ERROR"
			f.Recovery.Strategy = "correct_arguments"
			f.Recovery.Instruction = "Repair JSON syntax/types at the reported field or byte offset using the tool schema. Do not execute the same malformed arguments again. Parser cause: " + err.Error()
		case errors.As(err, &runner):
			f.Code, f.Details = runner.Code, runner
			f.ExecutionStatus = "failed"
			f.Recovery.Strategy = "correct_program"
			f.Recovery.Instruction = "Use the runner code and message in details to locate the failing Lua line or sandbox operation. Correct the program against the published Lua API and observed selectors, then validate the changed program. Changing IDs, refetching, or resubmitting unchanged code cannot repair this runner failure."
			if runner.Code == "budget_exceeded" || runner.Code == "timeout" {
				f.Recovery.Strategy = "partition_acquisition"
				f.Recovery.Instruction = "Compare details.limits with details.observed_io; the host stopped this execution at its admitted IO/output/time resource boundary. Preserve valid extraction logic and partition pagination/detail work into smaller batches with different real URL/params (for example a start page and detail limit), then validate the changed bounded program. Carry continuation and record deduplication across batches. An expired task deadline cannot be extended by this repair; deliver saved results if no task time remains. Do not rerun unchanged params or invent fields."
			}
		}
		return f
	}
	f.Category = "upstream"
	f.Code = strings.ToUpper(f.Kind)
	if f.Kind == "quota" || f.Kind == "spend_admission" || f.Kind == "inference_boundary" {
		f.Category = "policy"
	}
	if f.Kind == "invalid_proposal" {
		f.Category = "validation"
	}
	f.Retryable = f.Provider != nil && f.Provider.Retryable && !f.stops()
	f.ExecutionStatus = "unknown"
	f.Recovery = &ToolRecovery{Strategy: "stop", Instruction: f.Message}
	if !f.stops() {
		when := providerRetryTime(err, attempt, time.Now())
		f.RetryAt = &when
		f.Recovery.Strategy = "retry_after_delay"
	}
	return f
}
func (f *FailureInfo) stops() bool {
	return f != nil && (f.Kind == "quota" || f.Kind == "spend_admission" || f.Kind == "inference_boundary" || f.Kind == "invalid_proposal" || (f.Provider != nil && !f.Provider.Retryable))
}
func (f *FailureInfo) cause() error {
	switch f.Kind {
	case "quota":
		return fmt.Errorf("%w: %s", ErrProviderQuota, f.Message)
	case "spend_admission":
		return fmt.Errorf("%w: %s", agentdata.ErrSpendAdmission, f.Message)
	case "inference_boundary":
		return fmt.Errorf("%w: %s", ErrInferenceBoundary, f.Message)
	case "invalid_proposal":
		return fmt.Errorf("%w: %s", ErrInvalidProposal, f.Message)
	case "provider":
		if f.Provider != nil {
			return f.Provider
		}
	case "cooldown":
		if f.RetryAt != nil {
			return &providerCooldown{Until: *f.RetryAt}
		}
	}
	return errors.New(f.Message)
}

type providerCooldown struct{ Until time.Time }

func (e *providerCooldown) Error() string {
	return "provider cooldown until " + e.Until.UTC().Format(time.RFC3339Nano) + "; read local evidence or wait before a new request"
}

func toolCooldowns(ops []agentdata.Operation, now time.Time) map[string]*FailureInfo {
	active := map[string]*FailureInfo{}
	for _, op := range ops {
		if op.Stage != "failed" {
			continue
		}
		var receipt ToolResult
		if json.Unmarshal([]byte(op.ResultJSON), &receipt) != nil || receipt.Failure == nil {
			continue
		}
		f := receipt.Failure
		if f.Kind != "provider" || f.Provider == nil || !f.Provider.Retryable || f.RetryAt == nil || !now.Before(*f.RetryAt) {
			continue
		}
		if previous := active[receipt.Name]; previous == nil || previous.RetryAt.Before(*f.RetryAt) {
			active[receipt.Name] = f
		}
	}
	return active
}
func latestModelFailure(ops []agentdata.Operation) *FailureInfo {
	for n := len(ops) - 1; n >= 0; n-- {
		op := ops[n]
		if op.Kind != "model" {
			continue
		}
		if op.Stage == "succeeded" {
			return nil
		}
		var response ChatResponse
		if json.Unmarshal([]byte(op.ResultJSON), &response) == nil {
			return response.Failure
		}
		return nil
	}
	return nil
}
