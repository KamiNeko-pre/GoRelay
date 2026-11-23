package aiapp

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Frozen host policy. Input compaction, output capacity and elapsed time are
// different limits; none is a count of successful tool/model calls.
type PromptPolicy struct {
	WindowTokens    int    `json:"window_tokens"`
	CompactAtTokens int    `json:"compact_at_tokens"`
	CompactToTokens int    `json:"compact_to_tokens"`
	MaxOutputTokens int    `json:"max_output_tokens"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	Thinking        string `json:"thinking,omitempty"`
}

func configuredCompletionTokens() int {
	raw := strings.TrimSpace(os.Getenv("GORELAY_AI_MAX_COMPLETION_TOKENS"))
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

func configuredReasoningEffort() string {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv("GORELAY_AI_REASONING_EFFORT")))
	switch raw {
	case "none", "minimal", "low", "medium", "high":
		return raw
	default:
		return ""
	}
}

func configuredThinkingMode() string {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv("GORELAY_AI_THINKING")))
	switch raw {
	case "disabled", "enabled":
		return raw
	case "false", "off", "0", "none":
		return "disabled"
	case "true", "on", "1":
		return "enabled"
	default:
		return ""
	}
}

func configuredPromptPolicy() (*PromptPolicy, error) {
	p := &PromptPolicy{CompactAtTokens: 120000, MaxOutputTokens: configuredCompletionTokens(), ReasoningEffort: configuredReasoningEffort(), Thinking: configuredThinkingMode()}
	for _, setting := range []struct {
		name  string
		value *int
	}{
		{"GORELAY_AI_CONTEXT_WINDOW_TOKENS", &p.WindowTokens},
		{"GORELAY_AI_COMPACT_AT_TOKENS", &p.CompactAtTokens},
		{"GORELAY_AI_MAX_COMPLETION_TOKENS", &p.MaxOutputTokens},
	} {
		if raw := strings.TrimSpace(os.Getenv(setting.name)); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n <= 0 {
				return nil, fmt.Errorf("%s must be positive tokens", setting.name)
			}
			*setting.value = n
		}
	}
	p.CompactToTokens = p.CompactAtTokens
	if raw := strings.TrimSpace(os.Getenv("GORELAY_AI_COMPACT_TO_TOKENS")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("GORELAY_AI_COMPACT_TO_TOKENS must be positive tokens")
		}
		p.CompactToTokens = n
	}
	if p.CompactToTokens <= 0 || p.CompactToTokens > p.CompactAtTokens || p.WindowTokens > 0 && (p.MaxOutputTokens >= p.WindowTokens || p.CompactAtTokens > p.WindowTokens-p.MaxOutputTokens) {
		return nil, fmt.Errorf("prompt policy requires compact_to <= compact_at; an explicitly configured window must leave room for the configured output")
	}
	return p, nil
}

func ConfiguredPromptPolicy() (*PromptPolicy, error) {
	return configuredPromptPolicy()
}

