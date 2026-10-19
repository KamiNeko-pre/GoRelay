package aiapp

import (
	"context"
	"encoding/json"
	"fmt"
	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/testdb"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type unproductiveModel struct {
	calls    int
	mode     string
	curReads int
	Phases   map[string]bool
}

func (m *unproductiveModel) Chat(_ context.Context, req ChatRequest) (ChatResponse, error) {
	m.calls++
	_, facts := auditVisibleReceipts(req)
	var pages []struct {
		Handle string `json:"handle"`
	}
	json.Unmarshal(facts["acquired_pages"], &pages)
	for _, message := range req.Messages {
		if message.Role != "tool" {
			continue
		}
		var snapshot struct {
			Pages []struct {
				Handle string `json:"handle"`
			} `json:"acquired_pages"`
		}
		if json.Unmarshal([]byte(message.Content), &snapshot) == nil && len(snapshot.Pages) > 0 {
			pages = snapshot.Pages
		}
	}
	handle := ""
	if len(pages) > 0 {
		handle = pages[0].Handle
	}
	var recovery struct {
		Phase string `json:"phase"`
	}
	json.Unmarshal(facts["delivery_recovery"], &recovery)
	if m.Phases == nil {
		m.Phases = map[string]bool{}
	}
	if recovery.Phase != "" {
		m.Phases[recovery.Phase] = true
	}
	name := "read_source_content"
	args := map[string]any{"inspection_call_id": handle, "offset": m.calls * 400}
	if m.mode == "pointer" {
		name = "read_observation"
		args = map[string]any{"tool_call_id": handle, "pointer": "/data/source/body", "offset": m.calls * 400, "limit": 800}
	}
	if m.mode == "notes" {
		name = "update_task_progress"
		args = map[string]any{"expected_version": m.calls - 1, "requirements": []any{}, "notes": []string{fmt.Sprintf("changed note %d", m.calls)}, "next_purpose": "keep investigating"}
	}
	if m.mode == "data" || m.mode == "repeat_data" {
		name = "read_data"
		args = map[string]any{"handle": handle, "view": "dom", "limit": 1}
		if m.mode == "data" {
			for i := len(req.Messages) - 1; i >= 0; i-- {
				if req.Messages[i].Role != "tool" {
					continue
				}
				var out ToolResult
				var data map[string]any
				if json.Unmarshal([]byte(req.Messages[i].Content), &out) == nil && out.Name == name && json.Unmarshal(out.Data, &data) == nil {
					if cursor, ok := data["next_cursor"].(string); ok {
						args = map[string]any{"cursor": cursor}
						m.curReads++
						break
					}
				}
			}
		}
		if m.mode == "repeat_data" {
			args["offset"] = 0
		}
	}
	raw, _ := json.Marshal(args)
	return ChatResponse{Message: Message{Role: "assistant", ToolCalls: []ToolCall{{ID: fmt.Sprintf("step-%d", m.calls), Type: "function", Function: FunctionCall{Name: name, Arguments: string(raw)}}}}, Usage: &Usage{TotalTokens: 1}}, nil
}

