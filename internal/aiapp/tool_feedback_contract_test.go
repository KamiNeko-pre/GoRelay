package aiapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/testdb"
)

func TestToolFeedbackNamesInvalidFieldAndExpectedContract(t *testing.T) {
	r := agentdata.Run{BudgetJSON: `{"loop_version":5}`}
	for _, tc := range []struct{ tool, args, field, expected string }{
		{"finish_task", `{"status":"almost","reason":"done"}`, "status", "completed"},
		{"validate_program", `{"code":"return true,{}","kind":"javascript","source_id":"new","purpose":"collect","url":"https://example.test/"}`, "kind", "adapter"},
		{"read_data", `{"handle":"source","view":"plaintext"}`, "view", "dom"},
		{"plan_task", `{"required_fields":[{"name":"title"}]}`, "required_fields[0].type", "type"},
		{"read_data", `{"handle":"source","limit":-1}`, "limit", "0"},
		{"replace_record", `{"old_task_id":"guess","old_record_key":"a","new_task_id":2,"new_record_key":"b","reason":"correct"}`, "old_task_id", "integer"},
		{"submit_acquisition", `{"source_id":"published","candidate_id":"candidate","url":"https://example.test/"}`, "source_id", "exactly one"},
	} {
		t.Run(tc.tool+"-"+tc.field, func(t *testing.T) {
			err := requiredToolArguments(FunctionCall{Name: tc.tool, Arguments: tc.args}, runToolDefinitions(r))
			if err == nil {
				t.Fatal("invalid parameter escaped the shared schema boundary")
			}
			failure := captureFailure(err, 0, false)
			raw, _ := json.Marshal(failure)
			if failure.Recovery == nil || !strings.Contains(string(raw), tc.field) || !strings.Contains(string(raw), tc.expected) {
				t.Fatalf("missing field or accepted contract: %s", raw)
			}
		})
	}
	if err := requiredToolArguments(FunctionCall{Name: "read_data", Arguments: `{"handle":"source","view":"dom","limit":64}`}, runToolDefinitions(r)); err != nil {
		t.Fatal("valid structural read rejected", err)
	}
}

