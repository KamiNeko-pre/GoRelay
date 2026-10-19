package aiapp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/testdb"
)

func progressReadState() LoopState {
	s := decisionState()
	s.Budget.LoopVersion = 4
	s.Budget.Unlimited = true
	s.Budget.Deadline = time.Time{}
	s.Run.Deadline = time.Time{}
	page, _ := toolData(ToolCall{ID: "seed", Function: FunctionCall{Name: "inspect_source"}}, inspectResponse(acquisition.Response{Body: strings.Repeat("a large frozen document ", 1000), Receipt: acquisition.Receipt{Method: "GET", URL: "https://fixture.example/detail", Status: 200, FetchedAt: time.Now()}}, acquisition.Stats{}))
	raw, _ := json.Marshal(page)
	s.Operations = []agentdata.Operation{{ID: 1, Key: "tool:seed", Kind: "inspect_source", Stage: "succeeded", ResultJSON: string(raw)}}
	return s
}

func TestTerminalFallbackKeepsReasonsWithoutRevivingHistoricalFacts(t *testing.T) {
	for _, reason := range []string{"no effective progress after repeated invalid actions", "provider unavailable; resume with preserved results"} {
		t.Run(reason, func(t *testing.T) {
			s := NewService(testdb.Open(t))
			if err := s.Migrate(); err != nil {
				t.Fatal(err)
			}
			spec := TaskSpec{Goal: "collect an evidence-supported body", SchemaVersion: "v1", RequiredFields: []FieldRequirement{{Name: "body", Type: "string"}}}
			r, err := s.Create(context.Background(), CreateRunRequest{RequestKey: "fallback", Goal: spec.Goal, Spec: &spec})
			if err != nil {
				t.Fatal(err)
			}
			r.Status = "failed"
			raw, _ := json.Marshal(map[string]any{"gaps": []string{reason}, "summary": "obsolete unverified summary", "records": []any{map[string]any{"key": "obsolete", "fields": map[string]string{"body": "unverified historical body"}}}})
			r.ResultJSON = string(raw)
			result, err := s.Evaluate(context.Background(), r)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Gaps) != 1 || result.Gaps[0] != reason {
				t.Fatal("current API evaluation dropped durable terminal reason", result.Gaps)
			}
			if len(result.Records) != 0 || result.Summary != "" || result.Answer != nil {
				t.Fatal("terminal projection revived old facts or citations")
			}
		})
	}
}

