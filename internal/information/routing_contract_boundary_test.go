package information

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"go-lua-crawler/internal/aiapp"
)

// This is a boundary replay, not an Agent benchmark. It supplies only a tool
// proposal; the real parser, workflow, error contract and SQL receipts run.
type routingReply struct {
	args map[string]any
	last aiapp.ChatRequest
}

func (m *routingReply) Chat(_ context.Context, in aiapp.ChatRequest) (aiapp.ChatResponse, error) {
	m.last = in
	return aiapp.ChatResponse{Message: aiapp.Message{Role: "assistant", ToolCalls: []aiapp.ToolCall{{ID: "transport-only", Type: "function", Function: aiapp.FunctionCall{Name: in.Tools[0].Function.Name, Arguments: string(mustMarshal(m.args))}}}}, FinishReason: "tool_calls"}, nil
}

func TestRouteFieldCorrectionStaysInItsStageAndRestoresExactDiagnosis(t *testing.T) {
	for _, stage := range []string{"known_api", "select_api_candidate"} {
		t.Run(stage, func(t *testing.T) {
			s, _ := retrievalService(t)
			ctx := context.Background()
			spec := aiapp.TaskSpec{Goal: "collect observed names", SchemaVersion: "v1", RequiredFields: []aiapp.FieldRequirement{{Name: "name", Type: "string"}}}
			r, err := s.Create(ctx, Input{RequestKey: stage, Goal: spec.Goal, Spec: &spec, SourceScope: []string{"source.example"}})
			if err != nil {
				t.Fatal(err)
			}
			state := routeContext{Lookup: &PreflightResult{Experiment: &ExperimentControls{InitialAssets: []AssetRef{{ID: "internal-initial-asset", Version: "internal-version"}}}}}
			if err := s.DB.Model(&Request{}).Where("id = ?", r.ID).Updates(map[string]any{"stage": stage, "context_json": string(mustMarshal(state))}).Error; err != nil {
				t.Fatal(err)
			}
			m := &routingReply{args: map[string]any{"action": "inspect_api", "reason": "candidate endpoint", "api": map[string]any{"url": "https://source.example/api"}}}
			s.Model = m
			err = s.Advance(ctx, r.ID)
			var failure *ActionError
			if !errors.As(err, &failure) || failure.Code != "ROUTING_ARGUMENTS_INVALID" || !strings.Contains(failure.Message, "url") || len(failure.Steps) < 2 {
				t.Fatalf("missing URL diagnosis was lost: %v", err)
			}
			current, err := s.Request(ctx, r.ID)
			if err != nil || current.Stage != stage {
				t.Fatalf("parameter error changed capability route: %s %v", current.Stage, err)
			}
			if strings.Contains(m.last.Messages[1].Content, "internal-initial-asset") {
				t.Fatal("experiment bookkeeping was exposed as callable assets")
			}
			var schema struct{ Properties map[string]json.RawMessage }
			json.Unmarshal(m.last.Tools[0].Function.Parameters, &schema)
			if schema.Properties["url"] == nil || schema.Properties["api"] != nil || schema.Properties["asset_id"] != nil {
				t.Fatal("candidate stage advertises unrelated execution arguments")
			}
			m.args["reason"] = "same intent with a renamed explanation"
			if err := s.Advance(ctx, r.ID); !errors.As(err, &failure) || failure.Code != "ACTION_ALREADY_CALLED" || failure.PriorStepID == 0 {
				t.Fatalf("same bad arguments bypassed idempotency: %v", err)
			}
			current, _ = s.Request(ctx, r.ID)
			if !strings.Contains(current.FeedbackJSON, "prior_step") || !strings.Contains(current.FeedbackJSON, "ROUTING_ARGUMENTS_INVALID") {
				t.Fatal("duplicate hid the original exact parameter failure")
			}
			m.args = map[string]any{"action": "inspect_api", "reason": "use corrected field", "url": "https://source.example/api"}
			if err := s.Advance(ctx, r.ID); err != nil {
				t.Fatalf("corrected business arguments rejected: %v", err)
			}
			current, _ = s.Request(ctx, r.ID)
			if current.Stage != "inspect_api" && current.Stage != "inspect_searched_api" {
				t.Fatalf("valid candidate not inspected: %s", current.Stage)
			}
		})
	}
}