func TestEveryToolFailureIncludesReservationAndProtocolErrors(t *testing.T) {
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	r, err := s.Create(context.Background(), CreateRunRequest{RequestKey: "feedback-reservation", Goal: "collect titles"})
	if err != nil {
		t.Fatal(err)
	}
	r, err = s.Data.Claim(context.Background(), r.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// A stale ownership token fails before dispatch or receipt persistence.
	stale := r
	stale.Token = "stale-owner"
	for _, d := range runToolDefinitions(r) {
		call := ToolCall{ID: "stale-" + d.Function.Name, Type: "function", Function: FunctionCall{Name: d.Function.Name, Arguments: `{}`}}
		out, err := s.executeDurableTool(context.Background(), stale, call)
		if err == nil || out.Failure == nil || out.Failure.Recovery == nil || out.Name != call.Function.Name || out.CallID != call.ID || out.Error == "" {
			t.Errorf("%s lost its pre-dispatch failure: %+v / %v", d.Function.Name, out, err)
		}
	}
	out, err := s.executeDurableTool(context.Background(), r, ToolCall{ID: "invalid-protocol", Type: "not-a-function", Function: FunctionCall{Name: "list_sources", Arguments: `{}`}})
	if err == nil || out.Failure == nil || out.Failure.Code == "" {
		t.Errorf("invalid call envelope lost failure context: %+v / %v", out, err)
	}
}

func TestAllOfferedToolsReportOriginalAndDuplicateErrorsToModelHTTP(t *testing.T) {
	s := NewService(testdb.Open(t))
	s.Search = &ordinaryFailureSearch{}
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	var count atomic.Int32
	names := []string{}
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		n := count.Add(1)
		auditSave(t, fmt.Sprintf("all-tools-steps-request-%d", n), req)
		calls := []ToolCall{}
		switch n {
		case 1:
			for _, d := range req.Tools {
				names = append(names, d.Function.Name)
				calls = append(calls, ToolCall{ID: "first-" + d.Function.Name, Type: "function", Function: FunctionCall{Name: d.Function.Name, Arguments: `{"unexpected_field":true}`}})
			}
		case 2, 3:
			visible := map[string]ToolResult{}
			for _, m := range req.Messages {
				if m.Role == "tool" {
					var out ToolResult
					if json.Unmarshal([]byte(m.Content), &out) == nil {
						visible[out.CallID] = out
					}
				}
			}
			for _, name := range names {
				id := "first-" + name
				if n == 3 {
					id = "duplicate-" + name
				}
				out := visible[id]
				if out.Failure == nil || out.Failure.Recovery == nil || out.Error == "" {
					t.Errorf("request %d: %s has no actionable failure: %+v", n, name, out)
					continue
				}
				if len(out.Failure.Recovery.Steps) == 0 || out.Failure.Recovery.Instruction == "" || out.Failure.Recovery.Tool == "" {
					t.Errorf("request %d: %s error has no concrete correction instruction and step", n, name)
				}
				raw, _ := json.Marshal(out.Failure)
				if !strings.Contains(string(raw), "unexpected_field") {
					t.Errorf("request %d: %s original parameter cause lost: %s", n, name, raw)
				}
				if n == 3 && (out.Failure.Code != "DUPLICATE_TOOL_CALL" || !strings.Contains(out.Failure.Message, name) || !strings.Contains(string(raw), `"original_call_id":"first-`+name+`"`)) {
					t.Errorf("%s duplicate did not explicitly name prior call and tool: %s", name, raw)
				}
				if n == 2 {
					calls = append(calls, ToolCall{ID: "duplicate-" + name, Type: "function", Function: FunctionCall{Name: name, Arguments: `{"unexpected_field":true}`}})
				}
			}
			if n == 3 {
				calls = append(calls, ToolCall{ID: "corrected-catalog", Type: "function", Function: FunctionCall{Name: "list_sources", Arguments: `{}`}})
			}
		case 4:
			receipts, _ := auditVisibleReceipts(req)
			if receipts["list_sources"].Error != "" {
				t.Error("a corrected call was blocked as a duplicate")
			}
			calls = append(calls, ToolCall{ID: "conclude", Type: "function", Function: FunctionCall{Name: "finish_task", Arguments: `{"status":"failed","reason":"protocol-only check; no acquired records","gaps":["titles"]}`}})
		default:
			t.Errorf("unexpected model request %d", n)
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": Message{Role: "assistant", ToolCalls: calls}, "finish_reason": "tool_calls"}}})
	}))
	defer model.Close()
	var err error
	s.Model, err = NewModelProvider(ProviderConfig{BaseURL: model.URL, APIKey: "test-only", Model: "all-tools-feedback-mock"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Create(context.Background(), CreateRunRequest{RequestKey: "every-tool-http-feedback", Goal: "collect titles"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10 && count.Load() < 4; i++ {
		if err = s.Advance(context.Background(), r.ID); err != nil {
			current, _ := s.Data.Run(context.Background(), r.ID)
			state, stateErr := s.state(context.Background(), current)
			if stateErr == nil {
				_, profile, compactErr := compactContextMeasured(state, state.Messages)
				auditSave(t, "all-tools-steps-failed-context", map[string]any{"state": state, "profile": profile, "error": fmt.Sprint(compactErr)})
			}
			t.Fatal(err)
		}
	}
	if count.Load() != 4 || len(names) != len(runToolDefinitions(r)) {
		t.Fatal(fmt.Sprintf("incomplete actual request coverage: requests=%d tools=%v", count.Load(), names))
	}
	t.Logf("all offered tools exercised through actual model HTTP: %v", names)
}

func TestToolResourceFailuresExposeTheMissingReferenceAndRecovery(t *testing.T) {
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	spec := TaskSpec{Goal: "collect titles", SchemaVersion: "v1", RequiredFields: []FieldRequirement{{Name: "title", Type: "string"}}}
	r, err := s.Create(context.Background(), CreateRunRequest{RequestKey: "resource-feedback", Goal: spec.Goal, Spec: &spec})
	if err != nil {
		t.Fatal(err)
	}
	r, err = s.Data.Claim(context.Background(), r.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, args, field, reason string }{
		{"register_source", `{"candidate_id":"not-issued","description":"collect titles"}`, "candidate_id", "not-issued"},
		{"submit_acquisition", `{"candidate_id":"not-issued","url":"https://example.test/"}`, "candidate_id", "not-issued"},
		{"read_data", `{"cursor":"%%%"}`, "cursor", "base64"},
		{"read_source_experience", `{"source_id":"not-published"}`, "source_id", "not-published"},
		{"submit_extraction", `{"inspection_call_id":"not-observed","items":[]}`, "inspection_call_id", "not-observed"},
		{"validate_program", `{"code":"return true,{}","kind":"adapter","source_id":"` + strings.Repeat("a", 65) + `","purpose":"collect titles","url":"https://example.test/"}`, "source_id", "64"},
	} {
		out, err := s.executeDurableTool(context.Background(), r, ToolCall{ID: "resource-" + tc.name, Type: "function", Function: FunctionCall{Name: tc.name, Arguments: tc.args}})
		if err == nil || out.Failure == nil {
			t.Errorf("%s did not fail", tc.name)
			continue
		}
		raw, _ := json.Marshal(out.Failure)
		if !strings.Contains(string(raw), `"field":"`+tc.field+`"`) || !strings.Contains(string(raw), tc.reason) || out.Failure.Recovery == nil || out.Failure.Recovery.Instruction == out.Error {
			t.Errorf("%s lost the actionable source of its failure: %s", tc.name, raw)
		}
	}
}

func TestBudgetFailureNamesExhaustedResource(t *testing.T) {
	for _, resource := range []string{"tool_calls", "http_requests", "http_bytes"} {
		snapshot := BudgetSnapshot{LoopVersion: 5, Limits: DefaultBudgetLimits()}
		cost := CallCost{}
		switch resource {
		case "tool_calls":
			snapshot.Spent.ToolCalls = snapshot.Limits.ToolCalls
			cost.ToolCalls = 1
		case "http_requests":
			snapshot.Spent.HTTPRequests = snapshot.Limits.HTTPRequests
			cost.HTTPRequests = 1
		case "http_bytes":
			snapshot.Spent.HTTPBytes = snapshot.Limits.HTTPBytes
			cost.HTTPBytes = 1
		}
		budget, err := NewRunBudget(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		err = budget.Reserve(cost, time.Now())
		if err == nil {
			t.Fatal("expected exhausted resource")
		}
		failure := captureFailure(err, 0, false)
		raw, _ := json.Marshal(failure)
		if !strings.Contains(string(raw), `"resource":"`+resource+`"`) || !strings.Contains(string(raw), `"limit"`) || !strings.Contains(string(raw), `"requested"`) {
			t.Errorf("budget cause gives no exhausted dimension: %s", raw)
		}
	}
}

func TestBusinessFailureCausesReachTheNextModelHTTP(t *testing.T) {
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	runnerMessage := "<string>:7: attempted to call unavailable html_attr; use the documented sandbox API"
	runner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(RunnerResponse{Error: &RunnerError{Code: "LUA_RUNTIME_ERROR", Message: runnerMessage}})
	}))
	defer runner.Close()
	s.Runner, _ = NewRunnerClient(runner.URL, "test-only")
	causes := map[string]string{"register_source": "CANDIDATE_UNAVAILABLE", "submit_acquisition": "CANDIDATE_UNAVAILABLE", "submit_extraction": "INSPECTION_UNAVAILABLE", "read_source_experience": "SOURCE_UNAVAILABLE", "read_data": "INVALID_READ_CURSOR", "validate_program": "LUA_RUNTIME_ERROR"}
	var requests atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		calls := []ToolCall{}
		if requests.Add(1) == 1 {
			for _, c := range []struct{ name, args string }{
				{"register_source", `{"candidate_id":"not-issued","description":"collect"}`},
				{"submit_acquisition", `{"candidate_id":"not-issued","url":"https://example.test/"}`},
				{"submit_extraction", `{"inspection_call_id":"not-observed","items":[]}`},
				{"read_source_experience", `{"source_id":"not-published"}`},
				{"read_data", `{"cursor":"%%%"}`},
				{"validate_program", `{"kind":"adapter","code":"return true,{items={}}","source_id":"fresh","purpose":"collect titles","url":"https://example.test/"}`},
			} {
				calls = append(calls, ToolCall{ID: "business-" + c.name, Type: "function", Function: FunctionCall{Name: c.name, Arguments: c.args}})
			}
		} else {
			receipts, _ := auditVisibleReceipts(req)
			for name, code := range causes {
				out := receipts[name]
				if out.Failure == nil || out.Failure.Code != code || out.Failure.Details == nil || out.Failure.Recovery == nil {
					t.Errorf("%s business diagnosis was lost before actual HTTP: %+v", name, out)
				}
			}
			if receipts["validate_program"].Failure == nil || !strings.Contains(receipts["validate_program"].Failure.Message, runnerMessage) {
				t.Error("runner's original line-level cause was hidden")
			}
			calls = append(calls, ToolCall{ID: "business-conclude", Type: "function", Function: FunctionCall{Name: "finish_task", Arguments: `{"status":"failed","reason":"controlled error propagation check","gaps":["titles"]}`}})
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": Message{Role: "assistant", ToolCalls: calls}, "finish_reason": "tool_calls"}}})
	}))
	defer model.Close()
	var err error
	s.Model, err = NewModelProvider(ProviderConfig{BaseURL: model.URL, APIKey: "test-only", Model: "business-feedback-mock"})
	if err != nil {
		t.Fatal(err)
	}
	spec := TaskSpec{Goal: "collect titles", SchemaVersion: "v1", RequiredFields: []FieldRequirement{{Name: "title", Type: "string"}}}
	r, err := s.Create(context.Background(), CreateRunRequest{RequestKey: "business-next-model", Goal: spec.Goal, Spec: &spec})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5 && requests.Load() < 2; i++ {
		if err = s.Advance(context.Background(), r.ID); err != nil {
			t.Fatal(err)
		}
	}
	if requests.Load() != 2 {
		t.Fatal("no next-model error correction opportunity")
	}
}

