package aiapp

import (
	"encoding/json"
	"go-lua-crawler/internal/agentdata"
	"strings"
	"testing"
)

func TestUnresolvedErrorSurvivesUnrelatedObservations(t *testing.T) {
	s := efficiencyState()
	f := ToolResult{CallID: "broken", Name: "validate_program", Error: "missing field deadline", Failure: &FailureInfo{Code: "PARAMETER_CONTRACT_ERROR", Details: map[string]any{"field": "deadline"}, Recovery: &ToolRecovery{Strategy: "correct_arguments", Instruction: "provide deadline"}}}
	raw, _ := json.Marshal(f)
	s.Operations = append(s.Operations, agentdata.Operation{RunID: s.Run.ID, Key: "tool:broken", Kind: f.Name, InputHash: "broken-input", Stage: "failed", ResultJSON: string(raw)})
	for i := 0; i < 12; i++ {
		appendEfficiencyRound(&s, "list_sources", map[string]any{"items": i}, "")
	}
	got, err := CompactContext(s, s.Messages)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mustJSON(got)), "missing field deadline") {
		t.Fatal("unresolved error evicted by unrelated successful reads")
	}
}

func TestFailureMemoryClearsRecoveredProposalAndExcludesOtherRuns(t *testing.T) {
	s := efficiencyState()
	failure := ToolResult{CallID: "f", Name: "read_data", Error: "bad pointer"}
	raw, _ := json.Marshal(failure)
	op := agentdata.Operation{RunID: s.Run.ID, Key: "tool:f", Kind: "read_data", InputHash: "same", Stage: "failed", ResultJSON: string(raw)}
	s.Operations = append(s.Operations, op)
	foreign := op
	foreign.RunID = "foreign"
	foreign.InputHash = "other"
	s.Operations = append(s.Operations, foreign)
	success := ToolResult{CallID: "s", Name: "read_data", Data: json.RawMessage(`{"text":"recovered"}`)}
	raw, _ = json.Marshal(success)
	op.Key = "tool:s"
	op.Stage = "succeeded"
	op.ResultJSON = string(raw)
	s.Operations = append(s.Operations, op)
	if got := unresolvedToolFailures(s); len(got) != 0 {
		t.Fatalf("resolved/foreign diagnostics retained: %+v", got)
	}
}

func TestUnresolvedFailureAppearsOnceWhileVisibleAndReturnsAfterEviction(t *testing.T) {
	s := efficiencyState()
	appendEfficiencyRound(&s, "validate_program", map[string]any{"expected": "title"}, "unique-diagnostic-keep")
	got, err := CompactContext(s, s.Messages)
	if err != nil {
		t.Fatal(err)
	}
	n := strings.Count(string(mustJSON(got)), "unique-diagnostic-keep")
	if n != 1 {
		t.Fatalf("visible diagnostic duplicated %d times", n)
	}
	raw, _ := json.Marshal(map[string]any{"recent_tool_feedback": []ToolResult{}, "unresolved_tool_failures": unresolvedToolFailures(s)})
	projected := contextFeedbackForHistory(raw, nil)
	if !strings.Contains(string(projected), "unique-diagnostic-keep") {
		t.Fatal("evicted diagnostic disappeared")
	}
}