func TestHostStopsUnproductiveModelWithoutCheckpointCooperation(t *testing.T) {
	// Exercise the liveness controller at a short explicit threshold. New
	// timeboxed runs do not inherit the old 12-call work budget.
	t.Setenv("GORELAY_RUN_STAGNATION_CALLS", "3")
	for _, mode := range []string{"source", "pointer", "notes", "data", "repeat_data"} {
		t.Run(mode, func(t *testing.T) {
			s := NewService(testdb.Open(t))
			if err := s.Migrate(); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			spec := TaskSpec{Goal: "collect supported body records", SchemaVersion: "v1", RequiredFields: []FieldRequirement{{Name: "body", Type: "string"}}}
			r, err := s.Create(ctx, CreateRunRequest{RequestKey: mode, Goal: spec.Goal, Spec: &spec})
			if err != nil {
				t.Fatal(err)
			}
			body := "<main>"
			for n := 0; n < 100; n++ {
				body += fmt.Sprintf(`<section class="region_%03d"><h2>Heading %d</h2><a href="/detail/%d">Observed link</a></section>`, n, n, n)
			}
			body += "</main>"
			receipt, _ := toolData(ToolCall{ID: "seed", Function: FunctionCall{Name: "inspect_source"}}, inspectResponse(acquisition.Response{Body: body, Receipt: acquisition.Receipt{Method: "GET", URL: "https://fixture.example/document", Status: 200, FetchedAt: time.Now()}}, acquisition.Stats{}))
			raw, _ := json.Marshal(receipt)
			if err := s.Data.DB.Create(&agentdata.Operation{RunID: r.ID, Key: "tool:seed", Kind: "inspect_source", Stage: "succeeded", ResultJSON: string(raw)}).Error; err != nil {
				t.Fatal(err)
			}
			model := &unproductiveModel{mode: mode}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				var req ChatRequest
				if err := json.NewDecoder(request.Body).Decode(&req); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				response, err := model.Chat(request.Context(), req)
				if err != nil {
					t.Error(err)
					w.WriteHeader(500)
					return
				}
				json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": response.Message, "finish_reason": "tool_calls"}}})
			}))
			defer server.Close()
			provider, err := NewModelProvider(ProviderConfig{BaseURL: server.URL, APIKey: "test-only", Model: "adversarial-loop-test"})
			if err != nil {
				t.Fatal(err)
			}
			s.Model = provider
			for n := 0; n < 64; n++ {
				current, _ := s.Data.Run(ctx, r.ID)
				if current.IsTerminal() {
					break
				}
				if err := s.Advance(ctx, r.ID); err != nil {
					t.Fatal(err)
				}
				// Restore every step: a host counter must live in SQL, not process memory.
				s = NewService(s.Data.DB)
				s.Model = provider
			}
			current, _ := s.Data.Run(ctx, r.ID)
			var host HostProgress
			json.Unmarshal([]byte(current.HostProgressJSON), &host)
			if mode == "data" {
				// Fresh catalog entries are preparation evidence, not delivery.
				// The new contract forbids fixed-round termination while that
				// evidence is still arriving; the explicit deadline still owns time.
				if current.IsTerminal() || host.Recovery.Phase != "correcting" || host.NoProgressModelRounds == 0 || model.curReads < 2 {
					t.Fatal("fresh structural work was stopped or erased delivery debt", current.Status, host)
				}
				var budget BudgetSnapshot
				json.Unmarshal([]byte(current.BudgetJSON), &budget)
				budget.WorkDeadline = time.Now().Add(-time.Second)
				if err := s.Data.DB.Model(&agentdata.Run{}).Where("id = ?", r.ID).Update("budget_json", string(mustJSON(budget))).Error; err != nil {
					t.Fatal(err)
				}
				for n := 0; n < 6; n++ {
					if err := s.Advance(ctx, r.ID); err != nil {
						t.Fatal(err)
					}
					current, _ = s.Data.Run(ctx, r.ID)
					if current.IsTerminal() {
						break
					}
				}
				json.Unmarshal([]byte(current.HostProgressJSON), &host)
			}
			maxCalls := int(hostProgressLimit(BudgetSnapshot{Limits: BudgetLimits{StagnationModelCalls: 3}})*3) + int(deliveryIdleRoundLimit) + 2
			if current.Status != "failed" || mode != "data" && model.calls > maxCalls || host.Recovery.Phase != "closed" || !model.Phases["correcting"] || !model.Phases["concluding"] {
				t.Fatalf("unproductive model escaped host stop: status=%s calls=%d cursor_reads=%d phases=%v host=%s result=%s error=%s", current.Status, model.calls, model.curReads, model.Phases, current.HostProgressJSON, current.ResultJSON, current.LastError)
			}
			if mode == "data" && model.curReads < 2 {
				t.Fatal("finite structural preparation was not exercised")
			}
		})
	}
}

