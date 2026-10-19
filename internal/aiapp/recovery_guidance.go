package aiapp

import (
	"errors"
	"fmt"

	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/agentdata"
)

// A failed call is not a valid repair_of for a different tool. Keep the cause
// reference in the receipt, but never prescribe a protocol-invalid repair link.
func bindFailureRecovery(call ToolCall, out *ToolResult) {
	if out.Failure == nil || out.Failure.Recovery == nil {
		return
	}
	recovery := out.Failure.Recovery
	if recovery.Tool == "" {
		recovery.Tool = call.Function.Name
	}
	if recovery.Tool == call.Function.Name && !out.Failure.stops() && recovery.Strategy != "stop" && recovery.Strategy != "use_existing_receipt" {
		recovery.RepairOf = call.ID
	} else {
		recovery.RepairOf = ""
	}
	if len(recovery.Steps) != 0 {
		return
	}
	// A single correction uses the canonical instruction above. Multitool
	// workflows carry explicit ordered steps from their owning boundary.
	recovery.Steps = []RecoveryStep{{Action: recovery.Strategy, Tool: recovery.Tool, Instruction: recovery.Instruction}}
}

func sourceFetchFailure(cause error, response acquisition.Response, requested string, scope []string) error {
	details := map[string]any{"field": "url", "requested_url": requested, "receipt": response.Receipt, "allowed_scope": scope}
	code := "SOURCE_TRANSPORT_ERROR"
	instruction := "The source GET failed before usable data was obtained. Use an alternative URL from observed navigation or a compatible saved capability if the task permits; do not guess URL variants or repeat identical input. If no supported alternative exists, preserve the original network error and report the host/source dependency blocker."
	if response.Receipt.Status > 0 {
		code = "SOURCE_HTTP_ERROR"
		instruction = "The server returned the HTTP status shown in receipt; this is a known failed fetch, not an unknown execution. Use the exact next/detail URL from an observed link, checking url_resolve(base,href), source scope and current pagination. Change the URL only from that evidence. A rejected or unavailable page is not empty task data; if no permitted alternative exists, report the source gap."
	}
	if errors.Is(cause, acquisition.ErrNotAllowed) {
		code = "SOURCE_SCOPE_DENIED"
		instruction = "The requested URL violates the frozen public source scope. Choose an observed permitted URL within allowed_scope; do not rename calls or bypass network permissions. If the target cannot be reached under this contract, report the scope blocker."
	}
	if errors.Is(cause, acquisition.ErrBudgetExceeded) {
		code = "SOURCE_FETCH_LIMIT"
		instruction = "This fetch exceeded the source request/byte policy. Reuse saved snapshots or choose an observed narrower structured endpoint when available; do not refetch the same oversized source. If the required content cannot fit this fetch policy, report the capability limit."
	}
	return toolContractFailure(cause, code, "failed", details, ToolRecovery{Strategy: "correct_source_or_report_gap", Tool: "inspect_source", Instruction: instruction})
}

func sourceAdmissionFailure(decision AdmissionDecision, candidate Candidate, artifact agentdata.ProgramArtifact) error {
	details := map[string]any{"candidate_id": candidate.ID, "source_id": candidate.SourceID, "artifact_hash": artifact.Hash, "actual_kind": artifact.Kind, "expected_kind": "adapter", "admission": decision}
	recovery := ToolRecovery{Strategy: "correct_validated_candidate", Tool: "validate_program", Instruction: "Correct the failed admission checks in details using the frozen task contract and saved validation. Validate the corrected adapter, register its newly issued candidate_id, then submit_acquisition with its published source_id and validated parameters."}
	if artifact.Kind != "adapter" {
		details["field"] = "kind"
		recovery.ResolvedBy = &RecoveryCondition{Tool: "validate_program", Fields: map[string]string{"source_id": candidate.SourceID, "kind": "adapter"}}
		recovery.Instruction = "This candidate is kind=" + artifact.Kind + ", which is temporary and cannot be published. Reuse its existing program code and source_id, call validate_program with kind=adapter and mode=acquisition, then register_source with the new candidate_id, then submit_acquisition with the published source_id and validated params. Do not rename the candidate or reread pages to fix program kind."
		recovery.Steps = []RecoveryStep{
			{Action: "correct_kind_and_validate", Tool: "validate_program", ArgumentChanges: map[string]any{"kind": "adapter", "mode": "acquisition", "source_id": candidate.SourceID}, Instruction: "Apply these argument changes to the saved program validation; reuse its code, URL and params. mode=acquisition validates a publishable adapter. This is a different business input; do not attach the register_source failure as repair_of to a different tool."},
			{Action: "publish_validated_adapter", Tool: "register_source", Instruction: "Copy the new validation next_action candidate_id and description. The old composition candidate cannot be published."},
			{Action: "execute_published_adapter", Tool: "submit_acquisition", Instruction: "Copy source_id, URL and params from the new adapter's then_submit; verify child records and published_and_used before finish_task."},
		}
	}
	return toolContractFailure(fmt.Errorf("source admission rejected: %s", decision.Reason), "SOURCE_ADMISSION_REJECTED", "not_started", details, recovery)
}

func resultNextStep(result TaskResult) string {
	if result.CapabilityRequired && result.ReusableCapabilities == 0 {
		return "Required published-and-used capability is still missing, even if record coverage is satisfied. Reuse an adapter validation next_action, or validate existing extraction code with kind=adapter and mode=acquisition; register_source with its new candidate_id, submit_acquisition with the published source and validated params, then verify the child result. A temporary composition does not satisfy publication."
	}
	if result.Coverage.Satisfied {
		return "Coverage and required capability delivery are satisfied. Call finish_task with status=completed; a pure crawler delivery needs no knowledge retrieval or cited analysis."
	}
	return "Use coverage's missing fields/records to extract only the missing supported values, then check the changed receipt. If sources or tools cannot supply them, finish_task with partial/failed and explicit gaps; rereading unchanged results cannot satisfy coverage."
}
