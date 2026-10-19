package aiapp

import (
	"context"
	"encoding/json"
	"go-lua-crawler/internal/agentdata"
	"strings"
	"testing"
	"time"
)

func TestDeliveryExplicitRepairResolvesOnlyReferencedSameTarget(t *testing.T) {
	s, r, _ := storedHTMLReader(t, `<main><h1 id="heading">A</h1></main>`)
	r, err := s.Data.Claim(context.Background(), r.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	failed := ToolCall{ID: "failed-read", Type: "function", Function: FunctionCall{Name: "read_data", Arguments: `{"handle":"source","view":"dom","selector":".absent"}`}}
	if _, err = s.executeDurableTool(context.Background(), r, failed); err == nil {
		t.Fatal("expected failure")
	}
	correction := ToolCall{ID: "corrected-read", Type: "function", Function: FunctionCall{Name: "read_data", Arguments: `{"handle":"source","view":"dom","selector":"#heading","repair_of":"failed-read"}`}}
	reply, err := s.executeDurableTool(context.Background(), r, correction)
	if err != nil {
		t.Fatalf("valid explicit correction rejected: %v", err)
	}
	ops, err := s.Data.Operations(context.Background(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := unresolvedToolFailures(LoopState{Run: r, Operations: ops}); len(got) != 0 {
		t.Fatalf("successful changed parameters did not resolve referenced error: %+v", got)
	}
	var raw map[string]any
	_ = json.Unmarshal(mustJSON(reply), &raw)
	if raw["repair_of"] != "failed-read" {
		t.Fatal("repair relation not persisted")
	}
	// A read of a different target must not erase this source's diagnostic.
	if _, err = s.executeDurableTool(context.Background(), r, ToolCall{ID: "failed-again", Type: "function", Function: FunctionCall{Name: "read_data", Arguments: failed.Function.Arguments}}); err == nil {
		t.Fatal("expected second failure")
	}
	wrong := correction
	wrong.ID = "wrong-target"
	wrong.Function.Arguments = `{"handle":"another-source","view":"dom","repair_of":"failed-again"}`
	out, err := s.executeDurableTool(context.Background(), r, wrong)
	if err == nil || out.Failure == nil || !strings.Contains(out.Error, "target") {
		t.Fatalf("cross target repair was not refused: %+v %v", out, err)
	}
}

func TestDeliveryRepairParameterIsPublic(t *testing.T) {
	for _, def := range AgentToolDefinitions() {
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		_ = json.Unmarshal(def.Function.Parameters, &schema)
		if schema.Properties["repair_of"] == nil {
			t.Fatalf("repair_of missing from %s schema", def.Function.Name)
		}
	}
}

func TestDeliveryFailedReaderReceiptCanBeReadForRecovery(t *testing.T) {
	s, r, _ := storedHTMLReader(t, `<main><h1 id="heading">A</h1></main>`)
	r, err := s.Data.Claim(context.Background(), r.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	failed := ToolCall{ID: "reader-error", Type: "function", Function: FunctionCall{Name: "read_data", Arguments: `{"handle":"source","view":"dom","selector":".absent"}`}}
	if _, err = s.executeDurableTool(context.Background(), r, failed); err == nil {
		t.Fatal("expected selector error")
	}
	read := ToolCall{ID: "read-error", Type: "function", Function: FunctionCall{Name: "read_data", Arguments: `{"handle":"reader-error","view":"receipt"}`}}
	out, err := s.executeDurableTool(context.Background(), r, read)
	if err != nil || !strings.Contains(string(out.Data), "SOURCE_SELECTOR_NOT_FOUND") {
		t.Fatalf("stored diagnostic unreadable: %s %v", out.Data, err)
	}
}

func TestDeliveryFailedCorrectionRemainsLinkedUntilSuccess(t *testing.T) {
	s, r, _ := storedHTMLReader(t, `<main><h1 id="heading">A</h1></main>`)
	r, err := s.Data.Claim(context.Background(), r.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for i, args := range []string{
		`{"handle":"source","view":"dom","selector":".absent"}`,
		`{"handle":"source","view":"dom","limit":3000,"repair_of":"chain-a"}`,
		`{"handle":"source","view":"dom","selector":"#heading","repair_of":"chain-b"}`,
	} {
		id := []string{"chain-a", "chain-b", "chain-c"}[i]
		_, err = s.executeDurableTool(context.Background(), r, ToolCall{ID: id, Type: "function", Function: FunctionCall{Name: "read_data", Arguments: args}})
		if (err == nil) != (i == 2) {
			t.Fatalf("unexpected attempt %d: %v", i, err)
		}
	}
	ops, err := s.Data.Operations(context.Background(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := unresolvedToolFailures(LoopState{Run: r, Operations: ops}); len(got) != 0 {
		t.Fatalf("successful correction did not resolve ancestor failures: %+v", got)
	}
}

func TestDeliveryResolvedRepairCannotTriggerOldRejectionLoop(t *testing.T) {
	for _, name := range []string{"read_data", "submit_extraction"} {
		s := efficiencyState()
		for _, id := range []string{"first-failure", "second-failure"} {
			raw, _ := json.Marshal(ToolResult{CallID: id, Name: name, Error: "same failed condition"})
			s.Operations = append(s.Operations, agentdata.Operation{RunID: s.Run.ID, Kind: name, Key: "tool:" + id, InputHash: "old", Stage: "failed", ResultJSON: string(raw)})
		}
		if repeatedToolRejection(s) == nil {
			t.Fatal("fixture did not reproduce repetition")
		}
		raw, _ := json.Marshal(ToolResult{CallID: "fixed", Name: name, RepairOf: "second-failure", Data: json.RawMessage(`{"accepted":true}`)})
		s.Operations = append(s.Operations, agentdata.Operation{RunID: s.Run.ID, Kind: name, Key: "tool:fixed", InputHash: "changed-arguments", Stage: "succeeded", ResultJSON: string(raw)})
		if len(unresolvedToolFailures(s)) != 0 {
			t.Fatal("fixture did not resolve failure")
		}
		if repeatedToolRejection(s) != nil {
			t.Fatal("resolved error still drives loop recovery")
		}
	}
}

func TestDeliveryRepairTargetAllowsAddingMissingIdentityDetails(t *testing.T) {
	a := FunctionCall{Name: "validate_program", Arguments: `{"url":"https://example.com/list","purpose":"read"}`}
	b := FunctionCall{Name: "validate_program", Arguments: `{"url":"https://example.com/list","source_id":"asset","purpose":"read","code":"fixed"}`}
	if toolRepairTarget(a) != toolRepairTarget(b) {
		t.Fatal("adding missing source_id was mistaken for changing the source URL")
	}
	b.Arguments = `{"url":"https://another.example/list","source_id":"asset"}`
	if toolRepairTarget(a) == toolRepairTarget(b) {
		t.Fatal("different source URL was accepted as same target")
	}
}
func TestDeliveryPreflightFailurePreservesRepairRelation(t *testing.T) {
	s, r, _ := storedHTMLReader(t, "<main>source</main>")
	r, err := s.Data.Claim(context.Background(), r.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	call := ToolCall{ID: "preflight-one", Type: "function", Function: FunctionCall{Name: "validate_program", Arguments: `{"url":"https://example.com/list","source_id":"asset","kind":"adapter","code":"return true,{}"}`}}
	if _, err = s.executeDurableTool(context.Background(), r, call); err == nil {
		t.Fatal("expected missing purpose")
	}
	call.ID = "preflight-two"
	call.Function.Arguments = `{"url":"https://example.com/list","source_id":"asset","kind":"adapter","code":"return true,{}","repair_of":"preflight-one"}`
	reply, err := s.executeDurableTool(context.Background(), r, call)
	if err == nil || reply.RepairOf != "preflight-one" {
		t.Fatalf("preflight correction link lost: %+v %v", reply, err)
	}
}
