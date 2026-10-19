package aiapp

import (
	"context"
	"encoding/json"
	"fmt"
	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/agentdata"
	"html"
	"testing"
	"time"
)

func recoveryPhase(t *testing.T, h HostProgress) string {
	t.Helper()
	var v struct {
		Recovery struct {
			Phase    string `json:"phase"`
			Attempts int    `json:"attempts"`
		} `json:"delivery_recovery"`
	}
	_ = json.Unmarshal(mustJSON(h), &v)
	return v.Recovery.Phase
}
func addRecoveryRound(s *LoopState, phase, text string) {
	// Source material is host-owned. A synthetic read with no source binding
	// cannot stand in for new acquisition in a progress test.
	sourceID := "recovery-source-" + agentdata.Hash(text)[:16]
	body := "<article><h2>" + html.EscapeString(text) + "</h2></article>"
	selector := sourceSelectorCatalog(body)[0].Selector
	found := false
	for _, op := range s.Operations {
		if op.Key == "tool:"+sourceID {
			found = true
		}
	}
	if !found {
		page := inspectResponse(acquisition.Response{Body: body, Receipt: acquisition.Receipt{Method: "GET", URL: "https://fixture.example/recovery", Status: 200, FetchedAt: time.Now()}}, acquisition.Stats{})
		out, _ := toolData(ToolCall{ID: sourceID, Function: FunctionCall{Name: "inspect_source"}}, page)
		raw, _ := json.Marshal(out)
		s.Operations = append(s.Operations, agentdata.Operation{ID: uint64(len(s.Operations) + 1), RunID: s.Run.ID, Key: "tool:" + sourceID, Kind: "inspect_source", Stage: "succeeded", ResultJSON: string(raw)})
	}
	n := len(s.Operations)
	id := fmt.Sprintf("recovery-%d", n)
	args, _ := json.Marshal(dataReadArgs{Handle: sourceID, View: "content", Selector: selector})
	call := ToolCall{ID: id, Type: "function", Function: FunctionCall{Name: "read_data", Arguments: string(args)}}
	response := map[string]any{"delivery_recovery_phase": phase, "message": Message{Role: "assistant", ToolCalls: []ToolCall{call}}}
	raw, _ := json.Marshal(response)
	s.Operations = append(s.Operations, agentdata.Operation{ID: uint64(n + 1), RunID: s.Run.ID, Kind: "model", Stage: "succeeded", ResultJSON: string(raw)})
	result, _ := toolData(call, map[string]any{"handle": sourceID, "url": "https://fixture.example/recovery", "selector": selector, "text": text, "view_version": htmlInformationVersion})
	raw, _ = json.Marshal(result)
	s.Operations = append(s.Operations, agentdata.Operation{ID: uint64(n + 2), RunID: s.Run.ID, Key: "tool:" + id, Kind: "read_data", Stage: "succeeded", InputHash: text, ResultJSON: string(raw)})
}
func TestDeliveryRecoveryOffersTwoActualCorrectionsThenOneConclusion(t *testing.T) {
	s := efficiencyState()
	addRecoveryRound(&s, "", "same evidence")
	addRecoveryRound(&s, "", "same evidence")
	h := advanceHostProgressForTest(HostProgress{}, s)
	if recoveryPhase(t, h) != "correcting" {
		t.Fatal("repetition did not enter correction state")
	}
	for i := 0; i < 2; i++ {
		s.HostProgress = h
		addRecoveryRound(&s, "correcting", "same evidence")
		h = advanceHostProgressForTest(h, s)
		want := "correcting"
		if i == 1 {
			want = "concluding"
		}
		if recoveryPhase(t, h) != want {
			t.Fatalf("attempt %d phase=%s", i, recoveryPhase(t, h))
		}
	}
	s.HostProgress = h
	s.Messages = nil
	a, err := RunTaskLoop(context.Background(), s)
	if err != nil || a.Kind != "model" {
		t.Fatalf("no conclusion opportunity: %+v %v", a, err)
	}
	// A conclusion that tries to restart acquisition cannot reopen the loop.
	addRecoveryRound(&s, "concluding", "same evidence")
	h = advanceHostProgressForTest(h, s)
	s.HostProgress = h
	a, err = RunTaskLoop(context.Background(), s)
	if err != nil || a.Kind != "finish" {
		t.Fatalf("conclusion reopened exploration: %+v %v", a, err)
	}
}
func TestDeliveryRecoveryResetsOnlyForSubstantiveOutput(t *testing.T) {
	s := efficiencyState()
	addRecoveryRound(&s, "", "same evidence")
	addRecoveryRound(&s, "", "same evidence")
	h := advanceHostProgressForTest(HostProgress{}, s)
	s.HostProgress = h
	addRecoveryRound(&s, "correcting", "new evidence with a different date and amount")
	h = advanceHostProgressForTest(h, s)
	if recoveryPhase(t, h) != "correcting" {
		t.Fatal("a new page reset recovery without producing a requested field")
	}
	s.Result = &TaskResult{Records: []MergedRecord{{SchemaVersion: "v1", Fields: map[string]any{"title": "accepted source title"}}}}
	h = advanceHostProgressForTest(h, s)
	if recoveryPhase(t, h) != "" {
		t.Fatal("accepted fields failed to restore useful work")
	}
}

