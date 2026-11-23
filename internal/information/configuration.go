package information

import (
	"fmt"
	"os"
	"strings"
	"time"

	"go-lua-crawler/internal/agentstatus"
	"go-lua-crawler/internal/aiapp"
)

func firstNonEmptyEnv(keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

// Configure does not start a model, browser, container or background service.
func (s *Service) Configure() error {
	if model := strings.TrimSpace(os.Getenv("GORELAY_AI_AUDIT_MODEL")); model != "" {
		provider, err := aiapp.NewModelProvider(aiapp.ProviderConfig{
			BaseURL: firstNonEmptyEnv("GORELAY_AI_AUDIT_BASE_URL", "GORELAY_AI_BASE_URL"),
			APIKey:  firstNonEmptyEnv("GORELAY_AI_AUDIT_API_KEY", "GORELAY_AI_API_KEY"),
			Model:   model,
			Stream:  aiapp.ConfiguredStreamMode(),
		})
		if err != nil {
			return fmt.Errorf("delivery audit provider: %w", err)
		}
		s.DeliveryAuditModel = provider
	}
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
	var actionTimeout time.Duration
	if raw := os.Getenv("GORELAY_BROWSER_ACTION_TIMEOUT"); raw != "" {
		actionTimeout, err = time.ParseDuration(raw)
		if err != nil || actionTimeout <= 0 {
			return agentstatus.Wrap(agentstatus.SystemConfiguration, fmt.Errorf("GORELAY_BROWSER_ACTION_TIMEOUT must be a positive duration"))
		}
	}
	s.RequestTimeout = duration
	s.ConfigureBrowser()
	if s.Browser != nil {
		s.Browser.ActionTimeout = actionTimeout
	}
	return nil
}
