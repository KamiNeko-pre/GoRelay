package aiapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"go-lua-crawler/internal/agentdata"
	"gorm.io/gorm"
)

// Transport recovery reuses a persisted proposal without another model call.
// A new model decision must not silently replay a committed protocol ID: that
// hides the duplicate from the model even though no new business work happens.
func rejectCommittedToolCallIDs(state LoopState, message Message) error {
	committed := map[string]agentdata.Operation{}
	for _, op := range state.Operations {
		if strings.HasPrefix(op.Key, "tool:") {
			committed[strings.TrimPrefix(op.Key, "tool:")] = op
		}
	}
	for i, call := range message.ToolCalls {
		if op, ok := committed[call.ID]; ok {
			reason := fmt.Sprintf("%s proposed call ID %q which was already called by %s (saved stage %s). Use its saved receipt; no business action was repeated. Reusing a call ID or renaming an unchanged business intent cannot create new work.", call.Function.Name, call.ID, op.Kind, op.Stage)
			var receipt ToolResult
			if json.Unmarshal([]byte(op.ResultJSON), &receipt) == nil && receipt.Error != "" {
				reason += " Original cause: " + truncateRunes(receipt.Error, 600)
			}
			return invalidProposal(fmt.Sprintf("tool_calls[%d].id", i), reason)
		}
	}
	return nil
}

// Receipts keep the provider's call ID for message pairing. Execution identity
// is owned by the host and scoped to the Run: tool + canonical business input.
// Serializing lookup/reservation under the existing Run lock also closes the
// race between two proposals carrying different call IDs for the same action.
func (s *Service) reserveToolOperation(ctx context.Context, r agentdata.Run, call ToolCall, visible visibleReceipts, charge func(string) (string, error), quote *agentdata.SpendQuote, resources, repairOf string, protocolRejection bool) (op agentdata.Operation, fresh bool, err error) {
	err = s.Data.WithOwnedRun(ctx, r.ID, r.Token, func(store *agentdata.Store) error {
		ops, e := store.Operations(ctx, r.ID)
		if e != nil {
			return e
		}
		businessInput := businessToolInput(call.Function, ops)
		input := businessInput
		if protocolRejection {
			input = repairRejectionIdentity(businessInput)
		}
		var exact agentdata.Operation
		e = store.DB.WithContext(ctx).Where("run_id = ? AND `key` = ?", r.ID, "tool:"+call.ID).First(&exact).Error
		if e == nil {
			if exact.Kind != call.Function.Name || (exact.InputHash != input && exact.InputHash != canonicalToolInput(call.Function) && exact.InputHash != agentdata.Hash(call.Function)) {
				return agentdata.ErrConflict
			}
			op = exact
			return nil
		}
		if !errors.Is(e, gorm.ErrRecordNotFound) {
			return e
		}
		{
			var prior []agentdata.Operation
			identities := []string{input}
			if protocolRejection {
				identities = append(identities, businessInput)
			}
			if e = store.DB.WithContext(ctx).Where("run_id = ? AND kind = ? AND input_hash IN ?", r.ID, call.Function.Name, identities).Order("id ASC").Find(&prior).Error; e != nil {
				return e
			}
			if protocolRejection {
				// A business action already admitted takes precedence even when
				// the new proposal also carries invalid protocol metadata.
				admitted, rejected := []agentdata.Operation{}, []agentdata.Operation{}
				for _, saved := range prior {
					var receipt ToolResult
					json.Unmarshal([]byte(saved.ResultJSON), &receipt)
					if saved.InputHash == businessInput && !repairMetadataRejection(receipt.Failure) {
						admitted = append(admitted, saved)
					} else {
						rejected = append(rejected, saved)
					}
				}
				prior = append(admitted, rejected...)
			}
			for _, saved := range prior {
				var out ToolResult
				if saved.Stage == "reserved" {
					cause := toolContractFailure(fmt.Errorf("%s was already called as %s; its external outcome is not yet committed", call.Function.Name, strings.TrimPrefix(saved.Key, "tool:")), "OPERATION_OUTCOME_UNKNOWN", "unknown", map[string]any{"operation_id": saved.ID, "original_call_id": strings.TrimPrefix(saved.Key, "tool:")}, ToolRecovery{Strategy: "inspect_receipt", Instruction: "Inspect the saved operation before attempting new external work; a new call ID does not authorize repeating it."})
					out = ToolResult{CallID: call.ID, Handle: call.ID, Name: call.Function.Name, Error: cause.Error(), Failure: captureFailure(cause, 0, false)}
				} else {
					if e = json.Unmarshal([]byte(saved.ResultJSON), &out); e != nil {
						return fmt.Errorf("decode saved tool receipt: %w", e)
					}
					if out.ReplayOf != "" {
						continue
					}
					if !protocolRejection && repairMetadataRejection(out.Failure) {
						continue
					}
				}
				receiptVisible := visible == nil || visible[out.CallID]
				if saved.Stage != "reserved" && receiptVisible {
					original := out
					cause := toolContractFailure(fmt.Errorf("%s has already been called with the same business arguments (original call %s); no business action was repeated", call.Function.Name, original.CallID), "DUPLICATE_TOOL_CALL", "not_started", map[string]any{"business_key": call.Function.Name + "|" + input, "original_call_id": original.CallID, "original_status": saved.Stage, "original_error": original.Error, "original_receipt": original}, ToolRecovery{Strategy: "use_existing_receipt", Instruction: "Use the original receipt already in the conversation. If it failed, correct the arguments using that original error. Do not rename the call or handle, change metadata, or refetch to repeat the same work."})
					out = ToolResult{CallID: call.ID, Handle: call.ID, Name: call.Function.Name, Error: cause.Error(), Failure: captureFailure(cause, 0, false)}
				}
				// An evicted receipt can be restored, never re-executed. Its semantic
				// identity remains unchanged by visibility, wall clock or call ID.
				op, _, e = store.ReserveOperation(ctx, r.ID, r.Token, "tool:"+call.ID, input, call.Function.Name, nil)
				if e != nil {
					return e
				}
				out.CallID, out.Handle = call.ID, call.ID
				out.ReplayOf = saved.Key
				out.RepairOf = repairOf
				out.RepairTarget = businessRepairTarget(call.Function, ops)
				out.HTTP = &HTTPObservation{Known: true}
				bindFailureRecovery(call, &out)
				stage := "succeeded"
				if out.Error != "" {
					stage = "failed"
				}
				raw, e := json.Marshal(out)
				if e != nil {
					return e
				}
				if e = store.CompleteOperation(ctx, r.ID, r.Token, op.ID, stage, string(raw), ""); e != nil {
					return e
				}
				op.Stage, op.ResultJSON = stage, string(raw)
				return nil
			}
		}
		op, fresh, e = store.ReserveOperationWithResources(ctx, r.ID, r.Token, "tool:"+call.ID, input, call.Function.Name, charge, quote, resources, agentdata.Hash(call.Function))
		return e
	})
	return
}
