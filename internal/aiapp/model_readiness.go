package aiapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"
)

var ErrModelNotReady = errors.New("model backend not ready")

type ModelReadiness struct {
	Ready         bool       `json:"ready"`
	CheckedAt     *time.Time `json:"checked_at,omitempty"`
	Error         string     `json:"error,omitempty"`
	Method        string     `json:"method,omitempty"`
	Usage         *Usage     `json:"usage,omitempty"` // Readiness uses inference; missing usage is unknown.
	QuotaVerified bool       `json:"quota_verified"`  // No transport proves future quota.
}
type modelReadinessState struct {
	mu    sync.Mutex
	value ModelReadiness
	until time.Time
}
type modelPreflight interface {
	Preflight(context.Context) (ModelReadiness, error)
}

func (s *Service) ModelReadiness() ModelReadiness {
	if s.readiness == nil {
		return ModelReadiness{Ready: s.Model != nil, Method: "injected_test_transport"}
	}
	s.readiness.mu.Lock()
	defer s.readiness.mu.Unlock()
	return s.readiness.value
}

// Configuration is startup-owned; parallel submissions share one cached probe.
// Existing request-key replays need no new probe or inference.
func (s *Service) PreflightModel(ctx context.Context) error {
	if s.readiness == nil {
		return nil
	} // Explicitly injected fixtures do not invoke real transports.
	state := s.readiness
	state.mu.Lock()
	defer state.mu.Unlock()
	if time.Now().Before(state.until) {
		if state.value.Ready {
			return nil
		}
		return fmt.Errorf("%w: %s", ErrModelNotReady, state.value.Error)
	}
	now := time.Now()
	var result ModelReadiness
	var err error
	p, ok := s.Model.(modelPreflight)
	if !ok {
		err = fmt.Errorf("chat adapter is not configured for readiness")
	} else {
		result, err = p.Preflight(ctx)
	}
	result.CheckedAt = &now
	state.until = time.Now().Add(30 * time.Second)
	if err != nil {
		result.Ready = false
		result.Error = err.Error()
	} else if !result.Ready {
		err = fmt.Errorf("backend did not confirm readiness")
		result.Error = err.Error()
	}
	// Both transports probe inference. Share successful results across submissions.
	if err == nil {
		state.until = time.Now().Add(10 * time.Minute)
	}
	state.value = result
	if err != nil {
		return fmt.Errorf("%w: %v", ErrModelNotReady, err)
	}
	return nil
}

func (p *ModelProvider) Preflight(ctx context.Context) (ModelReadiness, error) {
	// Reuse the actual inference adapter, including its configured timeout and
	// response parser. Catalog access says nothing about inference permission.
	//
	// Exercise the native tool protocol used by the loop. A text-only check both
	// rejected valid tool-only responses and admitted backends without tool support.
	// This function is a protocol probe only: no returned tool is ever executed.
	response, err := p.Chat(ctx, ChatRequest{
		Messages: []Message{
			{Role: "system", Content: "Check the Agent tool protocol by calling gorelay_readiness exactly once with ready=true. Do not perform any external actions."},
			{Role: "user", Content: "Check readiness using the provided tool."},
		},
		Tools: []ToolDefinition{{Type: "function", Function: FunctionDefinition{
			Name: "gorelay_readiness", Description: "Confirm the native tool-call protocol. Has no side effects.",
			Parameters: json.RawMessage(`{"type":"object","properties":{"ready":{"type":"boolean","enum":[true]}},"required":["ready"],"additionalProperties":false}`),
		}}},
		MaxCompletionTokens: 512,
	})
	result := ModelReadiness{Method: "http_inference_probe", Usage: response.Usage}
	if err != nil {
		return result, err
	}
	if response.Message.Role != "assistant" || len(response.Message.ToolCalls) != 1 {
		return result, fmt.Errorf("HTTP readiness inference response is invalid")
	}
	call := response.Message.ToolCalls[0]
	var args struct {
		Ready bool `json:"ready"`
	}
	if call.ID == "" || call.Type != "function" || call.Function.Name != "gorelay_readiness" || decodeArgs(call.Function.Arguments, &args) != nil || !args.Ready {
		return result, fmt.Errorf("HTTP readiness tool protocol is invalid")
	}
	result.Ready = true
	return result, nil
}

func (p *CodexProvider) Preflight(ctx context.Context) (ModelReadiness, error) {
	result := ModelReadiness{Method: "cli_login_and_inference_probe"}
	if err := prepareInferenceHome(p.ParentHome, p.InferenceHome); err != nil {
		return result, err
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	cmd := exec.CommandContext(probeCtx, p.Binary, "login", "status")
	cmd.Env = inferenceHomeEnvironment(os.Environ(), p.InferenceHome)
	err := cmd.Run()
	cancel()
	if err != nil {
		return result, fmt.Errorf("Codex CLI login is unavailable: %w", err)
	}
	// Local login presence alone does not establish server authorization. This
	// single normal transport request also detects revoked login/quota/JSONL drift.
	response, err := p.Chat(ctx, ChatRequest{Messages: []Message{{Role: "user", Content: "Return content ready and an empty tool_calls array."}}, MaxCompletionTokens: 512})
	result.Usage = response.Usage
	if err != nil {
		return result, err
	}
	if response.Message.Content == "" || len(response.Message.ToolCalls) != 0 {
		return result, fmt.Errorf("Codex CLI readiness response is invalid")
	}
	result.Ready = true
	return result, nil
}
