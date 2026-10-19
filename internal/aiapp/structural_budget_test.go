package aiapp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go-lua-crawler/internal/agentdata"
)

func TestCrawlerStructuralReadHasNoSmallByteCeiling(t *testing.T) {
	for _, catalog := range []bool{false, true} {
		t.Run(fmt.Sprint("catalog=", catalog), func(t *testing.T) {
			var body strings.Builder
			body.WriteString("<main><ul>")
			last := ""
			for i := 0; i < 60; i++ {
				if catalog {
					last = fmt.Sprintf("observed_%03d_%s", i, strings.Repeat("x", 80))
					fmt.Fprintf(&body, `<h2 id="%s">Field %d</h2>`, last, i)
				} else {
					last = fmt.Sprintf("/detail/%03d?token=%s", i, strings.Repeat("x", 150))
					fmt.Fprintf(&body, `<li><a href="%s">Record %d</a><span>2026-10-16</span></li>`, last, i)
				}
			}
			body.WriteString("</ul></main>")
			s, run, _ := storedHTMLReader(t, body.String())
			args := dataReadArgs{Handle: "source", View: "dom", Limit: 2000}
			if !catalog {
				args.Selector = "li"
				args.Limit = 60
			}
			call := ToolCall{ID: "large-structure", Type: "function", Function: FunctionCall{Name: "read_data", Arguments: string(mustJSON(args))}}
			out, err := s.readData(context.Background(), run, call)
			if err != nil {
				t.Fatal("small byte ceiling rejected a valid structural page", err)
			}
			var data struct {
				Nodes   []any `json:"nodes"`
				Catalog []any `json:"selector_catalog"`
				Next    any   `json:"next_read"`
			}
			json.Unmarshal(out.Data, &data)
			if data.Next != nil || !catalog && len(data.Nodes) != 60 || catalog && len(data.Catalog) < 60 || !strings.Contains(string(out.Data), last) {
				t.Fatalf("requested structures were cut before context budgeting: nodes=%d catalog=%d next=%v last=%t", len(data.Nodes), len(data.Catalog), data.Next, strings.Contains(string(out.Data), last))
			}
			if len(out.Data) <= 6<<10 {
				t.Fatal("fixture did not cross the reported ceiling")
			}
			view := modelObservationForRun(run, out, nil)
			if !strings.Contains(string(view.Data), last) || strings.Contains(string(view.Data), "read_observation") {
				t.Fatal("projection discarded a valid structural result")
			}
			encoded := string(mustJSON(out))
			if err := s.Data.DB.Create(&agentdata.Operation{RunID: run.ID, Key: "tool:" + call.ID, Kind: call.Function.Name, InputHash: canonicalToolInput(call.Function), Stage: "succeeded", ResultJSON: encoded}).Error; err != nil {
				t.Fatal(err)
			}
			run, err = s.Data.Claim(context.Background(), run.ID, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			state, err := s.state(context.Background(), run)
			if err != nil {
				t.Fatal(err)
			}
			state.Messages = []Message{{Role: "assistant", ToolCalls: []ToolCall{call}}, {Role: "tool", ToolCallID: call.ID, Content: encoded}}
			feedback := recentToolFeedback(state)
			if !strings.Contains(string(mustJSON(feedback)), last) {
				t.Fatal("history fallback silently reduced structural data to 2 KiB")
			}
			var received ChatRequest
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
					t.Error(err)
				}
				json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": Message{Role: "assistant", Content: "structure received"}, "finish_reason": "stop"}}})
			}))
			defer server.Close()
			s.Model, err = NewModelProvider(ProviderConfig{BaseURL: server.URL, APIKey: "fixture", Model: "fixture"})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.think(context.Background(), state, 0, &LoopRound{RoundID: agentdata.Token(), StartedAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(mustJSON(received)), last) {
				t.Fatal("model HTTP lost the final selected structure")
			}
			auditSave(t, fmt.Sprintf("large-structure-catalog-%t-http", catalog), received)
		})
	}
}

func TestCrawlerLargeWorkflowFeedbackUsesContextBudget(t *testing.T) {
	_, state := promptPolicyState(t)
	items := []any{}
	for i := 0; i < 400; i++ {
		items = append(items, map[string]any{"record_id": fmt.Sprintf("row-%04d", i), "title": strings.Repeat("observed scalar ", 7), "body": strings.Repeat("PRIVATE ARTICLE PROSE ", 100)})
	}
	out, _ := toolData(ToolCall{ID: "large-probe", Function: FunctionCall{Name: "validate_program"}}, map[string]any{"id": "candidate", "items": items, "next_action": map[string]any{"tool": "register_source", "arguments": map[string]any{"candidate_id": "candidate"}}})
	view := modelObservationForRun(state.Run, out, nil)
	if !strings.Contains(string(view.Data), "row-0399") || strings.Contains(string(view.Data), "PRIVATE ARTICLE PROSE") || !strings.Contains(string(view.Data), "register_source") {
		t.Fatal("small workflow ceiling lost structured records/control or exposed prose")
	}
	state.Operations = append(state.Operations, agentdata.Operation{RunID: state.Run.ID, Key: "tool:" + out.CallID, Kind: out.Name, Stage: "succeeded", ResultJSON: string(mustJSON(out))})
	if !strings.Contains(string(mustJSON(recentToolFeedback(state))), "row-0399") {
		t.Fatal("large feedback was discarded by the historical 12 KiB ceiling")
	}
}
