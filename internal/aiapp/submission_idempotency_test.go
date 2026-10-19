package aiapp

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"go-lua-crawler/internal/agentdata"
)

// dispatchedCall builds a ledger row as the host stores it: the operation key is
// the call id (caller-chosen), while InputHash is the host-owned identity of
// tool + canonical arguments.
func dispatchedCall(callID, name, arguments, stage string) agentdata.Operation {
	res := ToolResult{CallID: callID, Name: name}
	raw, _ := json.Marshal(res)
	return agentdata.Operation{
		Kind:      name,
		Stage:     stage,
		Key:       "tool:" + callID,
		InputHash: canonicalToolInput(FunctionCall{Name: name, Arguments: arguments}),
		ResultJSON: string(raw),
	}
}

// The core defect: a new call id must not make an identical call new.
func TestIdenticalArgumentsWithNewCallIDAreRefused(t *testing.T) {
	args := `{"handle":"call_00_abc","view":"content","offset":0,"limit":2000}`
	ops := []agentdata.Operation{dispatchedCall("call_00_first", "read_data", args, "succeeded")}
	prior, duplicate := duplicateToolCall(ops, allVisible(ops), "read_data", args)
	if !duplicate {
		t.Fatal("identical arguments under a new call id were accepted")
	}
	if prior != "call_00_first" {
		t.Fatalf("refusal named the wrong earlier call: %s", prior)
	}
}

// Key order and whitespace must not disguise an identical call.
func TestArgumentFormattingDoesNotEvadeTheKey(t *testing.T) {
	stored := `{"handle":"call_00_abc","view":"content","offset":0}`
	resubmitted := "{ \"offset\" : 0,\n  \"view\": \"content\",\n  \"handle\": \"call_00_abc\" }"
	ops := []agentdata.Operation{dispatchedCall("call_00_first", "read_data", stored, "succeeded")}
	if _, duplicate := duplicateToolCall(ops, allVisible(ops), "read_data", resubmitted); !duplicate {
		t.Fatal("reordered keys evaded the idempotency key")
	}
}

