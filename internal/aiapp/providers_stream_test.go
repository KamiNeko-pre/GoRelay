package aiapp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestModelProviderStreamingAccumulatesToolCallsAndTimings(t *testing.T) {
	var seenReq ChatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&seenReq); err != nil {
			t.Fatalf("decode req: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		chunks := []string{
			`{"model":"qodercn/dfmodel","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"run_lua","arguments":""}}]},"finish_reason":null}]}`,
			`{"model":"qodercn/dfmodel","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"run_lua","arguments":""}}]},"finish_reason":null}]}`,
			`{"model":"qodercn/dfmodel","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"mode\":"}}]},"finish_reason":null}]}`,
			`{"model":"qodercn/dfmodel","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"probe\"}"}}]},"finish_reason":null}]}`,
			`{"model":"qodercn/dfmodel","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`{"model":"qodercn/dfmodel","choices":[],"usage":{"prompt_tokens":120,"completion_tokens":15,"total_tokens":135,"prompt_tokens_details":{"cached_tokens":64}}}`,
		}
		time.Sleep(15 * time.Millisecond)
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			if flusher != nil {
				flusher.Flush()
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	p, err := NewModelProvider(ProviderConfig{
		BaseURL: srv.URL,
		APIKey:  "test-key",
		Model:   "qodercn/dfmodel",
		Stream:  true,
		Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := p.Chat(context.Background(), ChatRequest{
		Messages:        []Message{{Role: "user", Content: "hi"}},
		ReasoningEffort: "none",
		Thinking:        &ThinkingConfig{Type: "disabled"},
	})
	if err != nil {
		t.Fatalf("stream chat failed: %v", err)
	}
	if !seenReq.Stream || seenReq.StreamOptions == nil || !seenReq.StreamOptions.IncludeUsage || seenReq.ReasoningEffort != "none" || seenReq.Thinking == nil || seenReq.Thinking.Type != "disabled" {
		t.Fatalf("unexpected request payload sent to gateway: %+v", seenReq)
	}
	if len(resp.Message.ToolCalls) != 1 || resp.Message.ToolCalls[0].Function.Name != "run_lua" || resp.Message.ToolCalls[0].Function.Arguments != `{"mode":"probe"}` {
		t.Fatalf("unexpected assembled tool call (must not duplicate name across chunks): %+v", resp.Message.ToolCalls)
	}
	if resp.ToolArgsChars != len(`{"mode":"probe"}`) || resp.Usage == nil || resp.Usage.Cached() != 64 || resp.FinishReason != "tool_calls" {
		t.Fatalf("unexpected response metrics/usage: %+v", resp)
	}
	if resp.DurationMS < resp.TTFTMS || resp.DecodeMS < 0 {
		t.Fatalf("invalid timing breakdown: dur=%d ttft=%d decode=%d", resp.DurationMS, resp.TTFTMS, resp.DecodeMS)
	}
}
