package aiapp

// ToolContractError is created where a tool knows its execution boundary and
// actual correction data. Recovery never classifies it by matching prose.
type ToolContractError struct {
	Failure FailureInfo
	Cause   error
}

func (e *ToolContractError) Error() string { return e.Cause.Error() }
func (e *ToolContractError) Unwrap() error { return e.Cause }

func toolContractFailure(cause error, code, execution string, details any, recovery ToolRecovery) error {
	return &ToolContractError{Cause: cause, Failure: FailureInfo{
		Kind: "tool", Category: "validation", Code: code,
		Message: cause.Error(), ExecutionStatus: execution, Details: details,
		Recovery: &recovery,
	}}
}
