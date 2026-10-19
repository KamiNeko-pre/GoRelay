package aiapp

import (
	"context"
	"encoding/json"
	"errors"
	"net"

	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/agentstatus"
	"gorm.io/gorm"
)

func OperationalStatus(cause error) agentstatus.Status {
	var carrier agentstatus.Carrier
	var provider *ProviderError
	var runner *RunnerError
	var parameter *ParameterError
	var arguments *ArgumentDecodeError
	var syntax *json.SyntaxError
	var mismatch *json.UnmarshalTypeError
	var contract *ToolContractError
	var network net.Error
	switch {
	case errors.As(cause, &carrier):
		return carrier.OperationalStatus()
	case errors.Is(cause, ErrProviderQuota):
		return agentstatus.Of(agentstatus.BalanceUnavailable)
	case errors.Is(cause, agentdata.ErrSpendAdmission), errors.Is(cause, agentdata.ErrDailyQuota):
		return agentstatus.Of(agentstatus.SpendPolicyHold)
	case errors.Is(cause, context.DeadlineExceeded):
		return agentstatus.Of(agentstatus.TaskDeadline)
	case errors.Is(cause, context.Canceled):
		return agentstatus.Of(agentstatus.TaskCancelled)
	case errors.As(cause, &contract):
		return OperationalFailureStatus(&contract.Failure)
	case errors.As(cause, &runner):
		switch runner.Code {
		case "execution_failed", "lua_error", "budget_exceeded", "timeout", "permission_denied":
			return agentstatus.Of(agentstatus.AgentCorrection)
		case "busy":
			return agentstatus.Of(agentstatus.StateConflict)
		default:
			return agentstatus.Of(agentstatus.SystemDependency)
		}
	case errors.As(cause, &parameter), errors.As(cause, &arguments), errors.Is(cause, ErrInvalidProposal):
		return agentstatus.Of(agentstatus.AgentCorrection)
	case errors.As(cause, &syntax), errors.As(cause, &mismatch), errors.Is(cause, gorm.ErrRecordNotFound):
		return agentstatus.Of(agentstatus.InvalidRequest)
	case errors.Is(cause, agentdata.ErrConflict):
		return agentstatus.Of(agentstatus.StateConflict)
	case errors.As(cause, &provider):
		return agentstatus.Of(agentstatus.SystemDependency)
	case errors.Is(cause, ErrModelNotReady):
		return agentstatus.Of(agentstatus.SystemConfiguration)
	case errors.As(cause, &network):
		return agentstatus.Of(agentstatus.SystemDependency)
	default:
		return agentstatus.Of(agentstatus.Unclassified)
	}
}

// This uses trusted failure types/categories, not words inside model/source text.
func OperationalFailureStatus(f *FailureInfo) agentstatus.Status {
	if f == nil {
		return agentstatus.Of(agentstatus.Unclassified)
	}
	if f.Kind == "quota" {
		return agentstatus.Of(agentstatus.BalanceUnavailable)
	}
	if f.Kind == "spend_admission" {
		return agentstatus.Of(agentstatus.SpendPolicyHold)
	}
	if f.Provider != nil || f.Kind == "unknown_model_outcome" {
		return agentstatus.Of(agentstatus.SystemDependency)
	}
	if f.Kind == "invalid_proposal" || f.Category == "validation" {
		return agentstatus.Of(agentstatus.AgentCorrection)
	}
	switch f.Code {
	case "SOURCE_HTTP_ERROR", "SOURCE_TRANSPORT_ERROR", "SOURCE_SCOPE_DENIED":
		return agentstatus.Of(agentstatus.SourceUnavailable)
	case "TOOL_ALREADY_CALLED", "BUSINESS_OPERATION_ALREADY_EXECUTED":
		return agentstatus.Of(agentstatus.AgentDuplicate)
	case "execution_failed", "lua_error", "budget_exceeded", "timeout", "permission_denied", "SOURCE_FETCH_LIMIT":
		return agentstatus.Of(agentstatus.AgentCorrection)
	case "TOOL_STATE_CONFLICT":
		return agentstatus.Of(agentstatus.SystemState)
	}
	if f.Recovery != nil && (f.Recovery.Strategy == "correct_program" || f.Recovery.Strategy == "correct_arguments" || f.Recovery.Strategy == "correct_validated_candidate") {
		return agentstatus.Of(agentstatus.AgentCorrection)
	}
	return agentstatus.Of(agentstatus.Unclassified)
}
