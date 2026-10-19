package aiapp

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/agentdata"
)

// Use the same persisted proposal/receipt boundary as executionState. No model
// claims or fabricated read offsets stand in for host-observed source evidence.
func appendEvidenceRound(s *LoopState, phase, name, args string, data any) uint64 {
	id := uint64(len(s.Operations) + 1)
	call := ToolCall{ID: fmt.Sprintf("evidence-%d", id), Type: "function", Function: FunctionCall{Name: name, Arguments: args}}
	model := modelRound(call.ID, call)
	model.ID, model.RunID = id, s.Run.ID
	var response ChatResponse
	json.Unmarshal([]byte(model.ResultJSON), &response)
	response.DeliveryRecoveryPhase = phase
	model.ResultJSON = string(mustJSON(response))
	out, _ := toolData(call, data)
	s.Operations = append(s.Operations, model, agentdata.Operation{ID: id + 1, RunID: s.Run.ID, Key: "tool:" + call.ID, Kind: name, Stage: "succeeded", ResultJSON: string(mustJSON(out))})
	return id
}

func evidenceRecoveryState() (LoopState, DeliveryRecovery) {
	s := efficiencyState()
	s.Budget.Unlimited = true
	id := appendEvidenceRound(&s, "", "list_sources", `{}`, map[string]any{"sources": []any{}})
	return s, DeliveryRecovery{Phase: "correcting", LastModel: id, Baseline: deliveryInformationSignature(s), ReadBaseline: agentdata.Hash(consumedReadRanges(s))}
}

func recoveryCounter(t *testing.T, r DeliveryRecovery, key string) int {
	t.Helper()
	var data map[string]any
	json.Unmarshal(mustJSON(r), &data)
	n, _ := data[key].(float64)
	return int(n)
}

func TestRecoverySavedInspectionMustReachTheRepairDecision(t *testing.T) {
	raw, err := os.ReadFile("testdata/recovery-premature-conclusion-20261016.json")
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Run        agentdata.Run         `json:"run"`
		Prior      HostProgress          `json:"prior"`
		Operations []agentdata.Operation `json:"operations"`
	}
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	s := LoopState{Run: saved.Run, Operations: saved.Operations}
	json.Unmarshal([]byte(s.Run.SpecJSON), &s.Spec)
	json.Unmarshal([]byte(s.Run.BudgetJSON), &s.Budget)
	s.Run.Status, s.Run.Deadline = "reviewing", time.Now().Add(6*time.Minute)
	s.Budget.WorkDeadline, s.Budget.Deadline = time.Time{}, s.Run.Deadline
	if saved.Prior.Recovery.Baseline != deliveryInformationSignature(s) {
		t.Fatal("saved reproduction lost its delivery baseline")
	}
	h := advanceHostProgressForTest(saved.Prior, s)
	if h.Recovery.Phase != "correcting" {
		t.Fatalf("model 27 acquired missing detail structure but cannot repair it: %+v", h.Recovery)
	}
	if h.NoProgressModelRounds <= saved.Prior.NoProgressModelRounds || h.Recovery.Attempts != saved.Prior.Recovery.Attempts+1 {
		t.Fatalf("preparation erased delivery debt or counted twice: %+v", h)
	}
	if h.Recovery.Baseline != saved.Prior.Recovery.Baseline {
		t.Fatal("inspection was promoted to accepted output")
	}
	if twice := advanceHostProgressForTest(h, s); twice.Recovery.Attempts != h.Recovery.Attempts {
		t.Fatal("same settled model was counted twice")
	}
	s.HostProgress = h
	canRepair := false
	for _, definition := range runToolDefinitionsForState(s) {
		canRepair = canRepair || definition.Function.Name == "validate_program"
	}
	if !canRepair || len(pendingProgramCorrections(s)) != 1 {
		t.Fatal("corrected phase still lost its editable program or validation tool")
	}
}

