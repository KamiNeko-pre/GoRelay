package aiapp

import (
	"os"
	"strconv"
	"strings"
)

// configuredCompletionTokens bounds one model reply. A hard multi-step task needs
// room for a full program plus its explanation in a single response, so the
// ceiling is frozen with the context policy. Invalid explicit settings fail
// task creation; the default matches the configured DeepSeek V4.1 capacity.
func configuredCompletionTokens() int {
	raw := strings.TrimSpace(os.Getenv("GORELAY_AI_MAX_COMPLETION_TOKENS"))
	if raw == "" {
		return 384000
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 384000
	}
	return n
}

// configuredReasoningEffort forwards a provider thinking budget when set. An
// empty value sends nothing, so providers that do not support the field are
// unaffected. Higher effort spends more search on the same prompt, which matters
// for a model that must choose a selector from the converted page rather than
// guess one.
func configuredReasoningEffort() string {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv("GORELAY_AI_REASONING_EFFORT")))
	switch raw {
	case "low", "medium", "high":
		return raw
	default:
		return ""
	}
}
