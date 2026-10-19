package aiapp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/knowledge"
	"go-lua-crawler/internal/testdb"
)

func TestRootCausePublishedReadsUseTheirOwnRepresentation(t *testing.T) {
	for _, tag := range []string{"article", "section"} {
		t.Run(tag, func(t *testing.T) {
			body := `<html><body><` + tag + `><h1>Source title</h1><p>` + strings.Repeat("Public conditions and evidence. ", 500) + `</p></` + tag + `></body></html>`
			s, r, original := storedHTMLReader(t, body)
			var receipt ToolResult
			if err := json.Unmarshal([]byte(original), &receipt); err != nil {
				t.Fatal(err)
			}
			projected, ok := locatedInspectionObservation(receipt)
			if !ok {
				t.Fatal("missing HTML projection")
			}
			var view struct {
				Next        *dataReadArgs  `json:"next_read"`
				Structure   map[string]any `json:"read_structure_with"`
				Information struct {
					Blocks []struct {
						ReadWith map[string]any `json:"read_with"`
					} `json:"blocks"`
				} `json:"information"`
			}
			if err := json.Unmarshal(projected.Data, &view); err != nil {
				t.Fatal(err)
			}
			read := func(label string, args any) {
				t.Helper()
				raw, _ := json.Marshal(args)
				if _, err := s.readData(context.Background(), r, ToolCall{ID: label, Type: "function", Function: FunctionCall{Name: "read_data", Arguments: string(raw)}}); err != nil {
					t.Errorf("host published unusable %s: %s: %v", label, raw, err)
				}
			}
			read("structure", view.Structure)
			if view.Next != nil {
				read("continuation", view.Next)
			}
			if len(view.Information.Blocks) != 0 {
				t.Fatal("crawler inspection exposed prose block readers")
			}
		})
	}
}

// Mirrors the receipt Service.think persists, including the HOST-owned phase
// actually sent to the model; the model cannot grant itself a recovery attempt.
func recordedModelRound(state LoopState, id uint64) agentdata.Operation {
	raw, _ := json.Marshal(ChatResponse{Message: Message{Role: "assistant"}, DeliveryRecoveryPhase: state.HostProgress.Recovery.Phase})
	return agentdata.Operation{ID: id, Kind: "model", Stage: "succeeded", ResultJSON: string(raw)}
}

func TestRootCauseTraceReceiptIdentity(t *testing.T) {
	s := progressReadState()
	s.Budget.LoopVersion = 5
	s.HostProgress = advanceHostProgressForTest(HostProgress{}, s)
	handle, pointer, version := "seed", "", "plain-text-v1"
	for n := 0; n < 25; n++ {
		id := uint64(len(s.Operations) + 1)
		s.Operations = append(s.Operations, recordedModelRound(s, id))
		callID := fmt.Sprint(id)
		out, _ := toolData(ToolCall{ID: callID, Function: FunctionCall{Name: "read_data"}}, map[string]any{"handle": handle, "url": "https://fixture.example/detail", "pointer": pointer, "view_version": version, "offset": 0, "text": "same source text forever"})
		raw, _ := json.Marshal(out)
		s.Operations = append(s.Operations, agentdata.Operation{ID: id + 1, Key: "tool:" + callID, Kind: "read_data", Stage: "succeeded", ResultJSON: string(raw)})
		s.HostProgress = advanceHostProgressForTest(s.HostProgress, s)
		if n < 4 || n == 24 {
			t.Logf("round=%d ranges=%v idle=%d phase=%s attempts=%d delivery=%s", n, consumedReadRanges(s), s.HostProgress.NoProgressModelRounds, s.HostProgress.Recovery.Phase, s.HostProgress.Recovery.Attempts, deliveryInformationSignature(s))
		}
		handle, pointer, version = callID, "/text", "receipt-json-v1"
	}
	action, err := RunTaskLoop(context.Background(), s)
	if err != nil || action.Kind != "finish" {
		t.Fatalf("receipt identities kept the run active: %+v %v", action, err)
	}
}

