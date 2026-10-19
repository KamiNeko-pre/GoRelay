package aiapp

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"go-lua-crawler/internal/agentdata"
)

func TestCompressionRetainsTheProgramThatMustBeCorrected(t *testing.T) {
	s, pressure := pressureHistory(t)
	code := "local marker='pending-program-repair'; return true,{items={}}"
	args, _ := json.Marshal(map[string]any{"kind": "adapter", "mode": "acquisition", "code": code, "source_id": "source", "purpose": "collect", "url": "https://example.test/"})
	call := ToolCall{ID: "failed-program", Type: "function", Function: FunctionCall{Name: "validate_program", Arguments: string(args)}}
	model := modelRound("program", call)
	model.ID = 100
	model.RunID = s.Run.ID
	failure := failedTool(call.ID, "validate_program", "FIELD_EVIDENCE_UNSUPPORTED", "title", "missing _evidence.title")
	failure.ID = 101
	failure.RunID = s.Run.ID
	s.Operations = append([]agentdata.Operation{model, failure}, s.Operations...)
	msgs := append([]Message{{Role: "assistant", ToolCalls: []ToolCall{call}}, {Role: "tool", ToolCallID: call.ID, Content: failure.ResultJSON}}, pressure...)
	out, profile, err := compactContextMeasured(s, msgs)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(out)
	if !profile.Triggered || !strings.Contains(string(raw), "pending-program-repair") {
		t.Fatal("failed program was evicted while its diagnostic survived", profile)
	}
	if f := profile.Facts["program_code"]; f.Before != 1 || f.Retention != 1 {
		t.Fatalf("program loss is unmeasured or unprotected: %+v", profile)
	}
	// A second compression must still recover from the SQL model operation,
	// without restoring its chain of thought or adding any tool execution.
	again, _, err := compactContextMeasured(s, append(out, pressure...))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(again)
	if !strings.Contains(string(raw), "pending-program-repair") {
		t.Fatal("program disappeared after subsequent compaction")
	}
}

func TestCompressionReportsProgramLossInsteadOfZeroCriticalLoss(t *testing.T) {
	s := efficiencyState()
	call := ToolCall{ID: "program", Function: FunctionCall{Name: "validate_program", Arguments: `{"code":"return true,{items={}}","kind":"adapter"}`}}
	model := modelRound("program", call)
	model.RunID = s.Run.ID
	failed := failedTool(call.ID, call.Function.Name, "FIELD_EVIDENCE_UNSUPPORTED", "title", "missing locator")
	failed.RunID = s.Run.ID
	s.Operations = []agentdata.Operation{model, failed}
	before := []Message{{Role: "assistant", ToolCalls: []ToolCall{call}}}
	profile := &ContextCompactionProfile{}
	finishContextMeasurement(s, before, nil, profile)
	if profile.Facts["program_code"].Lost != 1 || profile.Facts["program_arguments"].Lost != 1 || len(profile.MissingCriticalFacts) != 2 {
		t.Fatalf("lost program incorrectly reports no loss: %+v", profile)
	}
}

func TestCompressionDoesNotRetainResolvedProgramFailures(t *testing.T) {
	s := efficiencyState()
	call := ToolCall{ID: "bad", Function: FunctionCall{Name: "validate_program", Arguments: `{"code":"return true,{items={}}","kind":"adapter"}`}}
	model := modelRound("program", call)
	model.RunID = s.Run.ID
	failed := failedTool("bad", "validate_program", "FIELD_EVIDENCE_UNSUPPORTED", "title", "missing locator")
	failed.RunID = s.Run.ID
	s.Operations = []agentdata.Operation{model, failed}
	if len(pendingProgramCorrections(s)) != 1 {
		t.Fatal("fixture has no recoverable failed program")
	}
	repaired := ToolResult{CallID: "good", Name: "validate_program", RepairOf: "bad"}
	raw, _ := json.Marshal(repaired)
	s.Operations = append(s.Operations, agentdata.Operation{ID: 2, RunID: s.Run.ID, Key: "tool:good", Kind: "validate_program", Stage: "succeeded", ResultJSON: string(raw)})
	if len(pendingProgramCorrections(s)) != 0 {
		t.Fatal("resolved failure still pins obsolete code")
	}
}

