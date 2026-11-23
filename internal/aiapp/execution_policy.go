package aiapp

import (
	"encoding/json"
	"go-lua-crawler/internal/acquisition"
	"os"
	"strings"
	"time"

	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/config"
	"go-lua-crawler/internal/engine"
)

// Forward the existing execution timeout without treating the whole task as one Lua call.
func programLimitsForRun(run agentdata.Run) engine.ProgramLimits {
	limits := engine.DefaultProgramLimits()
	if host := config.Get(); host != nil && host.Engine.LuaTimeout > 0 {
		limits.Timeout = time.Duration(host.Engine.LuaTimeout) * time.Second
	}
	var budget BudgetSnapshot
	if json.Unmarshal([]byte(run.BudgetJSON), &budget) == nil && budget.ProgramLimits != nil && budget.ProgramLimits.Timeout > 0 {
		limits.Timeout = budget.ProgramLimits.Timeout
	}
	if !run.Deadline.IsZero() {
		remaining := max(time.Nanosecond, time.Until(run.Deadline))
		if limits.Timeout <= 0 || remaining < limits.Timeout {
			limits.Timeout = remaining
		}
	}
	return limits
}

// Acquisition uses the current crawler protocol; report planning belongs to information/Eino.
func executionBudgetForRun(run agentdata.Run) (BudgetSnapshot, error) {
	var budget BudgetSnapshot
	if err := json.Unmarshal([]byte(run.BudgetJSON), &budget); err != nil {
		return budget, err
	}
	policy, err := configuredPromptPolicy()
	if err != nil {
		return budget, err
	}
	limits := programLimitsForRun(run)
	budget.PromptPolicy, budget.ProgramLimits = policy, &limits
	budget.LoopVersion = 5

	return budget, nil
}

// Source inspection uses the same host HTTP policy as the existing backend.
// No Agent task deadline or additional default is created here.
func InspectionPolicy(domains []string) acquisition.Policy {
	policy := acquisition.InspectionPolicy(domains)
	policy.MaxBodyBytes = acquisition.ConfiguredMaxResponseBytes()
	if host := config.Get(); host != nil {
		seconds := host.Engine.HTTPTimeoutDirect
		if strings.TrimSpace(os.Getenv("GORELAY_ACQUISITION_PROXY")) != "" {
			seconds = host.Engine.HTTPTimeoutProxy
		}
		if seconds > 0 {
			policy.Timeout = time.Duration(seconds) * time.Second
		}
	}
	return policy
}