func TestRootCauseCrawlerKeepsDiscoveryAndAcquisitionDelivery(t *testing.T) {
	t.Setenv("GORELAY_FULL_TOOLSET", "0")
	state := decisionState()
	state.Budget.LoopVersion = 5
	state.Run.BudgetJSON = `{"loop_version":5}`
	state.Spec = &TaskSpec{Goal: "investigate and revisit", SchemaVersion: "v1", RequiredFields: []FieldRequirement{{Name: "title", Type: "string"}}, RequireSummary: true}
	raw, _ := json.Marshal(state.Spec)
	state.Run.SpecJSON = string(raw)
	s := &Service{Retriever: &knowledge.Retriever{}}
	have := map[string]bool{}
	for _, d := range s.configuredToolDefinitionsForState(state) {
		have[d.Function.Name] = true
	}
	for _, name := range []string{"inspect_source", "read_data", "list_sources", "validate_program", "register_source", "submit_acquisition", "get_task_results", "finish_task"} {
		if !have[name] {
			t.Errorf("task capability %s is unreachable", name)
		}
	}
}

func TestRootCauseRepairMetadataIsNotBusinessInput(t *testing.T) {
	a := toolCallKey("read_data", `{"handle":"source","view":"dom","selector":"article","repair_of":"first-error"}`)
	b := toolCallKey("read_data", `{"repair_of":"later-error","selector":"article","view":"dom","handle":"source"}`)
	c := toolCallKey("read_data", `{"handle":"source","view":"dom","selector":"section"}`)
	if a != b {
		t.Fatal("diagnostic call IDs changed the business identity")
	}
	if a == c {
		t.Fatal("different selectors collided")
	}
}

func TestRootCauseCanonicalInputPreservesLargeBusinessNumbers(t *testing.T) {
	a := toolCallKey("query", `{"id":9007199254740992}`)
	b := toolCallKey("query", `{"id":9007199254740993}`)
	if a == b {
		t.Fatal("distinct business numbers collapsed through float64")
	}
	for i := 0; i < 3; i++ {
		if a != toolCallKey("query", ` { "id" : 9007199254740992 } `) {
			t.Fatal("business identity depends on external time or JSON whitespace")
		}
	}
}

func TestRootCauseConcurrentCallIDsExecuteBusinessOnce(t *testing.T) {
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r, err := s.Create(ctx, CreateRunRequest{RequestKey: "concurrent-intent", Goal: "collect public titles"})
	if err != nil {
		t.Fatal(err)
	}
	r, err = s.Data.Claim(ctx, r.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = s.executeDurableToolWithVisibility(ctx, r, ToolCall{ID: fmt.Sprintf("concurrent-%d", i), Type: "function", Function: FunctionCall{Name: "plan_task", Arguments: `{"required_fields":[{"name":"title","type":"string"}]}`}}, visibleReceipts{})
		}(i)
	}
	wg.Wait()
	current, err := s.Data.Run(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	var budget BudgetSnapshot
	json.Unmarshal([]byte(current.BudgetJSON), &budget)
	if budget.Spent.ToolCalls != 1 {
		t.Fatalf("concurrent proposals executed %d business operations", budget.Spent.ToolCalls)
	}
}

