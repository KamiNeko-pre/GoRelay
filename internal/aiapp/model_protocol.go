package aiapp

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type ToolResult struct {
	ReplayOf     string           `json:"replay_of,omitempty"`
	RepairOf     string           `json:"repair_of,omitempty"`
	RepairTarget string           `json:"repair_target,omitempty"`
	Handle       string           `json:"handle,omitempty"`
	CallID       string           `json:"tool_call_id"`
	Name         string           `json:"name"`
	Data         json.RawMessage  `json:"data,omitempty"`
	Error        string           `json:"error,omitempty"`
	Failure      *FailureInfo     `json:"failure,omitempty"`
	HTTP         *HTTPObservation `json:"http,omitempty"`
}

type HTTPObservation struct {
	Known    bool  `json:"known"`
	Requests int   `json:"requests"`
	Bytes    int64 `json:"bytes"`
}

type FailureInfo struct {
	Kind            string         `json:"kind"`
	Message         string         `json:"message"`
	Provider        *ProviderError `json:"provider,omitempty"`
	RetryAt         *time.Time     `json:"retry_at,omitempty"`
	Retryable       bool           `json:"retryable,omitempty"`
	Details         map[string]any `json:"details,omitempty"`
	Category        string         `json:"category,omitempty"`
	Code            string         `json:"code,omitempty"`
	ExecutionStatus string         `json:"execution_status,omitempty"`
	Recovery        *ToolRecovery  `json:"recovery,omitempty"`
}

type RecoveryStep struct {
	Instruction string `json:"instruction,omitempty"`
	Action      string `json:"action,omitempty"`
}

type ToolRecovery struct {
	Strategy    string         `json:"strategy"`
	Instruction string         `json:"instruction"`
	Steps       []RecoveryStep `json:"steps,omitempty"`
}

func ModelFailureFeedback(err error, _ int) *FailureInfo {
	if err == nil {
		return nil
	}
	failure := &FailureInfo{
		Kind:    "provider",
		Message: err.Error(),
		Code:    "MODEL_CALL_FAILED",
	}
	if errors.Is(err, ErrInvalidProposal) {
		failure.Kind, failure.Code = "invalid_proposal", "MODEL_PROPOSAL_INVALID"
		return failure
	}
	var remote *ProviderError
	if errors.As(err, &remote) {
		if remote.QuotaExhausted {
			failure.Code = "MODEL_CHANNELS_EXHAUSTED"
		}
		if remote.Retryable {
			if seconds, err := time.ParseDuration(strings.TrimSpace(remote.RetryAfter) + "s"); err == nil && seconds > 0 {
				at := time.Now().Add(seconds)
				failure.RetryAt = &at
			} else if at, err := http.ParseTime(remote.RetryAfter); err == nil && at.After(time.Now()) {
				failure.RetryAt = &at
			}
			failure.Retryable = failure.RetryAt != nil
		}
	}
	return failure
}

// Call IDs pair transport messages; the durable business key still owns
// idempotency. Repair missing/colliding IDs without discarding valid actions.
func NormalizeModelToolCallIDs(message Message, operationID uint64, used map[string]bool) Message {
	if used == nil {
		used = map[string]bool{}
	}
	if message.Role == "" {
		message.Role = "assistant"
	}
	message.ToolCalls = append([]ToolCall(nil), message.ToolCalls...)
	for index := range message.ToolCalls {
		call := &message.ToolCalls[index]
		if call.Type == "" && call.Function.Name != "" {
			call.Type = "function"
		}
		if call.ID == "" || len(call.ID) > 128 || used[call.ID] {
			base := fmt.Sprintf("host_call_%d_%d", operationID, index)
			call.ID = base
			for suffix := 1; used[call.ID]; suffix++ {
				call.ID = fmt.Sprintf("%s_%d", base, suffix)
			}
		}
		used[call.ID] = true
	}
	return message
}

func DecisionSemanticText(message Message) string {
	parts := []string{}
	thought := message.ReasoningContent
	if thought == "" {
		thought = message.Content
	}
	if thought != "" {
		runes := []rune(thought)
		if len(runes) > 400 {
			thought = string(runes[:400])
		}
		parts = append(parts, "decision: "+thought)
	}
	for _, call := range message.ToolCalls {
		parts = append(parts, "action: "+call.Function.Name)
	}
	res := strings.Join(parts, " ; ")
	runes := []rune(res)
	if len(runes) > 800 {
		return string(runes[:800])
	}
	return res
}

func truncateRunes(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit])
}
