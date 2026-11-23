package programrunner

import (
	"fmt"
	"go-lua-crawler/internal/engine"
	"os"
	"time"
)

// Legacy serialized-policy compatibility; execution has no request-count quota.
const DefaultHTTPRequests = 0

// ConfiguredLimits keeps the existing runner contract without inventing task quotas.
func ConfiguredLimits() (engine.ProgramLimits, error) {
	return engine.DefaultProgramLimits(), nil
}

// Compatibility API: complete replies use the transport and physical sandbox;
// result size is not a business-task quota.
func ResponseByteLimit(_ engine.ProgramLimits) int64 { return 0 }

func ConfiguredRunTimeout() (time.Duration, error) {
	raw := os.Getenv("GORELAY_RUN_TIMEOUT")
	if raw == "" {
		raw = "0s"
	}
	timeout, err := time.ParseDuration(raw)
	if err != nil || timeout < 0 {
		return 0, fmt.Errorf("GORELAY_RUN_TIMEOUT must be a nonnegative duration; zero means no task timebox")
	}
	return timeout, nil
}
