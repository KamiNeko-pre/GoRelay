package aiapp

import (
	"encoding/json"
	"go-lua-crawler/internal/agentdata"
)

func probeDeliveryContinuation(spec TaskSpec, a trialArgs, probeCallID string) map[string]any {
	kind := a.Kind
	if spec.RequirePublishedCapability {
		kind = "adapter"
	}
	instruction := "Reuse the probe's observed extraction code and parameters. Apply field_diagnostics to repair incomplete fields on each observed layout, include real _evidence locators for required fields, and validate_program with the listed acquisition arguments. "
	if kind == "composition" {
		instruction += "Then submit_acquisition with the returned candidate_id and validated URL/params; this temporary route does not publish a capability."
	} else {
		instruction += "Then register_source with the returned candidate_id; submit_acquisition with the published source_id and validated URL/params."
	}
	return map[string]any{"tool": "validate_program", "reuse_arguments_from_probe": probeCallID, "argument_changes": map[string]any{"mode": "acquisition", "kind": kind, "produced_fields": frozenFieldList(spec)}, "instruction": instruction}
}

func programDeliveryContinuation(spec TaskSpec, a trialArgs, c Candidate, validationCallID string) map[string]any {
	if a.Kind == "composition" && spec.RequirePublishedCapability {
		return map[string]any{"tool": "validate_program", "reuse_arguments_from_validation": validationCallID, "argument_changes": map[string]any{"kind": "adapter", "mode": "acquisition"}, "instruction": "These temporary records validate, but this task requires a published-and-used capability. Reuse the saved validation arguments/code with the listed changes, validate the adapter, then follow its register_source and submit_acquisition continuation. Direct composition submission alone cannot complete this task."}
	}
	if a.Kind == "composition" {
		return map[string]any{"tool": "submit_acquisition", "arguments": map[string]any{"candidate_id": c.ID, "url": a.URL, "params": a.Params}}
	}
	return map[string]any{"tool": "register_source", "arguments": map[string]any{"candidate_id": c.ID, "description": a.Purpose}, "then_submit": map[string]any{"source_id": a.SourceID, "url": a.URL, "params": a.Params}}
}

func validatedSubmission(ops []agentdata.Operation, candidateID string) json.RawMessage {
	for _, op := range ops {
		if op.Kind != "validate_program" || op.Stage != "succeeded" {
			continue
		}
		var out ToolResult
		if json.Unmarshal([]byte(op.ResultJSON), &out) != nil || out.Error != "" || out.ReplayOf != "" {
			continue
		}
		var data struct {
			ID          string `json:"id"`
			CandidateID string `json:"candidate_id"`
			Next        struct {
				Submission json.RawMessage `json:"then_submit"`
			} `json:"next_action"`
		}
		if json.Unmarshal(out.Data, &data) == nil && (data.ID == candidateID || data.CandidateID == candidateID) && len(data.Next.Submission) > 0 {
			return data.Next.Submission
		}
	}
	return nil
}
