package aiapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go-lua-crawler/internal/agentdata"
	"gorm.io/gorm"
	"sort"
)

// Targets exclude editable implementation details (selector/code/params), but
// retain source identity and the requested field set. No fuzzy matching.
func toolRepairTarget(fn FunctionCall) string {
	var args map[string]json.RawMessage
	if json.Unmarshal([]byte(fn.Arguments), &args) != nil {
		return ""
	}
	target := map[string]any{}
	for _, key := range []string{"handle", "inspection_call_id", "tool_call_id", "url", "source_id", "task_id", "evidence_id"} {
		if value := args[key]; value != nil && string(value) != `""` && string(value) != "null" {
			target[key] = value
			break // The primary resource stays stable while missing metadata is corrected.
		}
	}
	if raw := args["field_selectors"]; raw != nil {
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) == nil {
			keys := make([]string, 0, len(fields))
			for key := range fields {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			target["fields"] = keys
		}
	}
	if len(target) == 0 {
		return ""
	}
	return agentdata.Hash(target)
}

func (s *Service) prepareToolRepair(ctx context.Context, r agentdata.Run, call ToolCall) (ToolCall, string, error) {
	var args map[string]json.RawMessage
	if json.Unmarshal([]byte(call.Function.Arguments), &args) != nil {
		return call, "", nil
	}
	raw, ok := args["repair_of"]
	if !ok {
		return call, "", nil
	}
	var id string
	if json.Unmarshal(raw, &id) != nil || id == "" || len(id) > 128 {
		return call, "", invalidRepairMetadata(call, "repair_of must be a failed tool_call_id", string(raw))
	}
	delete(args, "repair_of")
	stripped, _ := json.Marshal(args)
	execution := call
	execution.Function.Arguments = string(stripped)
	var prior agentdata.Operation
	if err := s.Data.DB.WithContext(ctx).Where("run_id = ? AND `key` = ? AND kind = ? AND stage = ?", r.ID, "tool:"+id, call.Function.Name, "failed").First(&prior).Error; err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return call, "", err
		}
		return call, "", invalidRepairMetadata(call, "repair_of must reference a failed call of the same tool in this Run", id)
	}
	var reply ToolResult
	if json.Unmarshal([]byte(prior.ResultJSON), &reply) != nil || reply.Error == "" {
		return call, "", fmt.Errorf("repair_of has no failed receipt")
	}
	ops, err := s.Data.Operations(ctx, r.ID)
	if err != nil {
		return call, "", err
	}
	if reply.RepairTarget != "" && reply.RepairTarget != businessRepairTarget(execution.Function, ops) && reply.RepairTarget != toolRepairTarget(execution.Function) {
		return call, "", invalidRepairMetadata(call, "repair_of target differs; use a new operation without repair_of for a different source/field set", id)
	}
	return execution, id, nil
}

func invalidRepairMetadata(call ToolCall, reason, actual string) error {
	cause := &ArgumentDecodeError{Reason: reason, Field: "repair_of", Expected: "omit this optional field for a new operation or revision of a successful call; otherwise use a failed same-tool same-target call ID in this Run", Actual: actual}
	return toolContractFailure(cause, "ARGUMENT_JSON_ERROR", "not_started", cause, ToolRecovery{Strategy: "correct_arguments", Tool: call.Function.Name,
		Instruction: "repair_of is optional protocol metadata. Omit the invalid repair_of when revising a successful/incomplete result or starting a different operation. For the same failed target, the host-issued failed-call reference may be used. Preserve the business code/arguments while correcting this metadata: no business action was admitted by this rejection. Once admitted, metadata changes cannot authorize a second execution.",
		Steps:       []RecoveryStep{{Action: "omit_invalid_repair_reference", Tool: call.Function.Name, Instruction: "Remove the optional repair_of field (do not set it to null) and keep the intended business arguments. An already admitted identical business action still returns its original receipt/duplicate error."}},
	})
}

// repair_of is excluded from business identity. Rejecting it therefore cannot
// consume that business action's first execution. The predicate also recognizes
// old durable protocol rejections that were incorrectly stored under that key.
func repairMetadataRejection(f *FailureInfo) bool {
	return f != nil && f.Code == "ARGUMENT_JSON_ERROR" && f.ExecutionStatus == "not_started" && failureField(f) == "repair_of"
}

func repairRejectionIdentity(businessInput string) string {
	return agentdata.Hash([]string{"protocol:repair_of", businessInput})
}
