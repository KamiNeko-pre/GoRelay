package aiapp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"go-lua-crawler/internal/agentdata"
)

func TestCrawlerTableRowsKeepBoundedFieldPairs(t *testing.T) {
	for _, pair := range [][2]string{{"项目编号", "CG-52"}, {"License", "CC-BY-4.0"}} {
		s, run, _ := storedHTMLReader(t, `<table><tr><th>`+pair[0]+`</th><td>`+pair[1]+`</td></tr><tr><td>`+strings.Repeat("private long article ", 100)+`</td></tr></table>`)
		out, err := s.readData(context.Background(), run, ToolCall{ID: "read-rows", Function: FunctionCall{Name: "read_data", Arguments: `{"handle":"source","view":"dom","selector":"tbody > tr"}`}})
		if err != nil {
			t.Fatal(err)
		}
		out = modelObservationForRun(run, out, nil)
		var got struct {
			Nodes []struct {
				Text string `json:"text"`
			} `json:"nodes"`
		}
		if err := json.Unmarshal(out.Data, &got); err != nil {
			t.Fatal(err)
		}
		if len(got.Nodes) != 2 || !strings.Contains(got.Nodes[0].Text, pair[0]) || !strings.Contains(got.Nodes[0].Text, pair[1]) {
			t.Fatalf("short table field pair erased at crawler projection: %s", out.Data)
		}
		if got.Nodes[1].Text != "" || strings.Contains(string(out.Data), "private long article") {
			t.Fatal("table row became a prose reader", string(out.Data))
		}
	}
}

func TestTruncatedModelFeedbackSuppliesCauseAndExecutableCorrection(t *testing.T) {
	for _, limit := range []int{4096, 8192} {
		t.Run(strconv.Itoa(limit), func(t *testing.T) {
			t.Setenv("GORELAY_AI_MAX_COMPLETION_TOKENS", strconv.Itoa(limit))
			s, run, _ := storedHTMLReader(t, `<h2>saved field</h2>`)
			var requests []ChatRequest
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request ChatRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				requests = append(requests, request)
				finish := "length"
				if len(requests) > 1 {
					finish = "stop"
				}
				json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": Message{Role: "assistant", Content: "", ReasoningContent: "draft program interrupted"}, "finish_reason": finish}}, "usage": Usage{CompletionTokens: int64(limit)}})
			}))
			defer server.Close()
			provider, err := NewModelProvider(ProviderConfig{BaseURL: server.URL, Model: "fixture", APIKey: "fixture"})
			if err != nil {
				t.Fatal(err)
			}
			s.Model = provider
			run, err = s.Data.Claim(context.Background(), run.ID, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			state, err := s.state(context.Background(), run)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.think(context.Background(), state, 0, &LoopRound{RoundID: agentdata.Token(), StartedAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
			run, err = s.Data.Claim(context.Background(), run.ID, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			state, err = s.state(context.Background(), run)
			if err != nil {
				t.Fatal(err)
			}
			for _, op := range state.Operations {
				if op.Kind != "model" && op.Kind != "inspect_source" {
					t.Fatal("truncated proposal dispatched a tool", op.Kind)
				}
			}
			if err := s.think(context.Background(), state, 1, &LoopRound{RoundID: agentdata.Token(), StartedAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
			if len(requests) != 2 || requests[0].MaxCompletionTokens != limit {
				t.Fatal("normal request configuration lost", len(requests))
			}
			var failure FailureInfo
			for _, m := range requests[1].Messages {
				if m.Role != "tool" {
					continue
				}
				var snapshot struct {
					Failure FailureInfo `json:"model_failure"`
				}
				if json.Unmarshal([]byte(m.Content), &snapshot) == nil && snapshot.Failure.Kind != "" {
					failure = snapshot.Failure
				}
			}
			if failure.Code != "MODEL_RESPONSE_TRUNCATED" || failure.Recovery == nil || len(failure.Recovery.Steps) == 0 {
				t.Fatalf("next model HTTP only says wrong without a repair: %+v", failure)
			}
			raw, _ := json.Marshal(failure.Details)
			if !strings.Contains(string(raw), fmt.Sprint(limit)) || !strings.Contains(failure.Recovery.Instruction, "code") {
				t.Fatal("truncation limit or concrete code submission repair missing", string(raw), failure.Recovery)
			}
			// A subsequent normal response must not inherit the old transport failure.
			ops, _ := s.Data.Operations(context.Background(), run.ID)
			if latestModelFailure(ops) != nil {
				t.Fatal("normal response inherited truncation failure")
			}
		})
	}
}
