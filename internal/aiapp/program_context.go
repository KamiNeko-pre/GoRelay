package aiapp

import (
	"encoding/json"
)

type programCorrection struct {
	FailedCallID      string          `json:"failed_call_id,omitempty"`
	ValidationCallID  string          `json:"validation_call_id"`
	Tool              string          `json:"tool"`
	PreviousArguments json.RawMessage `json:"previous_arguments"`
	Instruction       string          `json:"instruction"`
}

// A program remains a working object while its validation needs correction OR
// the contract still requires publishing and using it. Tool success does not
// end that lifecycle. Restore arguments from durable proposals, never reasoning.
func pendingProgramCorrections(state LoopState) []programCorrection {
	pending := map[string]ToolResult{}
	publicationPending := state.Spec != nil && state.Spec.RequirePublishedCapability && (state.Result == nil || state.Result.ReusableCapabilities == 0)
	validationReceipts := map[string]ToolResult{}
	for _, op := range state.Operations {
		if op.RunID == state.Run.ID && op.Kind == "validate_program" && op.Stage == "succeeded" {
			var out ToolResult
			if json.Unmarshal([]byte(op.ResultJSON), &out) == nil && out.Error == "" && out.ReplayOf == "" {
				validationReceipts[out.CallID] = out
			}
		}
	}
	for _, failure := range unresolvedToolFailures(state) {
		if failure.Name == "validate_program" && failure.ReplayOf == "" {
			pending[failure.CallID] = failure
		} else if failure.ReplayOf == "" && failure.Failure != nil && failure.Failure.Recovery != nil && failure.Failure.Recovery.Tool == "validate_program" {
			// Admission diagnostics name the validated candidate/artifact they
			// require changing. Follow that provenance rather than the last read.
			raw, _ := json.Marshal(failure.Failure.Details)
			var ref struct {
				CandidateID  string `json:"candidate_id"`
				ArtifactHash string `json:"artifact_hash"`
			}
			json.Unmarshal(raw, &ref)
			for id, receipt := range validationReceipts {
				var candidate struct {
					ID           string `json:"id"`
					CandidateID  string `json:"candidate_id"`
					ArtifactHash string `json:"artifact_hash"`
				}
				json.Unmarshal(receipt.Data, &candidate)
				if ref.CandidateID != "" && (ref.CandidateID == candidate.CandidateID || ref.CandidateID == candidate.ID) || ref.CandidateID == "" && ref.ArtifactHash != "" && ref.ArtifactHash == candidate.ArtifactHash {
					pending[id] = failure
				}
			}
		}
	}
	if len(pending) == 0 && !publicationPending {
		return nil
	}
	for n := len(state.Operations) - 1; n >= 0; n-- {
		op := state.Operations[n]
		if op.Kind != "model" || op.Stage != "succeeded" || op.RunID != state.Run.ID {
			continue
		}
		var response ChatResponse
		if json.Unmarshal([]byte(op.ResultJSON), &response) != nil {
			continue
		}
		for i := len(response.Message.ToolCalls) - 1; i >= 0; i-- {
			call := response.Message.ToolCalls[i]
			failure, needsCorrection := pending[call.ID]
			_, validated := validationReceipts[call.ID]
			// A named unresolved diagnosis owns its exact proposal. Publication
			// fallback must not replace it with a newer, unrelated validation.
			if call.Function.Name != "validate_program" || !needsCorrection && !(len(pending) == 0 && publicationPending && validated) {
				continue
			}
			var args map[string]json.RawMessage
			var code string
			if json.Unmarshal([]byte(call.Function.Arguments), &args) != nil || json.Unmarshal(args["code"], &code) != nil || code == "" {
				continue
			}
			delete(args, "repair_of")
			raw, _ := json.Marshal(args)
			instruction := "This is the saved working program, not a new proposal. Preserve its working pagination, extraction and parameters. Follow its successful validation next_action to register and then submit_acquisition; if admission requires changed kind/arguments, apply that diagnostic before validating again. Do not restart discovery because history was compacted."
			var validation struct {
				Mode string `json:"mode"`
			}
			json.Unmarshal(validationReceipts[call.ID].Data, &validation)
			if validation.Mode == "probe" {
				instruction = "This is the saved intermediate probe, not a publishable candidate. Preserve working navigation, code and parameters; apply its field_diagnostics to incomplete fields and add real _evidence locators. Validate mode=acquisition (kind=adapter when publication is required), then follow the returned registration and submit_acquisition steps. Do not register the probe or restart discovery after compaction."
			}
			if needsCorrection {
				instruction = "Apply the unresolved diagnostic to these saved program arguments; preserve working pagination and extraction. Validate the corrected proposal."
				if failure.Name == "validate_program" {
					instruction += " Use repair_of equal to failed_call_id for this same-tool correction."
				} else {
					instruction += " The failure belongs to a different tool: do not use its ID as validate_program repair_of. Follow the ordered recovery steps, then register the new candidate and use the published source."
				}
			}
			return []programCorrection{{FailedCallID: failure.CallID, ValidationCallID: call.ID, Tool: "validate_program", PreviousArguments: raw, Instruction: instruction}}
		}
	}
	return nil
}

func crawlerStateForHistory(state LoopState, history []Message) map[string]any {
	summary := crawlerStateSummary(state)
	visible := map[string]bool{}
	for _, m := range history {
		if m.Role == "assistant" {
			for _, call := range m.ToolCalls {
				if call.Function.Name == "validate_program" {
					visible[call.ID] = true
				}
			}
		}
	}
	missing := []programCorrection{}
	for _, program := range pendingProgramCorrections(state) {
		if !visible[program.ValidationCallID] {
			missing = append(missing, program)
		}
	}
	if len(missing) > 0 {
		summary["program_corrections"] = missing
	}
	return summary
}
