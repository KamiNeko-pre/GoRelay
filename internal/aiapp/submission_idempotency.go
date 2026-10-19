package aiapp

import (
	"encoding/json"
	"fmt"
	"go-lua-crawler/internal/agentdata"
)

// Tool-call idempotency.
//
// The operation ledger keys on the model's tool-call id, which the caller
// chooses. A model that wants to repeat an action only has to emit a new id:
// same tool, same arguments, same result, new ledger row. Measured on a real
// run, that is exactly how a loop survived the existing replay protection — 58
// reads of the same pages under fresh handles.
//
// The key must therefore be derived from the tool and its arguments on the host
// side, where the caller cannot change it: "tool name + canonical arguments".
// For an identical call the result is identical by construction, so repeating it
// cannot produce anything new, and the model is told which earlier call it
// duplicated and that it should use that receipt or choose another tool.
//
// A different host-resolved region is a different intent. Handles, cursors and
// equivalent selectors are normalized to the same immutable resource/region.
//
// One exception is mandatory. Context compaction replaces an old receipt with a
// summary, and a summary may lack actionable structure. A model may restore
// the saved structural receipt, or eviction becomes unrecoverable. The
// refusal therefore applies only while the receipt is still visible in the
// conversation the model was given. Once the host has compacted that receipt
// away, the identical call restores SQL data under a new reply ID. It never
// repeats the business execution or its external requests.

// toolCallKey is the host-owned identity of a tool call: the tool plus the
// canonical form of its arguments. It never includes the call id.
//
// The ledger already stores exactly this value in InputHash
// (canonicalToolInput hashes {name, arguments}), so a stored row compares
// directly.
func toolCallKey(name, arguments string) string {
	canonical := canonicalToolInput(FunctionCall{Name: name, Arguments: arguments})
	return name + "|" + canonical
}

// visibleReceipts is the set of tool-call ids whose receipt the model can still
// see in the messages it was given. A receipt that is absent here has been
// compacted away or evicted, and re-reading it is legitimate recovery.
type visibleReceipts map[string]bool

// visibleReceiptsOf collects the tool-call ids present in the conversation.
func visibleReceiptsOf(messages []Message) visibleReceipts {
	out := visibleReceipts{}
	for _, m := range messages {
		if m.Role == "tool" && m.ToolCallID != "" {
			out[m.ToolCallID] = true
		}
	}
	return out
}

// duplicateToolCall reports the earlier call that carried an identical tool and
// identical arguments and whose receipt the model can still see. When the
// receipt has been compacted away the repeat is recovery, not a loop, and this
// returns false.
//
// A prior call counts whether it succeeded or failed: a failed call with the
// same arguments is the same attempt, and repeating it is the loop.
func duplicateToolCall(ops []agentdata.Operation, visible visibleReceipts, name, arguments string) (string, bool) {
	return duplicateToolInput(ops, visible, name, canonicalToolInput(FunctionCall{Name: name, Arguments: arguments}))
}

func duplicateToolInput(ops []agentdata.Operation, visible visibleReceipts, name, input string) (string, bool) {
	for _, op := range ops {
		if op.Kind != name {
			continue
		}
		if op.Stage != "succeeded" && op.Stage != "failed" {
			continue
		}
		if op.InputHash == "" || op.InputHash != input {
			continue
		}
		var prior ToolResult
		if json.Unmarshal([]byte(op.ResultJSON), &prior) != nil || prior.CallID == "" {
			continue
		}
		// The receipt is gone from the conversation: the model is recovering
		// material it can no longer read, which is allowed and expected.
		if visible != nil && !visible[prior.CallID] {
			continue
		}
		return prior.CallID, true
	}
	return "", false
}

// duplicateToolCallError builds the structured refusal. It must tell the model
// three things: that this exact call already ran, that its receipt is still
// available, and that it should act on that receipt or choose another tool.
func duplicateToolCallError(name, priorCallID string) error {
	return &ParameterError{
		Reason: fmt.Sprintf(
			"%s has already been called with these exact arguments (earlier call %s), and its receipt is still in your context. The same arguments cannot return different content, so this call adds nothing. Read the receipt you already have from %s, or use a different tool for a different purpose.",
			name, priorCallID, priorCallID),
		Required: []string{"a different call than " + priorCallID},
		Accepted: map[string]string{
			"use_existing_receipt": "the result of " + priorCallID + " is already in this conversation",
			"change_arguments":     "target an actually different observed region or correct the diagnosed fields; changing call IDs, handles or equivalent selectors does not create new work",
			"other_tool":           "if this purpose is already served, choose a different tool",
		},
		Example: map[string]any{
			"do_not": "repeat " + priorCallID,
			"do":     "act on the receipt from " + priorCallID + ", or call " + name + " with different arguments",
		},
	}
}
