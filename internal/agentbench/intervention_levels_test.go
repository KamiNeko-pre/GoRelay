package agentbench

import (
	"encoding/json"
	"go-lua-crawler/internal/aiapp"
	"testing"
)

func TestSummarySeparatesApprovalHintsActionsAndUnknownAccounting(t *testing.T) {
	correct := true
	rows := []Trial{
		{CaseID: "none", Status: "completed", Correct: &correct, Accounting: &aiapp.RunAccounting{Interventions: []aiapp.HumanIntervention{}}},
		{CaseID: "approve", Status: "completed", Correct: &correct, Accounting: &aiapp.RunAccounting{Interventions: []aiapp.HumanIntervention{{Kind: "approval"}}}},
		{CaseID: "hint", Status: "completed", Correct: &correct, Accounting: &aiapp.RunAccounting{Interventions: []aiapp.HumanIntervention{{Kind: "source_hint"}}}},
		{CaseID: "mixed", Status: "failed", Accounting: &aiapp.RunAccounting{Interventions: []aiapp.HumanIntervention{{Kind: "approval"}, {Kind: "source_hint"}, {Kind: "manual_code"}}}},
		{CaseID: "legacy", Status: "completed", Correct: &correct},
		{CaseID: "unrecorded", Accounting: &aiapp.RunAccounting{}},
		{CaseID: "future", Accounting: &aiapp.RunAccounting{Interventions: []aiapp.HumanIntervention{{Kind: "future_kind"}}}},
	}
	raw, _ := json.Marshal(Summarize(rows)[0])
	var summary map[string]json.RawMessage
	_ = json.Unmarshal(raw, &summary)
	var trials, success, events map[string]int
	_ = json.Unmarshal(summary["trials_by_intervention_level"], &trials)
	_ = json.Unmarshal(summary["correct_tasks_by_intervention_level"], &success)
	_ = json.Unmarshal(summary["intervention_events_by_level"], &events)
	if trials["L0"] != 1 || trials["L1"] != 1 || trials["L2"] != 1 || trials["L3"] != 1 || trials["unknown"] != 3 {
		t.Fatalf("levels lost or missing ledger became autonomous: %s", raw)
	}
	if success["L0"] != 1 || success["L1"] != 1 || success["L2"] != 1 || success["L3"] != 0 || success["unknown"] != 1 {
		t.Fatal("assisted failure or unknown became autonomous success", string(raw))
	}
	if events["L1"] != 2 || events["L2"] != 2 || events["L3"] != 1 || events["unknown"] != 1 {
		t.Fatal("event counts confused with task counts", string(raw))
	}
	var total int
	_ = json.Unmarshal(summary["human_interventions"], &total)
	if total != 6 {
		t.Fatal("receipt events and total intervention count disagree", total)
	}
}
