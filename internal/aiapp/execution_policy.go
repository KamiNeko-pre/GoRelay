package aiapp

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/engine"
)

// ConfiguredProgramLimits is the single host resource policy for acquisition,
// probes and the private runner. These are execution resources, not model
// context budgets. New runs persist this policy before any model proposal.
func ConfiguredProgramLimits() (engine.ProgramLimits, error) {
	p := engine.DefaultProgramLimits()
	b, err := configuredBudgetLimits()
	if err != nil {
		return p, err
	}
	p.MaxRequests = int(b.HTTPRequests)
	taskTimeout, err := configuredRunTimeout()
	if err != nil {
		return p, err
	}
	// No task timebox does not disable the positive per-program resource
	// policy required by the VM and runner. Keep the existing engine default.
	if taskTimeout > 0 {
		p.Timeout = taskTimeout
	}
	for _, setting := range []struct {
		name   string
		target *int
	}{
		{"GORELAY_PROGRAM_HTTP_REQUESTS", &p.MaxRequests},
		{"GORELAY_PROGRAM_MAX_ITEMS", &p.MaxItems},
		{"GORELAY_PROGRAM_MAX_OUTPUT_BYTES", &p.MaxOutputBytes},
	} {
		if raw := strings.TrimSpace(os.Getenv(setting.name)); raw != "" {
			n, e := strconv.Atoi(raw)
			if e != nil || n <= 0 {
				return p, fmt.Errorf("%s must be a positive execution resource budget", setting.name)
			}
			*setting.target = n
		}
	}
	if raw := strings.TrimSpace(os.Getenv("GORELAY_PROGRAM_MAX_BYTES")); raw != "" {
		n, e := strconv.ParseInt(raw, 10, 64)
		if e != nil || n <= 0 {
			return p, fmt.Errorf("GORELAY_PROGRAM_MAX_BYTES must be a positive execution resource budget")
		}
		p.MaxBytes = n
	}
	return p, nil
}

func programLimitsForRun(run agentdata.Run) engine.ProgramLimits {
	var budget BudgetSnapshot
	json.Unmarshal([]byte(run.BudgetJSON), &budget)
	if budget.ProgramLimits != nil {
		limits := *budget.ProgramLimits
		if !run.Deadline.IsZero() {
			left := time.Until(run.Deadline)
			if left < limits.Timeout {
				limits.Timeout = max(time.Nanosecond, left)
			}
		}
		return limits
	}
	// Old runs retain their frozen execution protocol.
	limits := engine.DefaultProgramLimits()
	limits.MaxRequests, limits.MaxBytes = 8, 1<<20
	return limits
}

// JSON can escape every source/output byte. Derive the transport envelope
// from the actual admitted payload instead of silently clipping it at 10 MiB.
func RunnerResponseByteLimit(limits engine.ProgramLimits) int64 {
	return 6*limits.MaxBytes + 6*int64(limits.MaxOutputBytes) + (1 << 20)
}
