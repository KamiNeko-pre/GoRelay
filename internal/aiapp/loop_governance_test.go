package aiapp

import (
	"context"
	"encoding/json"
	"fmt"
	"go-lua-crawler/internal/agentdata"
	"strings"
	"testing"
	"time"
)

func cycleOperations(rounds int, productive bool) []agentdata.Operation {
	ops := []agentdata.Operation{}
	for i := 0; i < rounds; i++ {
		ops = append(ops, agentdata.Operation{Kind: "model", Stage: "succeeded"})
		input := fmt.Sprint(i % 2)
		text := "unchanged"
		if productive {
			text = fmt.Sprintf("page %d", i)
			input = fmt.Sprint(i)
		}
		raw, _ := json.Marshal(ToolResult{CallID: fmt.Sprint(i), Name: "read_data", Data: json.RawMessage(fmt.Sprintf(`{"text":%q}`, text))})
		ops = append(ops, agentdata.Operation{Kind: "read_data", Stage: "succeeded", InputHash: input, ResultJSON: string(raw)})
	}
	return ops
}

func TestAlternatingUnchangedCallsConvergeWithoutKillingPaging(t *testing.T) {
	for _, tc := range []struct {
		name       string
		productive bool
		rounds     int
		stop       bool
	}{{"AB-cycle", false, 6, true}, {"paging", true, 8, false}, {"second-cycle-can-correct", false, 4, false}} {
		t.Run(tc.name, func(t *testing.T) {
			state := decisionState()
			state.Budget.LoopVersion = 4
			state.Operations = cycleOperations(tc.rounds, tc.productive)
			action, err := RunTaskLoop(context.Background(), state)
			if err != nil || (action.Kind == "finish") != tc.stop {
				t.Fatalf("cycle governance: %+v %v", action, err)
			}
		})
	}
	// New results for the same inputs are real information, not an unchanged cycle.
	state := decisionState()
	state.Budget.LoopVersion = 4
	state.Operations = cycleOperations(6, false)
	raw, _ := json.Marshal(ToolResult{Name: "read_data", Data: json.RawMessage(`{"text":"recovered source"}`)})
	state.Operations[len(state.Operations)-1].ResultJSON = string(raw)
	action, err := RunTaskLoop(context.Background(), state)
	if err != nil || action.Kind == "finish" {
		t.Fatalf("changed source was suppressed: %+v %v", action, err)
	}
}

func TestNewReadArgumentsDoNotPretendUnchangedResultsAreProgress(t *testing.T) {
	state := decisionState()
	state.Budget.LoopVersion = 4
	state.Operations = cycleOperations(3, false)
	for i := range state.Operations {
		if state.Operations[i].Kind == "read_data" {
			state.Operations[i].InputHash = fmt.Sprint("new-query-", i)
			raw, _ := json.Marshal(ToolResult{Name: "read_data", Data: json.RawMessage(`{"offset":0,"view_version":"html-information-v2","text":"same located window"}`)})
			state.Operations[i].ResultJSON = string(raw)
		}
	}
	action, err := RunTaskLoop(context.Background(), state)
	if err != nil || action.Kind != "finish" {
		t.Fatalf("argument churn concealed unchanged information: %+v %v", action, err)
	}
	// Different proposed corrections that still fail have not executed a
	// successful read; they remain distinct attempts with their own diagnostics.
	for i := range state.Operations {
		if state.Operations[i].Kind == "read_data" {
			state.Operations[i].Stage = "failed"
			raw, _ := json.Marshal(ToolResult{Name: "read_data", Error: "invalid selector"})
			state.Operations[i].ResultJSON = string(raw)
		}
	}
	action, err = RunTaskLoop(context.Background(), state)
	if err != nil || action.Kind == "finish" {
		t.Fatal("new corrective proposals mistaken for identical read results")
	}
}

func TestStableSourceReadRestoresOnlyAfterCompaction(t *testing.T) {
	s, run, _ := storedHTMLReader(t, `<article>`+strings.Repeat("<p>stable source</p>", 50)+`</article>`)
	ctx := context.Background()
	run, err := s.Data.Claim(ctx, run.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	call := ToolCall{ID: "first-read", Type: "function", Function: FunctionCall{Name: "read_data", Arguments: `{"handle":"source","view":"dom","selector":"article > p","limit":20}`}}
	first, err := s.executeDurableTool(ctx, run, call)
	if err != nil {
		t.Fatal(err)
	}
	call.ID = "renamed-read"
	out, err := s.executeDurableToolWithVisibility(ctx, run, call, visibleReceipts{first.CallID: true})
	if err == nil || out.Failure == nil || out.Failure.Code != "DUPLICATE_TOOL_CALL" {
		t.Fatal("visible receipt repeat was not refused")
	}
	call.ID = "restored-read"
	out, err = s.executeDurableToolWithVisibility(ctx, run, call, visibleReceipts{})
	if err != nil || out.ReplayOf != "tool:"+first.CallID || string(out.Data) != string(first.Data) {
		t.Fatal("compaction did not restore the saved source receipt", err)
	}
	call.ID = "new-source-range"
	call.Function.Arguments = `{"handle":"source","view":"dom","selector":"article > p","offset":20,"limit":20,"view_version":"dom-structure-v1"}`
	if _, err := s.executeDurableTool(ctx, run, call); err != nil {
		t.Fatal("different source range was blocked:", err)
	}
}
func TestAlternatingReviewLoopDeliversPartialThroughNormalEntry(t *testing.T) {
	runCorrectModelSubsystemChain(t, true, 1, false, "", progressiveCase{Name: "alternating-review", Fault: "alternating_review", WrongPercent: 100, AfterAcquisition: true, Expected: "partial"})
}
