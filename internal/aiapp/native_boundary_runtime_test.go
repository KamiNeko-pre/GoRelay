package aiapp

import (
	"context"
	"encoding/json"
	"go-lua-crawler/internal/testdb"
	"testing"
)

func TestExecutedNativeActionsStopInferenceWithoutAnotherPaidAttempt(t *testing.T) {
	for _, kind := range []string{"mcp_tool_call", "command_execution"} {
		t.Run(kind, func(t *testing.T) {
			s := NewService(testdb.Open(t))
			if err := s.Migrate(); err != nil {
				t.Fatal(err)
			}
			p := &codexRefusalChat{raw: `{"type":"item.completed","item":{"type":"` + kind + `"}}
{"type":"item.completed","item":{"type":"agent_message","text":"{\"content\":\"ignored\",\"tool_calls\":[]}"}}
{"type":"turn.completed","usage":{"input_tokens":7,"output_tokens":3}}`}
			s.Model = p
			ctx := context.Background()
			r, err := s.Create(ctx, CreateRunRequest{RequestKey: "native-boundary", Goal: "facts"})
			if err != nil {
				t.Fatal(err)
			}
			_ = s.Advance(ctx, r.ID)
			current, err := s.Data.Run(ctx, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.Status != "failed" || p.calls != 1 {
				t.Fatalf("native contract violation retried: status=%s calls=%d", current.Status, p.calls)
			}
			ops, err := s.Data.Operations(ctx, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(ops) != 1 || ops[0].Stage != "failed" {
				t.Fatal("observed violation was classified as an unknown response", ops)
			}
			var response ChatResponse
			if err = json.Unmarshal([]byte(ops[0].ResultJSON), &response); err != nil {
				t.Fatal(err)
			}
			if response.Failure == nil || response.Failure.Kind != "inference_boundary" || response.Failure.RetryAt != nil || response.Usage == nil || response.Usage.TotalTokens != 10 {
				t.Fatal("boundary/usage facts lost", response)
			}
			recovered := NewService(s.Data.DB)
			recovered.Model = p
			for n := 0; n < 2; n++ {
				_ = recovered.Advance(ctx, r.ID)
			}
			if p.calls != 1 {
				t.Fatal("restart repeated violated inference")
			}
			// A crash after the response receipt must preserve the typed stop,
			// not reclassify the violation as an unknown/network retry.
			second, err := s.Create(ctx, CreateRunRequest{RequestKey: "boundary-crash", Goal: "facts"})
			if err != nil {
				t.Fatal(err)
			}
			crashRunTransition(t, s.Data.DB)
			_ = s.Advance(ctx, second.ID)
			if p.calls != 2 {
				t.Fatal("crash fixture did not make exactly one call")
			}
			s.Data.DB.Callback().Update().Remove("fixture:crash-run-transition")
			releaseCrashedLease(t, s, second.ID)
			recovered = NewService(s.Data.DB)
			recovered.Model = p
			_ = recovered.Advance(ctx, second.ID)
			final, err := s.Data.Run(ctx, second.ID)
			if err != nil || final.Status != "failed" || p.calls != 2 {
				t.Fatal("committed boundary receipt caused a paid retry after crash", final.Status, p.calls, err)
			}
		})
	}
}
