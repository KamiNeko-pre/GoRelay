package aiapp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// Provider-owned continuation text is opaque protocol data. It must survive
// the same SQL-JSON roundtrip as the assistant message, not become host facts.
func TestThinkingContinuationSurvivesProviderAndPersistence(t *testing.T) {
	for _, tool := range []bool{true, false} {
		t.Run(map[bool]string{true: "tool_decision", false: "assistant_continuation"}[tool], func(t *testing.T) {
			const continuation = "Observed structure is sufficient; next create the reusable action."
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if calls == 1 {
					message := map[string]any{"role": "assistant", "content": "", "reasoning_content": continuation}
					if tool {
						message["tool_calls"] = []any{map[string]any{"id": "inspect-1", "type": "function", "function": map[string]any{"name": "inspect_source", "arguments": "{}"}}}
					}
					json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": message, "finish_reason": "tool_calls"}}})
					return
				}
				var request map[string]json.RawMessage
				json.NewDecoder(r.Body).Decode(&request)
				var messages []map[string]any
				json.Unmarshal(request["messages"], &messages)
				if messages[1]["reasoning_content"] != continuation {
					t.Errorf("provider continuation lost before next round: %#v", messages[1])
				}
				json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "done"}, "finish_reason": "stop"}}})
			}))
			defer server.Close()
			provider, _ := NewModelProvider(ProviderConfig{BaseURL: server.URL, APIKey: "fixture", Model: "thinking-fixture"})
			response, err := provider.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "collect"}}})
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(response.Message)
			var restored Message
			if err = json.Unmarshal(encoded, &restored); err != nil {
				t.Fatal(err)
			}
			messages := []Message{{Role: "user", Content: "collect"}, restored}
			if tool {
				messages = append(messages, Message{Role: "tool", ToolCallID: "inspect-1", Content: `{"observed":true}`})
			}
			_, err = provider.Chat(context.Background(), ChatRequest{Messages: messages})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestNonThinkingMessageWireRemainsUnchanged(t *testing.T) {
	raw, _ := json.Marshal(Message{Role: "assistant", Content: "done"})
	var got map[string]any
	json.Unmarshal(raw, &got)
	if !reflect.DeepEqual(got, map[string]any{"role": "assistant", "content": "done"}) {
		t.Fatalf("non-thinking wire changed: %s", raw)
	}
}
