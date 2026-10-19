package aiapp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"go-lua-crawler/internal/agentdata"
)

// Replay the saved native failure state through the real context builder and
// provider transport. The local HTTP endpoint checks visibility only; it does
// not pretend that replay proves a real model repaired the program.
func TestCapturedFailuresKeepRepairMaterialsOnActualModelHTTP(t *testing.T) {
	paths := strings.Split(os.Getenv("GORELAY_TEST_CRAWLER_TRACES"), ";")
	if len(paths) == 1 && paths[0] == "" {
		t.Skip("explicit captured native traces required")
	}
	for n, path := range paths {
		t.Run(fmt.Sprint(n+1), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var saved struct {
				Run        agentdata.Run         `json:"run"`
				Operations []agentdata.Operation `json:"operations"`
				Result     TaskResult            `json:"result"`
			}
			if err = json.Unmarshal(raw, &saved); err != nil {
				t.Fatal(err)
			}
			s := LoopState{Run: saved.Run, Operations: saved.Operations, Result: &saved.Result}
			json.Unmarshal([]byte(s.Run.BudgetJSON), &s.Budget)
			json.Unmarshal([]byte(s.Run.SpecJSON), &s.Spec)
			json.Unmarshal([]byte(s.Run.PlanJSON), &s.Messages)
			s.HostProgress.Recovery = DeliveryRecovery{Phase: "correcting", Reason: "replay the saved failure with its complete editable object"}
			program := pendingProgramCorrections(s)
			if len(program) != 1 {
				t.Fatal("no pending working program", len(program))
			}
			var original map[string]any
			json.Unmarshal(program[0].PreviousArguments, &original)
			_, pressure := pressureHistory(t)
			for i, m := range pressure {
				if m.Role == "tool" {
					var r ToolResult
					json.Unmarshal([]byte(m.Content), &r)
					s.Operations = append(s.Operations, agentdata.Operation{ID: uint64(1000 + i), RunID: s.Run.ID, Key: "tool:" + r.CallID, Kind: r.Name, Stage: "succeeded", ResultJSON: m.Content})
				}
			}
			history := append(append([]Message{}, s.Messages...), pressure...)
			if s.Budget.PromptPolicy != nil {
				// Captured runs retain their actual token policy (now 500k).
				// The old 40KB legacy fixture no longer constitutes pressure.
				// Grow unique tool pairs to that policy, never lower production
				// thresholds or misreport an untriggered compaction as code loss.
				for batch := 0; ; batch++ {
					tokens, _ := estimateContextTokens(s, history)
					if tokens >= s.Budget.PromptPolicy.CompactAtTokens {
						break
					}
					for i := 1; i < len(pressure); i += 2 {
						id := fmt.Sprintf("captured-pressure-%d-%d", batch, i)
						var receipt ToolResult
						json.Unmarshal([]byte(pressure[i].Content), &receipt)
						receipt.CallID, receipt.Handle = id, id
						call := pressure[i-1].ToolCalls[0]
						call.ID = id
						raw, _ := json.Marshal(receipt)
						history = append(history, Message{Role: "assistant", ToolCalls: []ToolCall{call}}, Message{Role: "tool", ToolCallID: id, Content: string(raw)})
						s.Operations = append(s.Operations, agentdata.Operation{ID: uint64(100000 + batch*len(pressure) + i), RunID: s.Run.ID, Key: "tool:" + id, Kind: receipt.Name, Stage: "succeeded", ResultJSON: string(raw)})
					}
				}
			}
			messages, profile, err := compactContextMeasured(s, history)
			if err != nil {
				t.Fatal(err)
			}
			facts := compactionFacts(s, messages)
			workingInput := canonicalToolInput(FunctionCall{Name: "validate_program", Arguments: string(program[0].PreviousArguments)})
			if !profile.Triggered || !facts["program_arguments"][workingInput] {
				t.Fatal("current repair arguments were lost", profile)
			}
			for _, lost := range profile.MissingCriticalFacts {
				if strings.HasPrefix(lost, "program_arguments:") || strings.HasPrefix(lost, "program_code:") {
					t.Fatal("current repair object was evicted", lost)
				}
			}
			calls := 0
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req ChatRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				calls++
				observed, err := visibleWorkingProgram(req)
				if err != nil {
					t.Error(err)
				} else if agentdata.Hash(observed) != agentdata.Hash(original) {
					t.Error("provider received changed or incomplete editable arguments")
				}
				hasDiagnostic, hasMethod := false, false
				for _, m := range req.Messages {
					if m.Role == "tool" {
						hasDiagnostic = hasDiagnostic || strings.Contains(m.Content, `"unresolved_diagnostics"`) || strings.Contains(m.Content, `"unresolved_tool_failures"`)
						hasMethod = hasMethod || strings.Contains(m.Content, `"methodology"`)
					}
				}
				if !hasDiagnostic || !hasMethod {
					t.Error("editable object reached HTTP without diagnosis and method", hasDiagnostic, hasMethod)
				}
				auditSave(t, fmt.Sprintf("native-failure-%d-replayed-http", n+1), map[string]any{"request": req, "compaction": profile, "provider": "local transport assertion; no native inference"})
				json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "transport-check-done", Type: "function", Function: FunctionCall{Name: "finish_task", Arguments: `{"status":"failed","reason":"transport replay only; no business completion claim"}`}}}}, "finish_reason": "tool_calls"}}})
			}))
			defer endpoint.Close()
			p, err := NewModelProvider(ProviderConfig{BaseURL: endpoint.URL, APIKey: "test-only", Model: "captured-transport-check"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = p.Chat(context.Background(), ChatRequest{Messages: messages, Tools: runToolDefinitions(s.Run)}); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatal("unexpected transport retries", calls)
			}
		})
	}
}
