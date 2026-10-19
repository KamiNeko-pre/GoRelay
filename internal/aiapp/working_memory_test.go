package aiapp

import (
	"context"
	"encoding/json"
	"fmt"
	"go-lua-crawler/internal/agentdata"
	task "go-lua-crawler/internal/task"
	"go-lua-crawler/internal/testdb"
	"os"
	"strings"
	"testing"
	"time"
)

type countedDiscovery struct{ calls int }

func (p *countedDiscovery) Search(context.Context, string, int) (SearchResponse, error) {
	p.calls++
	return SearchResponse{Candidates: []SearchCandidate{}}, nil
}

func memoryRun(t *testing.T, goal string) (*Service, agentdata.Run, *countedDiscovery) {
	t.Helper()
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	spec := TaskSpec{Goal: goal, SchemaVersion: "v1", RequiredFields: []FieldRequirement{{Name: "body", Type: "string"}}, Requirements: []TaskRequirement{{ID: "scope", Description: "cover the requested scope"}}}
	r, err := s.Create(context.Background(), CreateRunRequest{RequestKey: "memory", Goal: goal, Spec: &spec})
	if err != nil {
		t.Fatal(err)
	}
	// Also exercises the frozen contract on an independently restored run.
	var budget map[string]any
	if err = json.Unmarshal([]byte(r.BudgetJSON), &budget); err != nil {
		t.Fatal(err)
	}
	budget["loop_version"] = 2
	raw, _ := json.Marshal(budget)
	if err = s.Data.DB.Model(&r).Update("budget_json", string(raw)).Error; err != nil {
		t.Fatal(err)
	}
	r, err = s.Data.Claim(context.Background(), r.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	p := &countedDiscovery{}
	s.Search = p
	return s, r, p
}

func TestWorkingMemoryAdvisesAssessmentWithoutBlockingExploration(t *testing.T) {
	for _, goal := range []string{"explain historical changes", "compare public release specifications"} {
		t.Run(goal, func(t *testing.T) {
			s, r, p := memoryRun(t, goal)
			for n := 0; n < 3; n++ {
				if err := s.Data.DB.Create(&agentdata.Operation{RunID: r.ID, Key: fmt.Sprintf("model-%d", n), Kind: "model", Stage: "succeeded", ResultJSON: `{}`}).Error; err != nil {
					t.Fatal(err)
				}
			}
			call := ToolCall{ID: "more-search", Type: "function", Function: FunctionCall{Name: "discover_sources", Arguments: `{"query":"another query"}`}}
			_, err := s.executeDurableTool(context.Background(), r, call)
			if err != nil || p.calls != 1 {
				t.Fatalf("advisory checkpoint blocked exploration: calls=%d err=%v", p.calls, err)
			}
			stored, _ := s.Data.Run(context.Background(), r.ID)
			var b BudgetSnapshot
			json.Unmarshal([]byte(stored.BudgetJSON), &b)
			if b.Spent.SearchCalls != 1 {
				t.Fatal("actual search attempt was not reserved")
			}
		})
	}
}

func TestWorkingMemoryAllowsInitialExplorationAndReads(t *testing.T) {
	s, r, p := memoryRun(t, "investigate a public component")
	call := ToolCall{ID: "initial", Type: "function", Function: FunctionCall{Name: "discover_sources", Arguments: `{"query":"initial discovery"}`}}
	if _, err := s.executeDurableTool(context.Background(), r, call); err != nil || p.calls != 1 {
		t.Fatalf("initial exploration blocked: %v", err)
	}
}

func TestWorkingMemoryCompactionRetainsUnassessedRounds(t *testing.T) {
	state := decisionState()
	raw, _ := json.Marshal(state.Budget)
	var b map[string]any
	json.Unmarshal(raw, &b)
	b["loop_version"] = 2
	raw, _ = json.Marshal(b)
	json.Unmarshal(raw, &state.Budget)
	var messages []Message
	for n := 0; n < 4; n++ {
		id := fmt.Sprintf("unassessed-%d", n)
		call := ToolCall{ID: id, Type: "function", Function: FunctionCall{Name: "read_evidence", Arguments: `{"evidence_id":"ev_test"}`}}
		out, _ := toolData(call, map[string]string{"text": strings.Repeat("original evidence ", 480)})
		encoded, _ := json.Marshal(out)
		state.Operations = append(state.Operations, agentdata.Operation{ID: uint64(n + 1), Key: "tool:" + id, Kind: "read_evidence", Stage: "succeeded", ResultJSON: string(encoded)})
		messages = append(messages, Message{Role: "assistant", ToolCalls: []ToolCall{call}}, Message{Role: "tool", ToolCallID: id, Content: string(encoded)})
	}
	got, err := CompactContext(state, messages)
	if err != nil {
		t.Fatal(err)
	}
	proposalCount := 0
	for _, m := range got {
		if m.Role == "assistant" && len(m.ToolCalls) > 0 && m.ToolCalls[0].Function.Name != "host_context" {
			proposalCount++
		}
	}
	if proposalCount != 4 {
		t.Fatalf("forgotten unassessed rounds: %d", proposalCount)
	}
	if contextRequestBytes(state, got) > 64<<10 {
		t.Fatal("whole context overflow")
	}
}

func TestWorkingMemoryCheckpointAcknowledgesObservedFactsAndSurvivesRestore(t *testing.T) {
	s, r, p := memoryRun(t, "compare public products")
	ctx := context.Background()
	for n := 0; n < 3; n++ {
		if err := s.Data.DB.Create(&agentdata.Operation{RunID: r.ID, Key: fmt.Sprintf("model-%d", n), Kind: "model", Stage: "succeeded", ResultJSON: `{}`}).Error; err != nil {
			t.Fatal(err)
		}
	}
	blocked := ToolCall{ID: "blocked", Type: "function", Function: FunctionCall{Name: "discover_sources", Arguments: `{"query":"later query"}`}}
	if _, err := s.executeDurableTool(ctx, r, blocked); err != nil || p.calls != 1 {
		t.Fatal("advisory review blocked execution", err)
	}
	checkpoint := ToolCall{ID: "checkpoint", Type: "function", Function: FunctionCall{Name: "update_task_progress", Arguments: `{"expected_version":0,"requirements":[{"id":"scope","status":"missing","explanation":"no supporting evidence yet","citations":[]}],"notes":["initial discovery exhausted"],"next_purpose":"find evidence for scope","explored_paths":[{"method":"search","target":"initial public query","outcome":"no result","reason":"no grounded candidate"}]}`}}
	if _, err := s.executeDurableTool(ctx, r, checkpoint); err != nil {
		t.Fatal(err)
	}
	stored, err := s.Data.Run(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	progress, err := progressOf(stored)
	if err != nil {
		t.Fatal(err)
	}
	if progress.ObservedThrough == 0 || progress.ModelRounds != 3 || progress.FactsSignature == "" || len(progress.ExploredPaths) != 1 {
		t.Fatalf("host acknowledgement absent: %+v", progress)
	}
	restored := NewService(s.Data.DB)
	restored.Search = p
	state, err := restored.state(ctx, stored)
	if err != nil {
		t.Fatal(err)
	}
	if taskReview(state).Required {
		t.Fatal("checkpoint did not clear review")
	}
	// Replaying the saved receipt never makes another external attempt.
	if _, err = restored.executeDurableTool(ctx, stored, blocked); err != nil || p.calls != 1 {
		t.Fatal("saved operation was silently retried", err)
	}
	allowed := blocked
	allowed.ID = "new-attempt"
	if _, err = restored.executeDurableTool(ctx, stored, allowed); err != nil || p.calls != 2 {
		t.Fatalf("assessed next action did not execute: %v", err)
	}
}

func TestWorkingMemoryChangedOutcomesRequireReviewButPendingAndPagingDoNot(t *testing.T) {
	for _, status := range []task.Status{task.StatusSucceeded, task.StatusFailed, task.StatusRunning} {
		state := decisionState()
		state.Budget.LoopVersion = 2
		state.Spec = &TaskSpec{Goal: "collect facts"}
		state.Progress = TaskProgress{Version: 1, FactsSignature: workingFactsSignature(LoopState{})}
		state.TaskResults = []ChildResult{{Task: task.CrawlTask{ID: 9, Status: status}}}
		if taskReview(state).Required != status.IsTerminal() {
			t.Fatalf("wrong outcome boundary for %s", status)
		}
	}
	state := decisionState()
	state.Budget.LoopVersion = 2
	state.Spec = &TaskSpec{Goal: "read a long document"}
	state.Progress = TaskProgress{Version: 1, FactsSignature: workingFactsSignature(LoopState{})}
	for n := 0; n < 2; n++ {
		state.Operations = append(state.Operations, agentdata.Operation{ID: uint64(n + 1), Kind: "read_observation", Stage: "succeeded", InputHash: fmt.Sprintf("page-%d", n)})
	}
	if taskReview(state).Required {
		t.Fatal("legitimate paging is an outcome change")
	}
}

func TestWorkingMemoryCompactionDropsOnlyAcknowledgedRounds(t *testing.T) {
	state := decisionState()
	state.Budget.LoopVersion = 2
	state.Progress = TaskProgress{Version: 1, ObservedThrough: 1}
	var messages []Message
	for n := 0; n < 4; n++ {
		id := fmt.Sprintf("round-%d", n)
		call := ToolCall{ID: id, Type: "function", Function: FunctionCall{Name: "read_evidence", Arguments: `{"evidence_id":"ev_test"}`}}
		out, _ := toolData(call, map[string]string{"text": strings.Repeat("retained evidence ", 650)})
		raw, _ := json.Marshal(out)
		state.Operations = append(state.Operations, agentdata.Operation{ID: uint64(n + 1), Key: "tool:" + id, Kind: "read_evidence", Stage: "succeeded", ResultJSON: string(raw)})
		messages = append(messages, Message{Role: "assistant", ToolCalls: []ToolCall{call}}, Message{Role: "tool", ToolCallID: id, Content: string(raw)})
	}
	got, err := CompactContext(state, messages)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, m := range got {
		for _, call := range m.ToolCalls {
			if call.Function.Name == "host_context" {
				continue
			}
			if call.ID == "round-0" {
				t.Fatal("assessed round retained despite size pressure")
			}
			count++
		}
	}
	if count != 3 {
		t.Fatalf("unassessed rounds discarded: %d", count)
	}
}

func TestWorkingMemoryStopsAfterUnproductiveRecoveryCycle(t *testing.T) {
	for _, partial := range []int{0, 1} {
		state := decisionState()
		state.Budget.LoopVersion = 2
		state.Spec = &TaskSpec{Goal: "finish the requested analysis"}
		state.Result = &TaskResult{Coverage: CoverageReport{PartialValidRecords: partial}}
		state.HostProgress = HostProgress{NoProgressModelRounds: hostProgressLimit(state.Budget)}
		json.Unmarshal([]byte(`{"version":4,"stalled_reviews":3}`), &state.Progress)
		next, err := RunTaskLoop(context.Background(), state)
		want := "failed"
		if partial > 0 {
			want = "partial"
		}
		if err != nil || next.Kind != "finish" || next.Status != want {
			t.Fatalf("kept an unproductive loop alive: %+v %v", next, err)
		}
	}
}

func TestWorkingMemoryChangingQueriesAndNotesDoNotResetStall(t *testing.T) {
	s, r, _ := memoryRun(t, "research public evidence")
	ctx := context.Background()
	for cycle := 0; cycle < 4; cycle++ {
		for n := 0; n < 3; n++ {
			if err := s.Data.DB.Create(&agentdata.Operation{RunID: r.ID, Key: fmt.Sprintf("model-%d-%d", cycle, n), Kind: "model", Stage: "succeeded", ResultJSON: `{}`}).Error; err != nil {
				t.Fatal(err)
			}
		}
		args := fmt.Sprintf(`{"expected_version":%d,"requirements":[{"id":"scope","status":"missing","explanation":"still missing %d","citations":[]}],"notes":["query variation %d"],"next_purpose":"different wording %d"}`, cycle, cycle, cycle, cycle)
		_, err := s.executeDurableTool(ctx, r, ToolCall{ID: fmt.Sprintf("checkpoint-%d", cycle), Type: "function", Function: FunctionCall{Name: "update_task_progress", Arguments: args}})
		if err != nil {
			t.Fatal(err)
		}
		r, err = s.Data.Run(ctx, r.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	var p map[string]any
	json.Unmarshal([]byte(r.ProgressJSON), &p)
	if p["stalled_reviews"] != float64(3) {
		t.Fatalf("cosmetic checkpoint reset stall: %s", r.ProgressJSON)
	}
}

func TestWorkingMemoryNewEvidenceAndCapabilitiesImproveDelivery(t *testing.T) {
	state := decisionState()
	p := TaskProgress{Version: 1, Requirements: []RequirementResult{{ID: "scope", Status: "missing"}}}
	before := deliveryProgressSignature(state, p)
	state.EvidenceCatalog = []map[string]any{{"id": "ev_new", "doc_id": "new-report", "revision": "v1"}}
	if deliveryProgressSignature(state, p) == before {
		t.Fatal("new current evidence ignored")
	}
	state.EvidenceCatalog = nil
	out, _ := toolData(ToolCall{ID: "trial", Function: FunctionCall{Name: "validate_program"}}, map[string]string{"artifact_hash": "validated-new-capability"})
	raw, _ := json.Marshal(out)
	state.Operations = []agentdata.Operation{{Kind: "validate_program", Stage: "succeeded", ResultJSON: string(raw)}}
	if deliveryProgressSignature(state, p) == before {
		t.Fatal("validated capability ignored")
	}
	state.Operations[0].Stage = "failed"
	if deliveryProgressSignature(state, p) != before {
		t.Fatal("a failed attempt masqueraded as delivery progress")
	}
}

func TestWorkingMemoryCapturedFailureRequiresReview(t *testing.T) {
	path := os.Getenv("GORELAY_CONTEXT_REPLAY")
	if path == "" {
		t.Skip("opt-in saved normal-entry run; no model calls")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var captured struct {
		Run        agentdata.Run         `json:"run"`
		Operations []agentdata.Operation `json:"operations"`
		Children   []ChildResult         `json:"children"`
		Progress   TaskProgress          `json:"progress"`
	}
	if err = json.Unmarshal(raw, &captured); err != nil {
		t.Fatal(err)
	}
	state := LoopState{Run: captured.Run, Operations: captured.Operations, TaskResults: captured.Children, Progress: captured.Progress}
	json.Unmarshal([]byte(captured.Run.BudgetJSON), &state.Budget)
	var spec TaskSpec
	json.Unmarshal([]byte(captured.Run.SpecJSON), &spec)
	state.Spec = &spec
	// Replay the new control policy against immutable historical facts; never
	// mutate or resume that terminal run, and never claim a successful live run.
	state.Budget.LoopVersion = 2
	review := taskReview(state)
	if !review.Required || review.UnassessedRounds < 3 {
		t.Fatalf("captured stalled research escaped review: %+v", review)
	}
	t.Logf("saved run: %d successful unassessed decisions, review=%s", review.UnassessedRounds, review.Reason)
}

func TestWorkingMemoryReservesLastDecisionForDelivery(t *testing.T) {
	s, r, p := memoryRun(t, "finish existing evidence")
	var b BudgetSnapshot
	json.Unmarshal([]byte(r.BudgetJSON), &b)
	b.Spent.ModelCalls = b.Limits.ModelCalls - 1
	raw, _ := json.Marshal(b)
	if err := s.Data.DB.Model(&r).Update("budget_json", string(raw)).Error; err != nil {
		t.Fatal(err)
	}
	_, err := s.executeDurableTool(context.Background(), r, ToolCall{ID: "too-late", Type: "function", Function: FunctionCall{Name: "discover_sources", Arguments: `{"query":"extra search"}`}})
	if err == nil || p.calls != 0 {
		t.Fatal("exploration consumed delivery reserve")
	}
}

func TestWorkingMemoryNewPagesPauseStallButRepeatedReadsDoNot(t *testing.T) {
	s, r, _ := memoryRun(t, "read supporting reports")
	ctx := context.Background()
	for cycle := 0; cycle < 4; cycle++ {
		for n := 0; n < 3; n++ {
			if err := s.Data.DB.Create(&agentdata.Operation{RunID: r.ID, Key: fmt.Sprintf("model-%d-%d", cycle, n), Kind: "model", Stage: "succeeded", ResultJSON: `{}`}).Error; err != nil {
				t.Fatal(err)
			}
		}
		if cycle == 1 || cycle == 2 {
			// A repeated page has identical source/pointer/text even with a new call ID.
			out, _ := toolData(ToolCall{ID: fmt.Sprintf("page-%d", cycle), Function: FunctionCall{Name: "read_observation"}}, map[string]any{"receipt_tool_call_id": "original-inspection", "pointer": "/data/body", "text": "new supporting page", "total_runes": 4000, "next_offset": 2000})
			raw, _ := json.Marshal(out)
			if err := s.Data.DB.Create(&agentdata.Operation{RunID: r.ID, Key: fmt.Sprintf("tool:page-%d", cycle), Kind: "read_observation", Stage: "succeeded", ResultJSON: string(raw)}).Error; err != nil {
				t.Fatal(err)
			}
		}
		args := fmt.Sprintf(`{"expected_version":%d,"requirements":[{"id":"scope","status":"missing","explanation":"reading remaining support","citations":[]}],"notes":[],"next_purpose":"read remaining support"}`, cycle)
		if _, err := s.executeDurableTool(ctx, r, ToolCall{ID: fmt.Sprintf("review-%d", cycle), Type: "function", Function: FunctionCall{Name: "update_task_progress", Arguments: args}}); err != nil {
			t.Fatal(err)
		}
		r, _ = s.Data.Run(ctx, r.ID)
	}
	var p map[string]any
	json.Unmarshal([]byte(r.ProgressJSON), &p)
	if p["stalled_reviews"] != float64(2) {
		t.Fatalf("new page counted as stall or duplicate counted as progress: %s", r.ProgressJSON)
	}
}

func TestWorkingMemoryRetrievalDoesNotConsumeReservedDecision(t *testing.T) {
	s, r, _ := memoryRun(t, "deliver with existing knowledge")
	var b BudgetSnapshot
	json.Unmarshal([]byte(r.BudgetJSON), &b)
	b.Spent.ModelCalls = b.Limits.ModelCalls - 1
	raw, _ := json.Marshal(b)
	if err := s.Data.DB.Model(&r).Update("budget_json", string(raw)).Error; err != nil {
		t.Fatal(err)
	}
	// Even an unsuccessful retrieval owns a retrieval attempt, not a decision.
	_, _ = s.executeDurableTool(context.Background(), r, ToolCall{ID: "retrieval", Type: "function", Function: FunctionCall{Name: "search_knowledge", Arguments: `{"query":"current supporting evidence"}`}})
	stored, err := s.Data.Run(context.Background(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	json.Unmarshal([]byte(stored.BudgetJSON), &b)
	if b.Spent.ModelCalls != b.Limits.ModelCalls-1 {
		t.Fatalf("retrieval spent delivery decision: %+v", b.Spent)
	}
}

func TestWorkingMemoryReviewStartsAfterThirdRoundFeedback(t *testing.T) {
	state := decisionState()
	state.Budget.LoopVersion = 2
	state.Spec = &TaskSpec{Goal: "collect requested facts"}
	for n := 0; n < 3; n++ {
		state.Operations = append(state.Operations, agentdata.Operation{Kind: "model", Stage: "succeeded"})
	}
	call := ToolCall{ID: "third-round-action", Type: "function", Function: FunctionCall{Name: "discover_sources", Arguments: `{"query":"third exploration"}`}}
	state.Messages = []Message{{Role: "assistant", ToolCalls: []ToolCall{call}}}
	if taskReview(state).Required {
		t.Fatal("third proposal rejected before its feedback; model saw only two completed rounds")
	}
	state.Messages = append(state.Messages, Message{Role: "tool", ToolCallID: call.ID, Content: `{"name":"discover_sources","data":{"candidates":[]}}`})
	if !taskReview(state).Required {
		t.Fatal("fourth decision escaped review")
	}
}

func TestWorkingMemoryDeliveryReserveUsesProposalDecisionSnapshot(t *testing.T) {
	state := decisionState()
	state.Budget.LoopVersion = 2
	state.Spec = &TaskSpec{Goal: "complete public acquisition"}
	state.Messages = []Message{{Role: "assistant", ToolCalls: []ToolCall{{ID: "pending", Function: FunctionCall{Name: "discover_sources"}}}}}
	state.Budget.Spent.ModelCalls = state.Budget.Limits.ModelCalls - 1
	if taskReview(state).DeliveryRequired {
		t.Fatal("second-last decision was allowed acquisition, then unexpectedly rejected before feedback")
	}
	state.Budget.Spent.ModelCalls = state.Budget.Limits.ModelCalls
	if !taskReview(state).DeliveryRequired {
		t.Fatal("last decision allowed new acquisition without a delivery reserve")
	}
}
