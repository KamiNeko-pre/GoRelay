// Package agentstatus defines service-owned business codes. Code is never an
// upstream HTTP status. AlertPolicy describes deployment behavior, not a mailer.
package agentstatus

import "fmt"

type Code int

const (
	InvalidRequest           Code = 11001
	StateConflict            Code = 11002
	BalanceUnavailable       Code = 21001
	SpendPolicyHold          Code = 21002
	SystemDependency         Code = 31001
	SystemConfiguration      Code = 31002
	SystemState              Code = 31003
	AgentCorrection          Code = 41001
	AgentDuplicate           Code = 41002
	AgentExhausted           Code = 41003
	SourceUnavailable        Code = 51001
	AntiBotBrowserApproved   Code = 51002
	BrowserAttemptsExhausted Code = 51003
	TaskDeadline             Code = 61001
	TaskCancelled            Code = 61002
	Unclassified             Code = 91001
)

type Status struct {
	Code        Code     `json:"code"`
	Name        string   `json:"name"`
	Owner       string   `json:"owner"`
	Strategy    string   `json:"strategy"`
	SystemLog   bool     `json:"system_log"`
	AlertPolicy string   `json:"alert_policy"`
	HTTPStatus  int      `json:"-"`
	Recovery    []string `json:"recovery,omitempty"`
}

func Of(code Code) Status {
	s := Status{Code: code, AlertPolicy: "none", HTTPStatus: 422}
	switch code {
	case InvalidRequest:
		s.Name, s.Owner, s.Strategy, s.HTTPStatus = "INVALID_REQUEST", "caller", "correct_input", 400
	case StateConflict:
		s.Name, s.Owner, s.Strategy, s.HTTPStatus = "STATE_CONFLICT", "host", "reload_saved_state", 409
	case BalanceUnavailable:
		s.Name, s.Owner, s.Strategy, s.HTTPStatus = "BUSINESS_BALANCE_UNAVAILABLE", "business", "stop_and_notify_on_deployment", 402
		s.SystemLog = true
		s.AlertPolicy = "immediate_on_deployment"
	case SpendPolicyHold:
		s.Name, s.Owner, s.Strategy = "BUSINESS_SPEND_POLICY_HOLD", "business", "stop_at_frozen_policy"
		s.SystemLog = true
	case SystemDependency:
		s.Name, s.Owner, s.Strategy, s.HTTPStatus = "SYSTEM_DEPENDENCY_FAILURE", "system", "retry_dependency_then_log", 503
		s.SystemLog = true
		s.AlertPolicy = "after_consecutive_system_failures_on_deployment"
	case SystemConfiguration:
		s.Name, s.Owner, s.Strategy, s.HTTPStatus = "SYSTEM_CONFIGURATION_ERROR", "system", "log_configuration_failure", 503
		s.SystemLog = true
		s.AlertPolicy = "after_consecutive_system_failures_on_deployment"
	case SystemState:
		s.Name, s.Owner, s.Strategy, s.HTTPStatus = "SYSTEM_STATE_ERROR", "system", "reload_and_reconcile", 500
		s.SystemLog = true
		s.AlertPolicy = "after_consecutive_system_failures_on_deployment"
	case AgentCorrection:
		s.Name, s.Owner, s.Strategy = "AGENT_CORRECTION_REQUIRED", "agent", "correct_and_retry"
	case AgentDuplicate:
		s.Name, s.Owner, s.Strategy, s.HTTPStatus = "AGENT_ACTION_ALREADY_CALLED", "agent", "use_saved_receipt", 409
	case AgentExhausted:
		s.Name, s.Owner, s.Strategy = "AGENT_CORRECTION_EXHAUSTED", "agent", "stop_with_agent_diagnostics"
	case SourceUnavailable:
		s.Name, s.Owner, s.Strategy = "SOURCE_UNAVAILABLE", "source", "respect_source_and_report"
	case AntiBotBrowserApproved:
		s.Name, s.Owner, s.Strategy, s.HTTPStatus = "ANTI_BOT_BROWSER_APPROVED", "source", "use_host_browser_grant", 409
	case BrowserAttemptsExhausted:
		s.Name, s.Owner, s.Strategy = "BROWSER_ATTEMPTS_EXHAUSTED", "source", "stop_with_browser_diagnostics"
	case TaskDeadline:
		s.Name, s.Owner, s.Strategy, s.HTTPStatus = "TASK_DEADLINE_REACHED", "policy", "deliver_saved_results", 408
	case TaskCancelled:
		s.Name, s.Owner, s.Strategy, s.HTTPStatus = "TASK_CANCELLED", "caller", "stop", 409
	default:
		s.Code, s.Name, s.Owner, s.Strategy, s.HTTPStatus = Unclassified, "UNCLASSIFIED_FAILURE", "unknown", "preserve_and_classify", 500
	}
	return s
}

type Carrier interface{ OperationalStatus() Status }
type Error struct {
	Status Status
	Cause  error
}

func (e *Error) Error() string             { return fmt.Sprintf("%s: %v", e.Status.Name, e.Cause) }
func (e *Error) Unwrap() error             { return e.Cause }
func (e *Error) OperationalStatus() Status { return e.Status }
func Wrap(code Code, cause error, steps ...string) error {
	s := Of(code)
	s.Recovery = steps
	return &Error{Status: s, Cause: cause}
}
