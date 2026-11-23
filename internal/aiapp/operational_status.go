package aiapp

import (
	"context"
	"encoding/json"
	"errors"
	"go-lua-crawler/internal/programrunner"
	"net"

	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/agentstatus"
	"gorm.io/gorm"
)

func OperationalStatus(cause error) agentstatus.Status {
	var carrier agentstatus.Carrier
	var provider *ProviderError
	var runner *programrunner.Error
	var parameter *ParameterError
	var syntax *json.SyntaxError
	var mismatch *json.UnmarshalTypeError
	var network net.Error
	switch {
	case errors.As(cause, &carrier):
		return carrier.OperationalStatus()
	case errors.Is(cause, ErrProviderQuota):
		return agentstatus.Of(agentstatus.BalanceUnavailable)
	case errors.Is(cause, context.DeadlineExceeded):
		return agentstatus.Of(agentstatus.TaskDeadline)
	case errors.Is(cause, context.Canceled):
		return agentstatus.Of(agentstatus.TaskCancelled)
	case errors.As(cause, &runner):
		switch runner.Code {
		case "execution_failed", "lua_error", "budget_exceeded", "memory_limit", "timeout", "permission_denied":
			return agentstatus.Of(agentstatus.AgentCorrection)
		case "busy":
			if runner.ObservedIO == nil {
				return agentstatus.Of(agentstatus.StateConflict)
			}
			return agentstatus.Of(agentstatus.AgentCorrection)
		default:
			return agentstatus.Of(agentstatus.SystemDependency)
		}
	case errors.As(cause, &parameter), errors.Is(cause, ErrInvalidProposal):
		return agentstatus.Of(agentstatus.AgentCorrection)
	case errors.As(cause, &syntax), errors.As(cause, &mismatch), errors.Is(cause, gorm.ErrRecordNotFound):
		return agentstatus.Of(agentstatus.InvalidRequest)
	case errors.Is(cause, agentdata.ErrConflict):
		return agentstatus.Of(agentstatus.StateConflict)
	case errors.As(cause, &provider):
		return agentstatus.Of(agentstatus.SystemDependency)
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
	case "execution_failed", "lua_error", "budget_exceeded", "memory_limit", "timeout", "permission_denied", "SOURCE_FETCH_LIMIT":
		return agentstatus.Of(agentstatus.AgentCorrection)
	case "busy":
		if f.ExecutionStatus == "not_started" {
			return agentstatus.Of(agentstatus.StateConflict)
		}
	case "TOOL_STATE_CONFLICT":
		return agentstatus.Of(agentstatus.SystemState)
	}
	if f.Recovery != nil && (f.Recovery.Strategy == "correct_program" || f.Recovery.Strategy == "correct_arguments" || f.Recovery.Strategy == "correct_validated_candidate") {
		return agentstatus.Of(agentstatus.AgentCorrection)
	}
	return agentstatus.Of(agentstatus.Unclassified)
}