func TestRootCausePreflightRejectionUsesTheSameBusinessGuard(t *testing.T) {
	s, r, _ := storedHTMLReader(t, `<article><h1>Title</h1></article>`)
	ctx := context.Background()
	r, err := s.Data.Claim(ctx, r.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	call := ToolCall{ID: "invalid-first", Type: "function", Function: FunctionCall{Name: "inspect_source", Arguments: `{"url":42}`}}
	first, err := s.executeDurableTool(ctx, r, call)
	if err == nil || first.Failure == nil {
		t.Fatal("expected preflight rejection")
	}
	call.ID = "invalid-renamed"
	again, err := s.executeDurableToolWithVisibility(ctx, r, call, visibleReceipts{first.CallID: true})
	if err == nil || again.Failure == nil || again.Failure.Code != "DUPLICATE_TOOL_CALL" {
		t.Fatalf("preflight bypassed semantic guard: %+v %v", again, err)
	}
	call.ID = "invalid-restored"
	again, err = s.executeDurableToolWithVisibility(ctx, r, call, visibleReceipts{})
	if err == nil || again.Failure == nil || again.Failure.Code != first.Failure.Code {
		t.Fatal("lost original preflight diagnosis")
	}
}

func TestRootCauseBusinessReplaySurvivesCallIDsAndCompaction(t *testing.T) {
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r, err := s.Create(ctx, CreateRunRequest{RequestKey: "host-business-replay", Goal: "collect public titles"})
	if err != nil {
		t.Fatal(err)
	}
	r, err = s.Data.Claim(ctx, r.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	spec := TaskSpec{Goal: r.Goal, SchemaVersion: "v1", RequiredFields: []FieldRequirement{{Name: "title", Type: "string"}}}
	args, _ := json.Marshal(spec)
	call := ToolCall{ID: "model-first", Type: "function", Function: FunctionCall{Name: "plan_task", Arguments: string(args)}}
	first, err := s.executeDurableTool(ctx, r, call)
	if err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"model-first", "new-model-id", "after-compaction"} {
		recovered := NewService(s.Data.DB)
		call.ID = id
		visible := visibleReceipts{}
		if i != 2 {
			visible[first.CallID] = true
		}
		got, e := recovered.executeDurableToolWithVisibility(ctx, r, call, visible)
		if i == 1 {
			if e == nil || got.Failure == nil || got.Failure.Code != "DUPLICATE_TOOL_CALL" {
				t.Fatalf("new call ID bypassed visible business receipt: %+v %v", got, e)
			}
			continue
		}
		if e != nil {
			t.Errorf("same business action rejected for %s: %v", id, e)
			continue
		}
		if got.CallID != id || string(got.Data) != string(first.Data) {
			t.Errorf("reply not paired or canonical result changed: %+v", got)
		}
	}
	call.ID = "after-restored-receipt"
	got, e := s.executeDurableToolWithVisibility(ctx, r, call, visibleReceipts{"after-compaction": true})
	if e == nil || got.Failure == nil || got.Failure.Code != "DUPLICATE_TOOL_CALL" {
		t.Fatalf("restored receipt visibility did not prevent repetition: %+v %v", got, e)
	}
	current, err := s.Data.Run(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	var budget BudgetSnapshot
	json.Unmarshal([]byte(current.BudgetJSON), &budget)
	if budget.Spent.ToolCalls != 1 {
		t.Errorf("business replay charged %d tool executions", budget.Spent.ToolCalls)
	}
	call.ID = "changed-contract"
	call.Function.Arguments = `{"required_fields":[{"name":"other","type":"string"}]}`
	if _, e := s.executeDurableTool(ctx, r, call); e == nil {
		t.Fatal("different contract incorrectly replayed")
	}
}

func TestRootCauseRepeatedErrorRetainsTheActionableCause(t *testing.T) {
	s, r, _ := storedHTMLReader(t, `<main><h2>A supported title</h2><p>Some source facts</p></main>`)
	ctx := context.Background()
	r, err := s.Data.Claim(ctx, r.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	call := ToolCall{ID: "selector-error-1", Type: "function", Function: FunctionCall{Name: "read_data", Arguments: `{"handle":"source","view":"dom","selector":".absent"}`}}
	first, err := s.executeDurableTool(ctx, r, call)
	if err == nil || first.Failure == nil {
		t.Fatal("fixture did not produce a structured selector error")
	}
	call.ID = "selector-error-2"
	again, e := s.executeDurableToolWithVisibility(ctx, r, call, visibleReceipts{first.CallID: true})
	if e == nil || again.Failure == nil || again.Failure.Code != "DUPLICATE_TOOL_CALL" {
		t.Fatalf("visible failed action was not refused: %+v %v", again, e)
	}
	raw, _ := json.Marshal(again.Failure.Details)
	var details struct {
		Original ToolResult `json:"original_receipt"`
	}
	json.Unmarshal(raw, &details)
	if details.Original.Error != first.Error || details.Original.Failure == nil || details.Original.Failure.Code != first.Failure.Code {
		t.Fatalf("duplicate refusal lost actionable original cause: %s", raw)
	}
	call.ID = "error-after-compaction"
	restored, e := s.executeDurableToolWithVisibility(ctx, r, call, visibleReceipts{})
	if e == nil || restored.Error != first.Error || restored.Failure == nil || restored.Failure.Code != first.Failure.Code {
		t.Fatalf("compaction did not restore original failure: %+v %v", restored, e)
	}
}