func TestHostProgressCreditsAcceptedFactsNotModelClaims(t *testing.T) {
	state := decisionState()
	state.Spec = &TaskSpec{Goal: "collect public records"}
	for n := 1; n <= 5; n++ {
		state.Operations = append(state.Operations, agentdata.Operation{ID: uint64(n), Kind: "model", Stage: "succeeded"})
	}
	p := advanceHostProgressForTest(HostProgress{}, state)
	state.Progress = TaskProgress{Version: 99, Requirements: []RequirementResult{{ID: "invented", Status: "satisfied"}}}
	state.Operations = append(state.Operations, agentdata.Operation{ID: 6, Kind: "read_observation", Stage: "succeeded", ResultJSON: `{"text":"new slice"}`}, agentdata.Operation{ID: 7, Kind: "model", Stage: "succeeded"})
	p = advanceHostProgressForTest(p, state)
	if p.NoProgressModelRounds != 6 {
		t.Fatal("model-written claims or reads reset host progress")
	}
	validated, _ := toolData(ToolCall{}, map[string]string{"artifact_hash": "actually-validated"})
	raw, _ := json.Marshal(validated)
	state.Operations = append(state.Operations, agentdata.Operation{ID: 8, Kind: "validate_program", Stage: "succeeded", ResultJSON: string(raw)})
	p = advanceHostProgressForTest(p, state)
	if p.NoProgressModelRounds != 0 {
		t.Fatal("validated capability did not reset host progress")
	}
	state.Operations = append(state.Operations, agentdata.Operation{ID: 9, Kind: "model", Stage: "succeeded"}, agentdata.Operation{ID: 10, Kind: "register_source", Stage: "succeeded", ResultJSON: string(raw)})
	p = advanceHostProgressForTest(p, state)
	if p.NoProgressModelRounds != 1 {
		t.Fatal("same artifact registered again manufactured progress")
	}
}

func TestHostProgressAllowsNewAcquisitionBeforeAcceptedRecords(t *testing.T) {
	for _, body := range []string{"<article>New procurement detail</article>", `{"items":[{"release":"2.1"}]}`} {
		t.Run(body, func(t *testing.T) {
			state := decisionState()
			state.Spec = &TaskSpec{Goal: "collect public records"}
			for id := uint64(1); id <= 6; id++ {
				state.Operations = append(state.Operations, agentdata.Operation{ID: id, Kind: "model", Stage: "succeeded"})
			}
			before := advanceHostProgressForTest(HostProgress{}, state)
			inspection, _ := toolData(ToolCall{}, inspectResponse(acquisition.Response{Body: body, Receipt: acquisition.Receipt{Method: "GET", URL: "https://fixture.example/detail", Status: 200, FetchedAt: time.Now()}}, acquisition.Stats{}))
			raw, _ := json.Marshal(inspection)
			state.Operations = append(state.Operations, agentdata.Operation{ID: 7, Kind: "inspect_source", Stage: "succeeded", ResultJSON: string(raw)})
			after := advanceHostProgressForTest(before, state)
			if after.NoProgressModelRounds != 0 {
				t.Fatalf("new host-fetched source was stopped before code generation: %+v", after)
			}
			// Same page through new call IDs, and model-written notes cannot reset it.
			state.Operations = append(state.Operations, agentdata.Operation{ID: 8, Kind: "model", Stage: "succeeded"})
			after = advanceHostProgressForTest(after, state)
			state.Operations = append(state.Operations, agentdata.Operation{ID: 9, Kind: "inspect_source", Stage: "succeeded", ResultJSON: string(raw)})
			state.Progress.Notes = []string{"claim more progress"}
			after = advanceHostProgressForTest(after, state)
			if after.NoProgressModelRounds != 1 {
				t.Fatal("repeated acquisition or notes manufactured progress")
			}
		})
	}
}

func TestHostProgressIgnoresFailedOrUnobservedAcquisition(t *testing.T) {
	state := decisionState()
	state.Spec = &TaskSpec{Goal: "collect public records"}
	state.Operations = []agentdata.Operation{{ID: 1, Kind: "model", Stage: "succeeded"}}
	p := advanceHostProgressForTest(HostProgress{}, state)
	for _, receipt := range []acquisition.Receipt{{Method: "GET", URL: "https://fixture.example/failed", Status: 503}, {URL: "https://fixture.example/invented", Status: 200}} {
		result, _ := toolData(ToolCall{}, inspectResponse(acquisition.Response{Body: "not successfully acquired", Receipt: receipt}, acquisition.Stats{}))
		raw, _ := json.Marshal(result)
		state.Operations = append(state.Operations, agentdata.Operation{ID: uint64(len(state.Operations) + 1), Kind: "inspect_source", Stage: "succeeded", ResultJSON: string(raw)})
		p = advanceHostProgressForTest(p, state)
		if p.NoProgressModelRounds != 1 {
			t.Fatal("non-successful fetch manufactured progress")
		}
	}
}
