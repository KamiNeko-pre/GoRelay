package aiapp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"go-lua-crawler/internal/agentdata"
)

func promptPolicyState(t *testing.T) (*Service, LoopState) {
	t.Helper()
	s, run, _ := storedHTMLReader(t, `<h2>observed field</h2>`)
	state, err := s.state(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	return s, state
}

func TestNewCrawlerFreezesLargePromptPolicyAndUsesOnlyTaskDeadline(t *testing.T) {
	t.Setenv("GORELAY_EXPERIMENT_UNLIMITED_BUDGET", "0")
	t.Setenv("GORELAY_RUN_TIMEOUT", "6m")
	t.Setenv("GORELAY_AI_MAX_COMPLETION_TOKENS", "384000")
	s, state := promptPolicyState(t)
	var saved struct {
		Policy struct {
			Window  int `json:"window_tokens"`
			Trigger int `json:"compact_at_tokens"`
			Target  int `json:"compact_to_tokens"`
			Output  int `json:"max_output_tokens"`
		} `json:"prompt_policy"`
	}
	json.Unmarshal([]byte(state.Run.BudgetJSON), &saved)
	if saved.Policy.Window != 1000000 || saved.Policy.Trigger != 500000 || saved.Policy.Target >= saved.Policy.Trigger || saved.Policy.Output != 384000 {
		t.Fatal("crawler still uses a KiB window or small output cap", saved)
	}
	if state.Budget.Unlimited || !state.Budget.WorkDeadline.IsZero() || time.Until(state.Run.Deadline) > 6*time.Minute || time.Until(state.Run.Deadline) < 5*time.Minute+50*time.Second {
		t.Fatal("healthy work is shortened by a call ceiling or reserved minute", state.Budget)
	}
	b, err := NewRunBudget(state.Budget)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Reserve(CallCost{ModelCalls: 100, ToolCalls: 500}, time.Now()); err != nil {
		t.Fatal("call counts stop an active timeboxed task", err)
	}
	t.Setenv("GORELAY_AI_MAX_COMPLETION_TOKENS", "4096")
	model := &feedbackChat{}
	s.Model = model
	state.Run, err = s.Data.Claim(context.Background(), state.Run.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.think(context.Background(), state, 0, &LoopRound{RoundID: agentdata.Token(), StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if model.last.MaxCompletionTokens != 384000 {
		t.Fatal("output policy changed after task creation", model.last.MaxCompletionTokens)
	}
	b.Cancel()
	if b.Reserve(CallCost{ModelCalls: 1}, time.Now()) == nil {
		t.Fatal("timebox policy bypassed cancellation")
	}
}

func TestCrawlerDoesNotCompressSmallOrHundredKTokenHistory(t *testing.T) {
	for _, text := range []string{strings.Repeat("normal observed row ", 9000), strings.Repeat("已观察字段与分页信息。", 18000)} {
		_, state := promptPolicyState(t)
		messages := []Message{{Role: "assistant", Content: text}, {Role: "assistant", Content: "latest decision"}}
		out, profile, err := compactContextMeasured(state, messages)
		if err != nil {
			t.Fatal("large-window model rejected a small history", err)
		}
		if profile.Triggered || !strings.Contains(string(mustJSON(out)), text) {
			t.Fatal("compressed far below 500k tokens", profile)
		}
	}
}

func TestCrawlerCompactsNear500KTokensUsingReportedUsage(t *testing.T) {
	_, state := promptPolicyState(t)
	calibration, _ := json.Marshal(ChatResponse{Message: Message{Role: "assistant", Content: "completed"}, Usage: &Usage{PromptTokens: 25000}, ContextProfile: &PromptContextProfile{RequestBytes: 100000}})
	state.Operations = append(state.Operations, agentdata.Operation{ID: 2, RunID: state.Run.ID, Kind: "model", Stage: "succeeded", ResultJSON: string(calibration)})
	messages := []Message{}
	for i := 0; i < 12; i++ {
		messages = append(messages, Message{Role: "assistant", Content: strings.Repeat("observed event ", 14000)})
	}
	call := ToolCall{ID: "pending-corrected-program", Type: "function", Function: FunctionCall{Name: "validate_program", Arguments: `{"code":"return true,{items={}}"}`}}
	messages = append(messages, Message{Role: "assistant", ToolCalls: []ToolCall{call}})
	out, profile, err := compactContextMeasured(state, messages)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(profile)
	var metric struct {
		Before  int    `json:"before_tokens"`
		After   int    `json:"after_tokens"`
		Trigger int    `json:"trigger_tokens"`
		Basis   string `json:"token_estimation_basis"`
	}
	json.Unmarshal(raw, &metric)
	if !profile.Triggered || metric.Before < 500000 || metric.After > 430000 || metric.Trigger != 500000 || !strings.Contains(metric.Basis, "provider_usage") {
		t.Fatal("compression is not driven by the requested token threshold", string(raw))
	}
	if !strings.Contains(string(mustJSON(out)), call.ID) || !strings.Contains(string(mustJSON(out)), "return true") || !strings.Contains(string(mustJSON(out)), state.Run.Goal) {
		t.Fatal("compression lost the current task/program")
	}
}