func TestNewModelDecisionCannotReuseAnAlreadyCommittedCallID(t *testing.T) {
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		n := requests.Add(1)
		call := ToolCall{ID: "existing-catalog", Type: "function", Function: FunctionCall{Name: "list_sources", Arguments: `{}`}}
		if n >= 3 {
			raw, _ := json.Marshal(req.Messages)
			for _, want := range []string{"existing-catalog", "list_sources", "already", "tool_calls[0].id"} {
				if !strings.Contains(string(raw), want) {
					t.Errorf("reused ID rejection lost %q in next model request", want)
				}
			}
			call = ToolCall{ID: "id-conclusion", Type: "function", Function: FunctionCall{Name: "finish_task", Arguments: `{"status":"failed","reason":"protocol check","gaps":["titles"]}`}}
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": Message{Role: "assistant", ToolCalls: []ToolCall{call}}, "finish_reason": "tool_calls"}}})
	}))
	defer model.Close()
	var err error
	s.Model, err = NewModelProvider(ProviderConfig{BaseURL: model.URL, APIKey: "test-only", Model: "repeat-id-mock"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Create(context.Background(), CreateRunRequest{RequestKey: "reused-tool-id", Goal: "collect titles"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 7 && requests.Load() < 3; i++ {
		if err = s.Advance(context.Background(), r.ID); err != nil && !errors.Is(err, ErrInvalidProposal) {
			t.Fatal(err)
		}
	}
	if requests.Load() != 3 {
		t.Fatal("missing explicit rejection opportunity")
	}
	var rows int64
	s.Data.DB.Model(&agentdata.Operation{}).Where("run_id = ? AND `key` = ?", r.ID, "tool:existing-catalog").Count(&rows)
	if rows != 1 {
		t.Fatal("committed original receipt was changed or re-executed")
	}
}
