package information

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"go-lua-crawler/internal/agentstatus"
)

// Configure does not start a model, browser, container or background service.
func (s *Service) Configure() error {
	timeout := os.Getenv("GORELAY_INFORMATION_REQUEST_TIMEOUT")
	if timeout == "" {
		timeout = os.Getenv("GORELAY_RUN_TIMEOUT")
	}
	if timeout == "" {
		timeout = "0s"
	}
	duration, err := time.ParseDuration(timeout)
	if err != nil || duration < 0 {
		return agentstatus.Wrap(agentstatus.SystemConfiguration, fmt.Errorf("information request timeout must be a nonnegative duration"))
	}
	maxFailed := 3
	if raw := os.Getenv("GORELAY_BROWSER_MAX_FAILED_PLANS"); raw != "" {
		maxFailed, err = strconv.Atoi(raw)
		if err != nil || maxFailed <= 0 {
			return agentstatus.Wrap(agentstatus.SystemConfiguration, fmt.Errorf("GORELAY_BROWSER_MAX_FAILED_PLANS must be positive"))
		}
	}
	actionTimeout := 30 * time.Second
	if raw := os.Getenv("GORELAY_BROWSER_ACTION_TIMEOUT"); raw != "" {
		actionTimeout, err = time.ParseDuration(raw)
		if err != nil || actionTimeout <= 0 {
			return agentstatus.Wrap(agentstatus.SystemConfiguration, fmt.Errorf("GORELAY_BROWSER_ACTION_TIMEOUT must be a positive duration"))
		}
	}
	s.RequestTimeout = duration
	s.BrowserPolicy = BrowserPolicy{MaxFailedPlans: maxFailed}
	s.ConfigureBrowser()
	if s.Browser != nil {
		s.Browser.ActionTimeout = actionTimeout
	}
	return nil
}