func TestDeliveryFailedConclusionDoesNotRetryModel(t *testing.T) {
	s := efficiencyState()
	s.HostProgress.Recovery = DeliveryRecovery{Phase: "concluding", LastModel: 1}
	raw, _ := json.Marshal(ChatResponse{DeliveryRecoveryPhase: "concluding", Failure: &FailureInfo{Kind: "unknown_model_outcome", Message: "timeout"}})
	s.Operations = []agentdata.Operation{{ID: 2, Kind: "model", Stage: "unknown_external", ResultJSON: string(raw)}}
	h := advanceHostProgressForTest(s.HostProgress, s)
	s.HostProgress = h
	if h.Recovery.Phase != "closed" {
		t.Fatal("failed conclusion reopened model retries")
	}
	a, err := RunTaskLoop(context.Background(), s)
	if err != nil || a.Kind != "finish" {
		t.Fatal(a, err)
	}
}

func TestFreshProgramDiagnosisGetsAnActualRepairOpportunity(t *testing.T) {
	for _, field := range []string{"title", "publisher"} {
		t.Run(field, func(t *testing.T) {
			s := efficiencyState()
			s.Budget.Unlimited = true
			call := ToolCall{ID: "first-failed-program", Function: FunctionCall{Name: "validate_program", Arguments: `{"code":"return true,{items={}}","kind":"adapter"}`}}
			model := modelRound("repair-boundary", call)
			model.ID, model.RunID = 10, s.Run.ID
			var response ChatResponse
			json.Unmarshal([]byte(model.ResultJSON), &response)
			response.DeliveryRecoveryPhase = "correcting"
			raw, _ := json.Marshal(response)
			model.ResultJSON = string(raw)
			failure := toolFailureReceipt(call, ToolResult{}, toolContractFailure(fmt.Errorf("missing _evidence.%s locator", field), "FIELD_EVIDENCE_UNSUPPORTED", "failed", map[string]any{"field": field}, ToolRecovery{Tool: "validate_program", Strategy: "correct_field_locator", Instruction: "Bind the field to its observed locator and validate the corrected program."}))
			raw, _ = json.Marshal(failure)
			failed := agentdata.Operation{ID: 11, RunID: s.Run.ID, Key: "tool:" + call.ID, Kind: call.Function.Name, Stage: "failed", ResultJSON: string(raw)}
			s.Operations = []agentdata.Operation{model, failed}
			prior := DeliveryRecovery{Phase: "correcting", LastModel: 8, IdleModels: deliveryIdleRoundLimit - 1, Baseline: deliveryInformationSignature(s), ReadBaseline: agentdata.Hash(consumedReadRanges(s))}
			host := HostProgress{NoProgressModelRounds: 9}
			got := advanceDeliveryRecovery(prior, s, host, false, "")
			if got.Phase != "correcting" || got.IdleModels != prior.IdleModels || got.Attempts != prior.Attempts+1 {
				t.Fatal("new diagnostic was consumed as an already-tried fix", got)
			}
			// A reworded defect under another proposal cannot renew the window.
			call.ID = "renamed-failed-program"
			response.Message.ToolCalls = []ToolCall{call}
			raw, _ = json.Marshal(response)
			model.ID, model.ResultJSON = 12, string(raw)
			failure.CallID, failure.Error = call.ID, "different wording of missing locator"
			failure.Failure.Message = failure.Error
			raw, _ = json.Marshal(failure)
			failed.ID, failed.Key, failed.ResultJSON = 13, "tool:"+call.ID, string(raw)
			s.Operations = append(s.Operations, model, failed)
			got = advanceDeliveryRecovery(got, s, host, false, "")
			if got.Phase != "correcting" || got.NoEvidenceRounds != 1 {
				t.Fatal("first evidence-free repeat consumed two correction rounds", got)
			}
			call.ID = "same-diagnosis-again"
			response.Message.ToolCalls = []ToolCall{call}
			model.ID, model.ResultJSON = 14, string(mustJSON(response))
			failure.CallID = call.ID
			failed.ID, failed.Key, failed.ResultJSON = 15, "tool:"+call.ID, string(mustJSON(failure))
			s.Operations = append(s.Operations, model, failed)
			if repeated := advanceDeliveryRecovery(got, s, host, false, ""); repeated.Phase != "concluding" || repeated.NoEvidenceRounds != 2 {
				t.Fatal("same diagnosed behavior survived two evidence-free correction rounds", repeated)
			}
			// Neither fresh diagnostics nor an editable object extend a deadline.
			s.Operations = s.Operations[:2]
			s.Budget.WorkDeadline = time.Now().Add(-time.Second)
			if got = advanceDeliveryRecovery(prior, s, host, false, ""); got.Phase != "concluding" {
				t.Fatal("diagnostic bypassed deadline", got)
			}
		})
	}
}