// Changing the read target is a different call and must stay allowed, or paging
// and corrected selectors would break.
func TestChangedArgumentsAreAllowed(t *testing.T) {
	for _, tc := range []struct{ name, before, after string }{
		{"offset paging", `{"handle":"h","offset":0,"limit":2000}`, `{"handle":"h","offset":2000,"limit":2000}`},
		{"selector correction", `{"handle":"h","selector":"ul li a"}`, `{"handle":"h","selector":"div.v_news_content a"}`},
		{"different handle", `{"handle":"h1","view":"content"}`, `{"handle":"h2","view":"content"}`},
		{"different view", `{"handle":"h","view":"content"}`, `{"handle":"h","view":"dom"}`},
		{"submission field change", `{"field_selectors":{"title":{"selector":"h1"}}}`, `{"field_selectors":{"title":{"selector":"h2"}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ops := []agentdata.Operation{dispatchedCall("call_00_first", "read_data", tc.before, "succeeded")}
			if _, duplicate := duplicateToolCall(ops, allVisible(ops), "read_data", tc.after); duplicate {
				t.Fatalf("a changed call was refused: %s -> %s", tc.before, tc.after)
			}
		})
	}
}

// A failed attempt is still the same attempt: repeating it identically is the
// loop, not a retry that could succeed.
func TestIdenticalRetryAfterFailureIsRefused(t *testing.T) {
	args := `{"code":"return nil,{}"}`
	ops := []agentdata.Operation{dispatchedCall("call_00_bad", "validate_program", args, "failed")}
	if _, duplicate := duplicateToolCall(ops, allVisible(ops), "validate_program", args); !duplicate {
		t.Fatal("an identical retry of a failed call was accepted")
	}
}

// A corrected program is a different call.
func TestCorrectedProgramIsAllowed(t *testing.T) {
	ops := []agentdata.Operation{dispatchedCall("call_00_bad", "validate_program", `{"code":"return nil,{}"}`, "failed")}
	if _, duplicate := duplicateToolCall(ops, allVisible(ops), "validate_program", `{"code":"return {items=1},{}"}`); duplicate {
		t.Fatal("a corrected program was refused")
	}
}

// The check applies to every tool, not only submissions.
func TestEveryToolIsCovered(t *testing.T) {
	for _, name := range []string{"read_data", "read_observation", "inspect_source", "submit_extraction", "submit_acquisition", "validate_program", "register_source", "finish_task", "search_knowledge", "update_task_progress"} {
		t.Run(name, func(t *testing.T) {
			args := `{"x":1}`
			ops := []agentdata.Operation{dispatchedCall("call_00_a", name, args, "succeeded")}
			if _, duplicate := duplicateToolCall(ops, allVisible(ops), name, args); !duplicate {
				t.Fatalf("%s is not covered by the idempotency key", name)
			}
		})
	}
}

// The same arguments for a *different* tool are a different call.
func TestDifferentToolsAreNotDuplicates(t *testing.T) {
	args := `{"handle":"h"}`
	ops := []agentdata.Operation{dispatchedCall("call_00_a", "read_data", args, "succeeded")}
	if _, duplicate := duplicateToolCall(ops, allVisible(ops), "read_observation", args); duplicate {
		t.Fatal("a different tool was treated as a duplicate")
	}
}

// An operation with no stored identity must not be treated as a match, or a
// legitimate first submission would be refused.
func TestUnknownStoredIdentityIsNotADuplicate(t *testing.T) {
	ops := []agentdata.Operation{{Kind: "read_data", Stage: "succeeded", Key: "tool:call_00_a", ResultJSON: `{"tool_call_id":"call_00_a","name":"read_data"}`}}
	if _, duplicate := duplicateToolCall(ops, allVisible(ops), "read_data", `{"handle":"h"}`); duplicate {
		t.Fatal("a row with no stored identity was treated as a duplicate")
	}
}

// The refusal must be structured and name the earlier call, so the model can act.
func TestRefusalIsStructuredAndNamesTheEarlierCall(t *testing.T) {
	err := duplicateToolCallError("read_data", "call_00_first")
	var perr *ParameterError
	if !errors.As(err, &perr) {
		t.Fatalf("refusal is not a ParameterError: %T", err)
	}
	if !strings.Contains(perr.Reason, "call_00_first") {
		t.Fatalf("refusal does not name the earlier call: %s", perr.Reason)
	}
	if len(perr.Required) == 0 || perr.Example == nil {
		t.Fatalf("refusal does not say what to do instead: %+v", perr)
	}
	// It must tell the model the earlier receipt is reusable.
	if !strings.Contains(perr.Reason, "receipt") {
		t.Fatalf("refusal does not point at the existing receipt: %s", perr.Reason)
	}
}

// The key must never contain the call id.
func TestKeyIsIndependentOfCallID(t *testing.T) {
	args := `{"handle":"h","offset":0}`
	a := toolCallKey("read_data", args)
	b := toolCallKey("read_data", args)
	if a != b {
		t.Fatal("the key is not deterministic")
	}
	if strings.Contains(a, "call_") {
		t.Fatalf("the key embeds a call id: %s", a)
	}
	// Two different call ids producing the same arguments yield the same key.
	opsA := []agentdata.Operation{dispatchedCall("call_00_x", "read_data", args, "succeeded")}
	opsB := []agentdata.Operation{dispatchedCall("call_99_y", "read_data", args, "succeeded")}
	if opsA[0].InputHash != opsB[0].InputHash {
		t.Fatal("the stored identity depends on the call id")
	}
}

// After compaction the receipt is no longer in the conversation. The identical
// call is then the documented way to recover it, and must be allowed — otherwise
// eviction becomes unrecoverable and the model cannot finish the task.
func TestIdenticalCallIsAllowedAfterCompaction(t *testing.T) {
	args := `{"handle":"call_00_abc","view":"content","offset":0,"limit":2000}`
	ops := []agentdata.Operation{dispatchedCall("call_00_first", "read_data", args, "succeeded")}
	// The conversation the model sees no longer carries that receipt.
	compacted := visibleReceipts{"call_00_other": true}
	if prior, duplicate := duplicateToolCall(ops, compacted, "read_data", args); duplicate {
		t.Fatalf("re-reading compacted material was refused as a repeat of %s", prior)
	}
}

// While the receipt is still visible, the identical call is a loop and is refused.
func TestIdenticalCallIsRefusedWhileReceiptIsVisible(t *testing.T) {
	args := `{"handle":"call_00_abc","view":"content","offset":0,"limit":2000}`
	ops := []agentdata.Operation{dispatchedCall("call_00_first", "read_data", args, "succeeded")}
	visible := visibleReceipts{"call_00_first": true}
	if _, duplicate := duplicateToolCall(ops, visible, "read_data", args); !duplicate {
		t.Fatal("re-reading material still in context was accepted")
	}
}

// A receipt that is not in the conversation must not be treated as visible just
// because the ledger holds it.
func TestLedgerPresenceDoesNotImplyVisibility(t *testing.T) {
	args := `{"handle":"h","offset":0}`
	ops := []agentdata.Operation{dispatchedCall("call_00_first", "read_data", args, "succeeded")}
	if _, duplicate := duplicateToolCall(ops, visibleReceipts{}, "read_data", args); duplicate {
		t.Fatal("a compacted receipt was treated as visible")
	}
}

// visibleReceiptsOf must read exactly the tool replies present in the messages.
func TestVisibleReceiptsReadsTheConversation(t *testing.T) {
	messages := []Message{
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "call_00_a", Function: FunctionCall{Name: "read_data"}}}},
		{Role: "tool", ToolCallID: "call_00_a", Content: `{"tool_call_id":"call_00_a"}`},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "call_00_b", Function: FunctionCall{Name: "read_data"}}}},
		{Role: "tool", ToolCallID: "call_00_b", Content: `{"tool_call_id":"call_00_b"}`},
	}
	visible := visibleReceiptsOf(messages)
	if !visible["call_00_a"] || !visible["call_00_b"] {
		t.Fatalf("present receipts were not seen: %+v", visible)
	}
	if visible["call_00_c"] {
		t.Fatal("an absent receipt was reported as visible")
	}
	// A compacted receipt is a summary message, not a tool reply, so it is not
	// counted as visible material.
	compacted := []Message{
		{Role: "tool", ToolCallID: "call_00_keep", Content: `{"tool_call_id":"call_00_keep"}`},
		{Role: "assistant", Content: "summary of earlier work"},
	}
	if got := visibleReceiptsOf(compacted); got["call_00_gone"] {
		t.Fatal("a compacted receipt was counted as visible")
	}
}

// The refusal must state that the receipt is still available and name the tool
// the model should consider instead.
func TestRefusalNamesTheToolAndTheReceipt(t *testing.T) {
	err := duplicateToolCallError("read_data", "call_00_first")
	var perr *ParameterError
	if !errors.As(err, &perr) {
		t.Fatalf("refusal is not a ParameterError: %T", err)
	}
	for _, want := range []string{"read_data", "call_00_first", "still in your context", "different tool"} {
		if !strings.Contains(perr.Reason, want) {
			t.Fatalf("refusal does not mention %q: %s", want, perr.Reason)
		}
	}
	if _, ok := perr.Accepted["other_tool"]; !ok {
		t.Fatalf("refusal does not offer trying another tool: %+v", perr.Accepted)
	}
}

// allVisible marks every stored receipt as visible, which is the state a refusal
// is meant for: the model can see the earlier answer.
func allVisible(ops []agentdata.Operation) visibleReceipts {
	out := visibleReceipts{}
	for _, op := range ops {
		var prior ToolResult
		if json.Unmarshal([]byte(op.ResultJSON), &prior) == nil && prior.CallID != "" {
			out[prior.CallID] = true
		}
	}
	return out
}
