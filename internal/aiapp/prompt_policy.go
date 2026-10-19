package aiapp

import (
	"encoding/json"
	"fmt"
	"math"
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
}

func configuredPromptPolicy() (*PromptPolicy, error) {
	p := &PromptPolicy{WindowTokens: 1000000, CompactAtTokens: 500000, MaxOutputTokens: configuredCompletionTokens(), ReasoningEffort: configuredReasoningEffort()}
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
	p.CompactToTokens = p.CompactAtTokens * 4 / 5
	if raw := strings.TrimSpace(os.Getenv("GORELAY_AI_COMPACT_TO_TOKENS")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("GORELAY_AI_COMPACT_TO_TOKENS must be positive tokens")
		}
		p.CompactToTokens = n
	}
	if p.CompactToTokens <= 0 || p.CompactToTokens >= p.CompactAtTokens || p.MaxOutputTokens >= p.WindowTokens || p.CompactAtTokens > p.WindowTokens-p.MaxOutputTokens {
		return nil, fmt.Errorf("prompt policy must leave complete output capacity: compact_to < compact_at and compact_at + max_output <= context_window (all in tokens)")
	}
	return p, nil
}

func completionTokens(state LoopState) int {
	if state.Budget.PromptPolicy != nil {
		return state.Budget.PromptPolicy.MaxOutputTokens
	}
	return configuredCompletionTokens()
}

func reasoningEffort(state LoopState) string {
	if state.Budget.PromptPolicy != nil {
		return state.Budget.PromptPolicy.ReasoningEffort
	}
	return configuredReasoningEffort()
}

func promptPolicyForRun(raw string) *PromptPolicy {
	var b struct {
		Policy *PromptPolicy `json:"prompt_policy"`
	}
	json.Unmarshal([]byte(raw), &b)
	return b.Policy
}

// Providers report actual prompt tokens after each call. Calibrate against the
// exact serialized request profile; future requests remain estimates, labelled
// as such, rather than falsely presenting byte lengths as tokenizer counts.
func estimateContextTokens(state LoopState, messages []Message) (int, string) {
	bytes := contextRequestBytes(state, messages)
	for i := len(state.Operations) - 1; i >= 0; i-- {
		op := state.Operations[i]
		if op.Kind != "model" || op.RunID != "" && op.RunID != state.Run.ID {
			continue
		}
		var response ChatResponse
		if json.Unmarshal([]byte(op.ResultJSON), &response) != nil || response.Usage == nil || response.Usage.PromptTokens <= 0 || response.ContextProfile == nil || response.ContextProfile.RequestBytes <= 0 {
			continue
		}
		ratio := float64(response.Usage.PromptTokens) / float64(response.ContextProfile.RequestBytes)
		return int(math.Ceil(float64(bytes) * ratio)), "provider_usage_calibrated_estimate"
	}
	raw, _ := json.Marshal(ChatRequest{Messages: messages, Tools: runToolDefinitionsForState(state), MaxCompletionTokens: completionTokens(state)})
	ascii, other := 0, 0
	for _, r := range string(raw) {
		if r < 128 {
			ascii++
		} else {
			other++
		}
	}
	return (ascii+3)/4 + other + 256, "unicode_text_estimate_before_provider_usage"
}
