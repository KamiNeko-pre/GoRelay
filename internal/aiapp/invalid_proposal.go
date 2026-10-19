package aiapp

import (
	"encoding/json"
	"errors"
	"fmt"
)

// A received response was examined and rejected; this is not an unknown
// transport outcome. Its already reported usage must still be settled.
var ErrInvalidProposal = errors.New("invalid model proposal")

type invalidProposalError struct {
	Path   string
	Reason string
}

func (e *invalidProposalError) Error() string {
	return fmt.Sprintf("%s at %s: %s", ErrInvalidProposal, e.Path, e.Reason)
}
func (e *invalidProposalError) Unwrap() error { return ErrInvalidProposal }

func invalidProposal(path, reason string) error {
	return &invalidProposalError{Path: path, Reason: reason}
}

func rejectedProposalFeedback(cause error, response ChatResponse, request ChatRequest) *FailureInfo {
	details := map[string]any{"finish_reason": response.FinishReason, "max_completion_tokens": request.MaxCompletionTokens, "tools_dispatched": false}
	var invalid *invalidProposalError
	if errors.As(cause, &invalid) {
		details["field"] = invalid.Path
	}
	if response.Usage != nil {
		details["reported_completion_tokens"] = response.Usage.CompletionTokens
	}
	f := &FailureInfo{Kind: "rejected_proposal", Message: cause.Error(), Category: "validation", Code: "MODEL_PROPOSAL_INVALID", ExecutionStatus: "not_started", Details: details,
		Recovery: &ToolRecovery{Strategy: "correct_model_proposal", Instruction: "Return one complete assistant decision with named function calls and arguments matching the offered schemas. Correct the rejected field in details; keep existing observations and do not repeat completed tools.", Steps: []RecoveryStep{{Action: "apply"}}}}
	if response.FinishReason == "length" {
		f.Code = "MODEL_RESPONSE_TRUNCATED"
		details["termination_source"] = "provider_finish_reason"
		limitStatus := "usage_unavailable"
		if response.Usage != nil && response.Usage.CompletionTokens >= 0 {
			limitStatus = "no_requested_limit"
			if request.MaxCompletionTokens > 0 {
				limitStatus = "reported_below_requested_limit"
				if response.Usage.CompletionTokens >= int64(request.MaxCompletionTokens) {
					limitStatus = "reported_at_or_above_requested_limit"
				}
			}
		}
		details["output_limit_status"] = limitStatus
		// A termination marker can coexist with a visible syntax error. Report
		// that error without accepting or dispatching any part of the proposal.
		argumentErrors := []map[string]any{}
		for i, call := range response.Message.ToolCalls {
			var args map[string]json.RawMessage
			err := json.Unmarshal([]byte(call.Function.Arguments), &args)
			if err == nil && args != nil {
				continue
			}
			diagnostic := map[string]any{"field": fmt.Sprintf("tool_calls/%d/function.arguments", i), "tool": call.Function.Name, "call_id": call.ID, "expected": "JSON object"}
			if err != nil {
				decoded := argumentDecodeFailure(err, 0)
				diagnostic["reason"], diagnostic["byte_offset"] = decoded.Reason, decoded.Offset
				if path := malformedArgumentPath(call.Function.Arguments); path != "" {
					diagnostic["argument_path"] = path
					diagnostic["field"] = fmt.Sprintf("tool_calls/%d/function.arguments/%s", i, path)
					diagnostic["expected"] = argumentSchemaExpected(request, call.Function.Name, path)
				}
			} else {
				diagnostic["reason"] = "expected a JSON object, received null"
			}
			argumentErrors = append(argumentErrors, diagnostic)
		}
		if len(argumentErrors) > 0 {
			details["argument_errors"] = argumentErrors
		}
		f.Recovery.Instruction = "The provider reported finish_reason=length; no tool ran. That signal alone does not prove the configured output budget was exhausted: compare reported_completion_tokens with max_completion_tokens and output_limit_status. Correct each argument_errors entry at its field, argument_path, expected type and byte_offset. JSON string values must be quoted; an array of string must be encoded as an array containing quoted strings (or []), never as bare prose; return one complete function call with a valid JSON argument object matching its schema. Put the full program only in the tool's code argument, without duplicating it in explanations. Reuse saved selectors, field pairs, pagination and completed results. If a complete valid proposal again receives length below the shown cap, report the provider/gateway blocker instead of restarting discovery or rewriting working acquisition code."
	}
	return f
}
