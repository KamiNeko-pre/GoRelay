package aiapp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"go-lua-crawler/internal/agentdata"
)

func TestProviderTerminationFeedbackUsesObservedFactsAcrossHTTP(t *testing.T) {
	raw, err := readPrivateReplayFixture(t, "provider-length-below-requested-limit.json")
	if err != nil {
		t.Fatal(err)
	}
	var original struct {
		Response ChatResponse `json:"response"`
	}
	if err := json.Unmarshal(raw, &original); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, limitStatus, argumentField string
		limit                            int
		response                         ChatResponse
	}{
		{"saved-real-response", "reported_below_requested_limit", "tool_calls/1/function.arguments", 384000, original.Response},
		{"different-malformed-argument", "reported_below_requested_limit", "tool_calls/0/function.arguments", 64000, ChatResponse{Message: Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "different", Type: "function", Function: FunctionCall{Name: "read_data", Arguments: `{"handle":}`}}}}, FinishReason: "length", Usage: &Usage{CompletionTokens: 27}}},
		{"reported-at-limit", "reported_at_or_above_requested_limit", "", 4096, ChatResponse{Message: Message{Role: "assistant"}, FinishReason: "length", Usage: &Usage{CompletionTokens: 4096}}},
		{"usage-unknown", "usage_unavailable", "", 384000, ChatResponse{Message: Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "complete-but-upstream-stopped", Type: "function", Function: FunctionCall{Name: "list_sources", Arguments: `{}`}}}}, FinishReason: "length"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GORELAY_AI_MAX_COMPLETION_TOKENS", strconv.Itoa(tc.limit))
			s, run, _ := storedHTMLReader(t, `<h2>already saved</h2>`)
			var requests []ChatRequest
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request ChatRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				requests = append(requests, request)
				response := tc.response
				if len(requests) > 1 {
					response = ChatResponse{Message: Message{Role: "assistant"}, FinishReason: "stop"}
				}
				json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": response.Message, "finish_reason": response.FinishReason}}, "usage": response.Usage})
			}))
			defer remote.Close()
			provider, err := NewModelProvider(ProviderConfig{BaseURL: remote.URL, APIKey: "fixture", Model: "fixture"})
			if err != nil {
				t.Fatal(err)
			}
			s.Model = provider
			for attempt := 0; attempt < 2; attempt++ {
				run, err = s.Data.Claim(context.Background(), run.ID, time.Minute)
				if err != nil {
					t.Fatal(err)
				}
				state, err := s.state(context.Background(), run)
				if err != nil {
					t.Fatal(err)
				}
				if err := s.think(context.Background(), state, attempt, &LoopRound{RoundID: agentdata.Token(), StartedAt: time.Now()}); err != nil {
					t.Fatal(err)
				}
			}
			if len(requests) != 2 || requests[0].MaxCompletionTokens != tc.limit {
				t.Fatal("request output policy changed", requests)
			}
			var failure FailureInfo
			for _, message := range requests[1].Messages {
				if message.Role != "tool" {
					continue
				}
				var snapshot struct {
					Failure FailureInfo `json:"model_failure"`
				}
				if json.Unmarshal([]byte(message.Content), &snapshot) == nil && snapshot.Failure.Kind != "" {
					failure = snapshot.Failure
				}
			}
			details, ok := failure.Details.(map[string]any)
			if !ok {
				t.Fatalf("typed failure details missing: %+v", failure)
			}
			if failure.Code != "MODEL_RESPONSE_TRUNCATED" || failure.ExecutionStatus != "not_started" || details["tools_dispatched"] != false {
				t.Fatalf("upstream incomplete proposal was accepted or lost: %+v", failure)
			}
			if details["termination_source"] != "provider_finish_reason" || details["output_limit_status"] != tc.limitStatus {
				t.Fatalf("provider signal was turned into an unproven budget cause: %+v", failure)
			}
			if failure.Recovery == nil || strings.Contains(failure.Recovery.Instruction, "exhausted max_completion_tokens") || !strings.Contains(failure.Recovery.Instruction, "JSON") {
				t.Fatal("missing honest, executable JSON correction", failure.Recovery)
			}
			if tc.argumentField != "" {
				encoded, _ := json.Marshal(details["argument_errors"])
				if !strings.Contains(string(encoded), tc.argumentField) || !strings.Contains(string(encoded), "byte_offset") {
					t.Fatal("observable argument syntax error was hidden by finish_reason", string(encoded))
				}
			}
			ops, _ := s.Data.Operations(context.Background(), run.ID)
			for _, op := range ops {
				if op.Kind != "model" && op.Kind != "inspect_source" {
					t.Fatal("rejected proposal executed a tool", op.Kind)
				}
			}
			if latestModelFailure(ops) != nil {
				t.Fatal("normal response inherited the earlier termination")
			}
		})
	}
}

func TestMalformedProposalReportsSpecificSchemaField(t *testing.T) {
	for _, tc := range []struct{ args, path, want string }{{`{"status":"completed","limitations": Catalog text, "gaps":[]}`, "limitations", "array of string"}, {`{"nested":{"count": many}}`, "nested/count", "integer"}} {
		request := ChatRequest{MaxCompletionTokens: 384000, Tools: []ToolDefinition{{Type: "function", Function: FunctionDefinition{Name: "finish_task", Parameters: json.RawMessage(`{"type":"object","properties":{"status":{"type":"string"},"limitations":{"type":"array","items":{"type":"string"}},"nested":{"type":"object","properties":{"count":{"type":"integer"}}}}}`)}}}}
		response := ChatResponse{FinishReason: "length", Usage: &Usage{CompletionTokens: 800}, Message: Message{ToolCalls: []ToolCall{{ID: "bad", Function: FunctionCall{Name: "finish_task", Arguments: tc.args}}}}}
		f := rejectedProposalFeedback(invalidProposal("finish_reason", "length"), response, request)
		details := f.Details.(map[string]any)
		diagnostic := details["argument_errors"].([]map[string]any)[0]
		if diagnostic["argument_path"] != tc.path || diagnostic["expected"] != tc.want {
			t.Fatalf("JSON failure did not name schema field %s: %+v", tc.path, diagnostic)
		}
	}
}