func TestRecoveryRequiresTwoPostFeedbackSignalsAndUnchangedBehavior(t *testing.T) {
	t.Run("old idle debt cannot terminate the first correction", func(t *testing.T) {
		s, r := evidenceRecoveryState()
		r.IdleModels = 99 // Previously persisted debt is not two observed corrections.
		appendEvidenceRound(&s, "correcting", "list_sources", `{}`, map[string]any{"sources": []any{}})
		r = advanceDeliveryRecovery(r, s, HostProgress{}, true, "round-1")
		if r.Phase != "correcting" || recoveryCounter(t, r, "semantic_repeat_rounds") != 1 || recoveryCounter(t, r, "no_evidence_rounds") != 1 {
			t.Fatalf("first feedback opportunity was consumed by pre-correction debt: %+v", r)
		}
		appendEvidenceRound(&s, "correcting", "list_sources", `{}`, map[string]any{"sources": []any{}})
		r = advanceDeliveryRecovery(r, s, HostProgress{}, true, "round-1")
		if r.Phase != "concluding" {
			t.Fatal("two unchanged, evidence-free correction rounds did not conclude", r)
		}
	})
	t.Run("new repair action must receive its feedback", func(t *testing.T) {
		s, r := evidenceRecoveryState()
		appendEvidenceRound(&s, "correcting", "list_sources", `{}`, map[string]any{"sources": []any{}})
		r = advanceDeliveryRecovery(r, s, HostProgress{}, false, "")
		appendEvidenceRound(&s, "correcting", "validate_program", `{"mode":"probe","kind":"adapter","code":"return true,{items={}}"}`, map[string]any{"mode": "probe", "items": []any{}})
		r = advanceDeliveryRecovery(r, s, HostProgress{}, true, "round-1")
		if r.Phase != "correcting" {
			t.Fatal("different repair action was treated as unchanged looping", r)
		}
		appendEvidenceRound(&s, "correcting", "validate_program", `{"mode":"probe","kind":"adapter","code":"return true,{items={}}"}`, map[string]any{"mode": "probe", "items": []any{}})
		r = advanceDeliveryRecovery(r, s, HostProgress{}, false, "")
		if r.Phase != "concluding" {
			t.Fatal("repeating the same evidence-free repair must conclude", r)
		}
	})
	t.Run("interleaved matches are not consecutive semantic repeats", func(t *testing.T) {
		s, r := evidenceRecoveryState()
		methods := []string{"read_task_context", "validate_program", "inspect_source", "get_task_results"}
		for i, repeated := range []bool{true, false, true, false} {
			args := fmt.Sprintf(`{"code":"return true,{items={n=%d}}","mode":"probe"}`, i)
			appendEvidenceRound(&s, "correcting", methods[i], args, map[string]any{"mode": "probe", "items": []any{}})
			r = advanceDeliveryRecovery(r, s, HostProgress{}, repeated, "round-1")
			if r.Phase != "correcting" {
				t.Fatalf("novel action %d was cut off by accumulated matches: %+v", i, r)
			}
			want := 0
			if repeated {
				want = 1
			}
			if recoveryCounter(t, r, "semantic_repeat_rounds") != want {
				t.Fatal("semantic streak was not reset", r)
			}
		}
	})
}

func TestRecoveryCosmeticProbeEditsAreStillUnchangedLooping(t *testing.T) {
	s, r := evidenceRecoveryState()
	for i := 1; i <= 2; i++ {
		args := fmt.Sprintf(`{"mode":"probe","code":"local cosmetic_%d=1; return true,{items={}}"}`, i)
		appendEvidenceRound(&s, "correcting", "validate_program", args, map[string]any{"mode": "probe", "items": []any{}})
		r = advanceDeliveryRecovery(r, s, HostProgress{}, true, "round-1")
	}
	if r.Phase != "concluding" {
		t.Fatal("different code spelling with identical empty probe outcome bypassed loop detection", r)
	}
}

