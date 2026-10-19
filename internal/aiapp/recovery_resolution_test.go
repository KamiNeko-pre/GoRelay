package aiapp

import (
	"encoding/json"
	"testing"

	"go-lua-crawler/internal/agentdata"
)

func TestAdmissionDiagnosisResolvesOnlyAfterTheRequiredValidatedState(t *testing.T) {
	for _, source := range []string{"source-alpha", "source-beta"} {
		for _, tc := range []struct {
			name, successSource, kind string
			resolved                  bool
		}{
			{"corrected", source, "adapter", true},
			{"other-source", source + "-other", "adapter", false},
			{"unchanged-kind", source, "composition", false},
		} {
			t.Run(source+"/"+tc.name, func(t *testing.T) {
				s := efficiencyState()
				failure := toolFailureReceipt(ToolCall{ID: "rejected", Function: FunctionCall{Name: "register_source"}}, ToolResult{}, sourceAdmissionFailure(AdmissionDecision{Reason: "adapter required"}, Candidate{ID: "old", SourceID: source}, agentdata.ProgramArtifact{Hash: "old-hash", Kind: "composition"}))
				raw, _ := json.Marshal(failure)
				success, _ := toolData(ToolCall{ID: "new-validation", Function: FunctionCall{Name: "validate_program"}}, map[string]any{"candidate_id": "new-candidate", "source_id": tc.successSource, "kind": tc.kind})
				done, _ := json.Marshal(success)
				s.Operations = []agentdata.Operation{{ID: 1, RunID: s.Run.ID, Key: "tool:rejected", Kind: "register_source", Stage: "failed", ResultJSON: string(raw)}, {ID: 2, RunID: s.Run.ID, Key: "tool:new-validation", Kind: "validate_program", Stage: "succeeded", ResultJSON: string(done)}}
				pending := unresolvedToolFailures(s)
				if (len(pending) == 0) != tc.resolved {
					t.Fatalf("stale admission diagnosis resolution=%v wanted=%v: %+v", len(pending) == 0, tc.resolved, pending)
				}
			})
		}
	}
}

func TestAdmissionRepairKeepsTheExactCandidatesParameters(t *testing.T) {
	s := efficiencyState()
	for i, id := range []string{"target-candidate", "other-candidate"} {
		args, _ := json.Marshal(map[string]any{"code": "return true,{items={}}", "kind": "composition", "source_id": "source", "url": "https://example.test/" + id})
		call := ToolCall{ID: id, Function: FunctionCall{Name: "validate_program", Arguments: string(args)}}
		model := modelRound(id, call)
		model.RunID = s.Run.ID
		model.ID = uint64(i*2 + 1)
		out, _ := toolData(call, map[string]any{"candidate_id": id, "artifact_hash": "shared-code-hash"})
		raw, _ := json.Marshal(out)
		s.Operations = append(s.Operations, model, agentdata.Operation{ID: uint64(i*2 + 2), RunID: s.Run.ID, Key: "tool:" + id, Kind: "validate_program", Stage: "succeeded", ResultJSON: string(raw)})
	}
	failure := toolFailureReceipt(ToolCall{ID: "admission", Function: FunctionCall{Name: "register_source"}}, ToolResult{}, sourceAdmissionFailure(AdmissionDecision{Reason: "adapter required"}, Candidate{ID: "target-candidate", SourceID: "source"}, agentdata.ProgramArtifact{Hash: "shared-code-hash", Kind: "composition"}))
	raw, _ := json.Marshal(failure)
	s.Operations = append(s.Operations, agentdata.Operation{ID: 5, RunID: s.Run.ID, Key: "tool:admission", Kind: "register_source", Stage: "failed", ResultJSON: string(raw)})
	for _, requirePublication := range []bool{false, true} {
		state := s
		spec := TaskSpec{}
		spec.RequirePublishedCapability = requirePublication
		state.Spec = &spec
		program := pendingProgramCorrections(state)
		if len(program) != 1 || program[0].ValidationCallID != "target-candidate" {
			t.Fatal("shared code selected a different URL/parameter proposal", requirePublication, program)
		}
	}
}