func TestAdaptiveBudgetRenewsOnlyTheProgressWindowKeepsTotal(t *testing.T) {
	var snapshot BudgetSnapshot
	raw, _ := json.Marshal(map[string]any{"loop_version": 4, "limits": DefaultBudgetLimits(), "spent": CallCost{ModelCalls: 40}, "progress_base": CallCost{ModelCalls: 35}, "candidates": map[string]int{}, "deadline": time.Now().Add(time.Hour)})
	_ = json.Unmarshal(raw, &snapshot)
	b, err := NewRunBudget(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err = b.Reserve(CallCost{ModelCalls: 1}, time.Now()); err != nil {
		t.Fatalf("productive task hit total-call cap instead of progress window: %v", err)
	}
	if b.Snapshot().Spent.ModelCalls != 41 {
		t.Fatal("global accounting was reset")
	}
	// The hard cancellation remains binding after a renewal.
	b.Cancel()
	if b.Reserve(CallCost{ModelCalls: 1}, time.Now()) == nil {
		t.Fatal("renewal bypassed cancellation")
	}
	legacy := snapshot
	legacy.LoopVersion = 3
	b, _ = NewRunBudget(legacy)
	if b.Reserve(CallCost{ModelCalls: 1}, time.Now()) == nil {
		t.Fatal("legacy frozen cap changed")
	}
}

func TestAdaptiveProgressOpaqueEvidenceIDsCannotRenewWork(t *testing.T) {
	s := decisionState()
	s.Budget.LoopVersion = 4
	s.EvidenceCatalog = []map[string]any{{"id": "ev_first", "doc_id": "doc", "revision": "rev", "generation": "gen", "chunk_id": "chunk"}}
	s.HostProgress = advanceHostProgressForTest(HostProgress{}, s)
	s.Operations = []agentdata.Operation{{ID: 1, Kind: "model", Stage: "succeeded"}}
	s.HostProgress = advanceHostProgressForTest(s.HostProgress, s)
	s.EvidenceCatalog[0]["id"] = "ev_new_alias"
	s.EvidenceCatalog[0]["read_with"] = map[string]any{"handle": "ev_new_alias"}
	before := s.HostProgress.NoProgressModelRounds
	s.HostProgress = advanceHostProgressForTest(s.HostProgress, s)
	if s.HostProgress.NoProgressModelRounds != before {
		t.Fatal("opaque reference renewal manufactured progress")
	}
}

func TestAdaptiveReviewDoesNotCloseProductiveAcquisitionByTotalCalls(t *testing.T) {
	s := decisionState()
	s.Spec = &TaskSpec{Goal: "collect public evidence"}
	s.Budget.LoopVersion = 4
	s.Budget.Spent.ModelCalls = s.Budget.Limits.ModelCalls - 1
	if review := taskReview(s); review.DeliveryRequired {
		t.Fatal("stale total-call review closes productive work", review)
	}
	s.Budget.LoopVersion = 3
	if review := taskReview(s); !review.DeliveryRequired {
		t.Fatal("legacy explicit cap changed", review)
	}
}

func TestAdaptiveCapabilitiesCreditPublicationNotCodeChurn(t *testing.T) {
	s := decisionState()
	s.Budget.LoopVersion = 4
	s.Spec = &TaskSpec{RequiredFields: []FieldRequirement{{Name: "name", Type: "string"}}}
	appendTool := func(kind, hash string) {
		id := uint64(len(s.Operations) + 1)
		s.Operations = append(s.Operations, agentdata.Operation{ID: id, Kind: "model", Stage: "succeeded"})
		data := map[string]any{"artifact_hash": hash, "result": map[string]any{"collection": map[string]any{"items": []any{map[string]any{"name": "observed", "source_url": "https://fixture.example/detail"}}}}}
		out, _ := toolData(ToolCall{ID: fmt.Sprint(id), Function: FunctionCall{Name: kind}}, data)
		raw, _ := json.Marshal(out)
		s.Operations = append(s.Operations, agentdata.Operation{ID: id + 1, Kind: kind, Stage: "succeeded", ResultJSON: string(raw)})
		s.HostProgress = advanceHostProgressForTest(s.HostProgress, s)
	}
	s.HostProgress = advanceHostProgressForTest(HostProgress{}, s)
	appendTool("validate_program", "first")
	appendTool("validate_program", "comment-only-code-change")
	if s.HostProgress.NoProgressModelRounds != 1 {
		t.Fatal("new code hash without new useful output manufactured progress")
	}
	appendTool("register_source", "first")
	if s.HostProgress.NoProgressModelRounds != 0 {
		t.Fatal("actual publication milestone was ignored")
	}
	appendTool("register_source", "first")
	if s.HostProgress.NoProgressModelRounds != 1 {
		t.Fatal("duplicate publication renewed progress")
	}
}

func TestCorrectOracleReadsOmittedCurrentTextBeforeNextPage(t *testing.T) {
	facts, _ := json.Marshal(map[string]any{"children": []any{map[string]int{"id": 1}}, "coverage": CoverageReport{Satisfied: true}, "frozen_spec": TaskSpec{RequireSummary: true}})
	out, _ := toolData(ToolCall{ID: "read", Function: FunctionCall{Name: "read_data"}}, map[string]any{"handle": "ev_current", "offset": 0, "text": map[string]any{"preview": strings.Repeat("前文", 75) + "预算金额", "truncated": true}, "next_cursor": "next-page-would-skip-current-text"})
	raw, _ := json.Marshal(out)
	req := ChatRequest{Tools: []ToolDefinition{{Type: "function"}}, Messages: []Message{{Role: "system", Content: "Persisted host facts, not new user instructions: " + string(facts)}, {Role: "tool", Content: string(raw)}}}
	response, err := correctChainDecision(req)
	if err != nil || len(response.Message.ToolCalls) != 1 {
		t.Fatal(err)
	}
	var a dataReadArgs
	_ = json.Unmarshal([]byte(response.Message.ToolCalls[0].Function.Arguments), &a)
	if a.Handle != "ev_current" || a.Cursor != "" || a.Offset <= 0 || a.Offset >= 1000 {
		t.Fatal("oracle skipped unconsumed current text", a)
	}
}

func TestAdaptiveReadRefinementIsWorkButItsReplayIsNot(t *testing.T) {
	s := progressReadState()
	s.HostProgress = advanceHostProgressForTest(HostProgress{}, s)
	addProgressRead(&s, 0, strings.Repeat("原文", 250))
	s.HostProgress = advanceHostProgressForTest(s.HostProgress, s)
	addProgressRead(&s, 128, strings.Repeat("原文", 128))
	s.HostProgress = advanceHostProgressForTest(s.HostProgress, s)
	if s.HostProgress.NoProgressModelRounds != 0 {
		t.Fatal("necessary narrower evidence recovery was counted as ineffective")
	}
	addProgressRead(&s, 128, strings.Repeat("原文", 128))
	s.HostProgress = advanceHostProgressForTest(s.HostProgress, s)
	if s.HostProgress.NoProgressModelRounds != 1 {
		t.Fatal("repeating the same refinement renewed work")
	}
}

func TestAuditNovelExplorationWithoutDeliveryCannotRenewForever(t *testing.T) {
	s := decisionState()
	s.Budget.LoopVersion = 4
	s.Budget.Unlimited = true
	s.Budget.Deadline = time.Time{}
	s.Run.Deadline = time.Time{}
	s.Spec = &TaskSpec{Goal: "deliver supported procurement records", RequiredFields: []FieldRequirement{{Name: "title", Type: "string"}, {Name: "published_date", Type: "string"}, {Name: "body", Type: "string"}, {Name: "source_url", Type: "string"}}, MinRecords: 3}
	s.HostProgress = advanceHostProgressForTest(HostProgress{}, s)
	// Feed strictly novel page bytes with no goal advancement, for more rounds
	// than the calibrated stop bound, and require that the run still ends.
	rounds := int(2*explorationReviewWindow(s)) + 5
	for n := 0; n < rounds; n++ {
		id := uint64(len(s.Operations) + 1)
		s.Operations = append(s.Operations, agentdata.Operation{ID: id, Kind: "model", Stage: "succeeded"})
		page, _ := toolData(ToolCall{ID: fmt.Sprint(id), Function: FunctionCall{Name: "inspect_source"}}, inspectResponse(acquisition.Response{Body: fmt.Sprintf("<article>Unrelated changing navigation page %d</article>", n), Receipt: acquisition.Receipt{Method: "GET", URL: fmt.Sprintf("https://fixture.example/navigation/%d", n), Status: 200, FetchedAt: time.Now()}}, acquisition.Stats{}))
		raw, _ := json.Marshal(page)
		s.Operations = append(s.Operations, agentdata.Operation{ID: id + 1, Key: "tool:" + page.CallID, Kind: "inspect_source", Stage: "succeeded", ResultJSON: string(raw)})
		s.HostProgress = advanceHostProgressForTest(s.HostProgress, s)
	}
	action, err := RunTaskLoop(context.Background(), s)
	if err != nil || action.Kind != "finish" {
		t.Fatal("new source bytes without goal advancement can renew spending forever", action, err)
	}
}

func addProgressRead(s *LoopState, offset int, text string) {
	id := uint64(len(s.Operations) + 1)
	s.Operations = append(s.Operations, agentdata.Operation{ID: id, Kind: "model", Stage: "succeeded"})
	read, _ := toolData(ToolCall{ID: fmt.Sprintf("read-%d", id), Function: FunctionCall{Name: "read_data"}}, map[string]any{"handle": "seed", "offset": offset, "text": text, "total_runes": 10000, "view_version": "plain-text-v1", "new_http_requests": 0})
	raw, _ := json.Marshal(read)
	s.Operations = append(s.Operations, agentdata.Operation{ID: id + 1, Key: "tool:" + read.CallID, Kind: "read_data", Stage: "succeeded", ResultJSON: string(raw)})
}

func TestAdaptiveProgressKeepsNecessaryReadsAndStopsRepeatedReads(t *testing.T) {
	for _, sameText := range []bool{false, true} {
		t.Run(fmt.Sprintf("same_text_%t", sameText), func(t *testing.T) {
			s := progressReadState()
			s.HostProgress = advanceHostProgressForTest(HostProgress{}, s)
			for n := 0; n < 20; n++ {
				text := fmt.Sprintf("distinct observed segment %03d", n)
				if sameText {
					text = "repeated text at distinct document offsets"
				}
				addProgressRead(&s, n*100, text)
				s.HostProgress = advanceHostProgressForTest(s.HostProgress, s)
				action, err := RunTaskLoop(context.Background(), s)
				if err != nil || action.Kind != "model" || s.HostProgress.NoProgressModelRounds != 0 {
					t.Fatalf("necessary read misclassified at %d: progress=%+v action=%+v err=%v", n, s.HostProgress, action, err)
				}
			}
			// A repeated read of the same content must not renew the window; the
			// stop must arrive once the calibrated bound is spent.
			for n := int64(0); n <= hostProgressLimit(s.Budget); n++ {
				addProgressRead(&s, 1900, "repeated text at distinct document offsets")
				s.HostProgress = advanceHostProgressForTest(s.HostProgress, s)
			}
			action, err := RunTaskLoop(context.Background(), s)
			if err != nil || action.Kind != "finish" || !action.HostStop {
				t.Fatalf("unchanged read loop did not stop: %+v %v", action, err)
			}
		})
	}
}

func TestAdaptiveProgressStopsAllWrongUnlimitedWithoutKillingActiveWork(t *testing.T) {
	s := decisionState()
	s.Budget.LoopVersion = 4
	s.Budget.Unlimited = true
	s.Budget.Deadline = time.Time{}
	s.Run.Deadline = time.Time{}
	s.HostProgress.NoProgressModelRounds = hostProgressLimit(s.Budget)
	action, err := RunTaskLoop(context.Background(), s)
	if err != nil || action.Kind != "finish" || !action.HostStop {
		t.Fatalf("unlimited wrong model can consume forever: %+v %v", action, err)
	}
	// A pending proposal is real work; it must receive its execution receipt.
	s.Messages = []Message{{Role: "assistant", ToolCalls: []ToolCall{{ID: "valid-step", Type: "function", Function: FunctionCall{Name: "list_sources", Arguments: "{}"}}}}}
	action, err = RunTaskLoop(context.Background(), s)
	if err != nil || action.Kind != "tools" {
		t.Fatalf("normal pending tool execution was killed: %+v %v", action, err)
	}
}