func TestCapturedCrawlerProgramCorrectionIsRecovered(t *testing.T) {
	path := os.Getenv("GORELAY_TEST_CRAWLER_TRACE")
	if path == "" {
		t.Skip("explicit captured crawler trace required")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Run        agentdata.Run         `json:"run"`
		Operations []agentdata.Operation `json:"operations"`
		Result     TaskResult            `json:"result"`
	}
	if err = json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	s := LoopState{Run: saved.Run, Operations: saved.Operations, Result: &saved.Result}
	json.Unmarshal([]byte(saved.Run.BudgetJSON), &s.Budget)
	json.Unmarshal([]byte(saved.Run.SpecJSON), &s.Spec)
	json.Unmarshal([]byte(saved.Run.PlanJSON), &s.Messages)
	programs := pendingProgramCorrections(s)
	if len(programs) != 1 {
		t.Fatal("captured unresolved program missing", len(programs))
	}
	// Force source/history pressure after the actual saved final projection.
	_, pressure := pressureHistory(t)
	for i := range pressure {
		if pressure[i].Role == "tool" {
			var receipt ToolResult
			json.Unmarshal([]byte(pressure[i].Content), &receipt)
			s.Operations = append(s.Operations, agentdata.Operation{ID: uint64(1000 + i), RunID: s.Run.ID, Key: "tool:" + receipt.CallID, Kind: receipt.Name, Stage: "succeeded", ResultJSON: pressure[i].Content})
		}
	}
	out, profile, err := compactContextMeasured(s, append(s.Messages, pressure...))
	if err != nil {
		t.Fatal(err)
	}
	var original struct {
		Code string `json:"code"`
	}
	json.Unmarshal(programs[0].PreviousArguments, &original)
	facts := compactionFacts(s, out)
	if !facts["program_code"][agentdata.Hash([]string{original.Code})] {
		t.Fatal("actual failed Lua remains unavailable after compression")
	}
	for _, lost := range profile.MissingCriticalFacts {
		if strings.HasPrefix(lost, "program_code:") {
			t.Fatal(lost)
		}
	}
	auditSave(t, "captured-r2-program-recovery", map[string]any{"profile": profile, "failed_call_id": programs[0].FailedCallID, "code_bytes": len(original.Code), "recovered": true, "new_model_calls": 0, "messages": out})
}

func TestCompressionRetainsProgramAcrossPublicationStages(t *testing.T) {
	for _, rejected := range []bool{false, true} {
		t.Run(fmt.Sprint("admission_rejected_", rejected), func(t *testing.T) {
			s, pressure := pressureHistory(t)
			s.Spec = &TaskSpec{Goal: "collect reusable records", RequirePublishedCapability: true}
			args := `{"code":"local marker='publication-work-object'; return true,{items={}}","kind":"composition","source_id":"reusable-source","mode":"acquisition","url":"https://example.test/list","params":{"pages":2}}`
			call := ToolCall{ID: "validated-program", Type: "function", Function: FunctionCall{Name: "validate_program", Arguments: args}}
			model := modelRound("program", call)
			model.ID, model.RunID = 100, s.Run.ID
			validation, _ := toolData(call, map[string]any{"candidate_id": "candidate", "id": "candidate", "artifact_hash": "program-hash", "source_id": "reusable-source", "kind": "composition"})
			raw, _ := json.Marshal(validation)
			s.Operations = append([]agentdata.Operation{model, {ID: 101, RunID: s.Run.ID, Key: "tool:" + call.ID, Kind: call.Function.Name, Stage: "succeeded", ResultJSON: string(raw)}}, s.Operations...)
			messages := []Message{{Role: "assistant", ToolCalls: []ToolCall{call}}, {Role: "tool", ToolCallID: call.ID, Content: string(raw)}}
			if rejected {
				registration := ToolCall{ID: "rejected-publication", Function: FunctionCall{Name: "register_source"}}
				err := sourceAdmissionFailure(AdmissionDecision{Reason: "adapter required"}, Candidate{ID: "candidate", SourceID: "reusable-source"}, agentdata.ProgramArtifact{Hash: "program-hash", Kind: "composition"})
				failure := toolFailureReceipt(registration, ToolResult{}, err)
				raw, _ = json.Marshal(failure)
				s.Operations = append(s.Operations, agentdata.Operation{ID: 102, RunID: s.Run.ID, Key: "tool:" + registration.ID, Kind: registration.Function.Name, Stage: "failed", ResultJSON: string(raw)})
			}
			out, profile, err := compactContextMeasured(s, append(messages, pressure...))
			if err != nil {
				t.Fatal(err)
			}
			raw, _ = json.Marshal(out)
			if !strings.Contains(string(raw), "publication-work-object") {
				t.Fatalf("publication requires the program but its editable object was evicted: %+v", profile)
			}
			if profile.Facts["program_arguments"].Retention != 1 {
				t.Fatal("working parameters were not retained", profile)
			}
			s.Result = &TaskResult{ReusableCapabilities: 1}
			if !rejected && len(pendingProgramCorrections(s)) != 0 {
				t.Fatal("completed publication still pins obsolete working code")
			}
		})
	}
}