func TestRecoveryNewEvidenceVetoesTerminationWithoutBecomingDelivery(t *testing.T) {
	for _, body := range []string{`<dl><dt>Owner</dt><dd data-field="owner">North bureau</dd></dl>`, `{"dataset":{"publisher":{"name":"Data office"}}}`} {
		t.Run(body[:8], func(t *testing.T) {
			s, r := evidenceRecoveryState()
			appendEvidenceRound(&s, "correcting", "list_sources", `{}`, map[string]any{"sources": []any{}})
			r = advanceDeliveryRecovery(r, s, HostProgress{}, true, "round-1")
			page := inspectResponse(acquisition.Response{Body: body, Receipt: acquisition.Receipt{Method: "GET", URL: "https://source.example/detail", Status: 200, FetchedAt: time.Now()}}, acquisition.Stats{})
			appendEvidenceRound(&s, "correcting", "inspect_source", `{"url":"https://source.example/detail"}`, page)
			r = advanceDeliveryRecovery(r, s, HostProgress{}, true, "round-1")
			if r.Phase != "correcting" || recoveryCounter(t, r, "no_evidence_rounds") != 0 || r.Baseline != deliveryInformationSignature(s) {
				t.Fatal("new source structure was ignored or promoted to delivery", r)
			}
			// Same content under new URL/call/timestamp must not renew evidence.
			page.Source.Receipt.URL = "https://source.example/alias"
			page.Source.Receipt.FetchedAt = time.Now().Add(time.Second)
			appendEvidenceRound(&s, "correcting", "inspect_source", `{"url":"https://source.example/alias"}`, page)
			r = advanceDeliveryRecovery(r, s, HostProgress{}, true, "round-1")
			if r.Phase != "concluding" {
				t.Fatal("aliases of an observed source bypassed the semantic stop condition", r)
			}
		})
	}
}

func TestRecoveryNovelStructureContinuesBeyondOldPreparationAllowance(t *testing.T) {
	s, r := evidenceRecoveryState()
	for n := int64(0); n < explorationReviewWindow(s)+3; n++ {
		page := inspectResponse(acquisition.Response{Body: fmt.Sprintf(`<h2>Record %d</h2><a href="/detail/%d">detail</a>`, n, n), Receipt: acquisition.Receipt{Method: "GET", URL: fmt.Sprintf("https://source.example/%d", n), Status: 200, FetchedAt: time.Now()}}, acquisition.Stats{})
		appendEvidenceRound(&s, "correcting", "inspect_source", fmt.Sprintf(`{"url":"https://source.example/%d"}`, n), page)
		r = advanceDeliveryRecovery(r, s, HostProgress{}, false, "")
		if r.Phase != "correcting" {
			t.Fatalf("fixed preparation count overrode observed evidence: %+v", r)
		}
	}
	s.Budget.WorkDeadline = time.Now().Add(-time.Second)
	if got := advanceDeliveryRecovery(r, s, HostProgress{}, false, ""); got.Phase != "concluding" {
		t.Fatal("new evidence extended the explicit deadline", got)
	}
}

func TestRecoveryRepairBriefReportsTheActualStopRule(t *testing.T) {
	s, r := evidenceRecoveryState()
	appendEvidenceRound(&s, "correcting", "list_sources", `{}`, map[string]any{"sources": []any{}})
	s.HostProgress.Recovery = advanceDeliveryRecovery(r, s, HostProgress{}, true, "round-1")
	raw := string(mustJSON(buildCorrectionBrief(s, "correcting", "same observations")))
	for _, key := range []string{"loop_stop_condition", "no_evidence_rounds", "semantic_repeat_rounds", "behavior_repeated"} {
		if !strings.Contains(raw, key) {
			t.Fatal("model cannot see the reason or remaining correction opportunity", key, raw)
		}
	}
}
