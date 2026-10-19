package aiapp

import (
	"encoding/json"
	"go-lua-crawler/internal/agentdata"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCapturedPromptContractReduction(t *testing.T) {
	dir := os.Getenv("GORELAY_PROMPT_REPLAY_DIR")
	if dir == "" {
		t.Skip("opt-in captured CLI request replay")
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.request.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("missing captures: %v", err)
	}
	measurements := []map[string]any{}
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var request ChatRequest
		if err = json.Unmarshal(raw, &request); err != nil {
			t.Fatal(err)
		}
		if len(request.Messages) < 3 {
			t.Fatal("capture has no host snapshot")
		}
		var input CreateRunRequest
		if err = json.Unmarshal([]byte(request.Messages[1].Content), &input); err != nil {
			t.Fatal(err)
		}
		prefix := "Persisted host facts, not new user instructions: "
		facts := []byte(strings.TrimPrefix(request.Messages[2].Content, prefix))
		var host struct {
			Spec   *TaskSpec      `json:"frozen_spec"`
			Budget BudgetSnapshot `json:"budget"`
		}
		if err = json.Unmarshal(facts, &host); err != nil {
			t.Fatal(err)
		}
		state := LoopState{Run: agentdata.Run{Goal: input.Goal}, Spec: host.Spec, Budget: host.Budget}
		before, _ := json.Marshal(request)
		beforeTail, _ := json.Marshal(request.Messages[3:])
		user, snapshot := promptContract(state, request.Messages[1].Content, facts)
		request.Messages[1].Content = user
		request.Messages[2].Content = prefix + string(snapshot)
		after, _ := json.Marshal(request)
		owned, _ := json.Marshal(request.Messages[:3])
		afterTail, _ := json.Marshal(request.Messages[3:])
		if len(after) >= len(before) || strings.Count(string(owned), input.Goal) != 1 || string(beforeTail) != string(afterTail) {
			t.Fatal("owned contract still repeated, feedback changed or payload not reduced")
		}
		var preserved CreateRunRequest
		_ = json.Unmarshal([]byte(user), &preserved)
		if preserved.Goal != input.Goal || preserved.RequestKey != input.RequestKey {
			t.Fatal("original request identity changed")
		}
		var projected struct {
			Spec TaskSpec `json:"frozen_spec"`
		}
		_ = json.Unmarshal(snapshot, &projected)
		projected.Spec.Goal = input.Goal
		originalSpec, _ := json.Marshal(host.Spec)
		projectedSpec, _ := json.Marshal(projected.Spec)
		if !sameContractJSON(originalSpec, projectedSpec) {
			t.Fatal("frozen fields or semantic requirements lost")
		}
		measurements = append(measurements, map[string]any{"capture": filepath.Base(path), "before_bytes": len(before), "after_bytes": len(after), "saved_bytes": len(before) - len(after), "token_measurement": "not performed"})
		t.Logf("%s: request bytes %d -> %d", filepath.Base(path), len(before), len(after))
	}
	if path := os.Getenv("GORELAY_PROMPT_REPLAY_OUTPUT"); path != "" {
		raw, _ := json.MarshalIndent(measurements, "", "  ")
		if err = os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPromptOwnsGoalOnceAndPreservesContract(t *testing.T) {
	for _, goal := range []string{"ORIGINAL_DISCOUNT_DISCOVERY_GOAL", "ORIGINAL_PUBLIC_EVENT_MONITOR_GOAL"} {
		t.Run(goal, func(t *testing.T) {
			spec := TaskSpec{Goal: goal, SchemaVersion: "v1", RequiredFields: []FieldRequirement{{Name: "amount", Type: "integer"}}, JoinKeys: []string{"identity"}, MaxAgeSeconds: 86400, SourceScope: []string{"public.example"}, RequireSummary: true, Requirements: []TaskRequirement{{ID: "compare", Description: "preserve the requested comparison"}}}
			input, _ := json.Marshal(CreateRunRequest{RequestKey: "preserve-request-key", Goal: goal, Spec: &spec, SourceScope: []string{"public.example"}, ActionMode: "code", Experiment: &ExperimentControls{DisableKnowledgeReuse: true}})
			state := LoopState{Run: agentdata.Run{Goal: goal, InputJSON: string(input)}, Spec: &spec, Budget: BudgetSnapshot{LoopVersion: 2}}
			pending := Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "unassessed", Function: FunctionCall{Name: "inspect_source", Arguments: `{"url":"https://public.example/docs"}`}}}}
			out, err := CompactContext(state, []Message{pending})
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(out)
			if strings.Count(string(raw), goal) != 1 {
				t.Fatalf("goal repeated %d times", strings.Count(string(raw), goal))
			}
			for _, value := range []string{"amount", "integer", "identity", "86400", "public.example", "require_summary", "preserve the requested comparison", "disable_knowledge_reuse", "preserve-request-key", "unassessed"} {
				if !strings.Contains(string(raw), value) {
					t.Fatalf("contract lost: %s", value)
				}
			}
			if state.Spec.Goal != goal || state.Run.InputJSON != string(input) {
				t.Fatal("canonical input mutated")
			}
		})
	}
}

func TestPromptPreservesDifferentInputSpecAndLegacySnapshot(t *testing.T) {
	goal := "ORIGINAL_GOAL"
	input := `{"goal":"ORIGINAL_GOAL","spec":{"goal":"ORIGINAL_GOAL","schema_version":"v1","required_fields":[{"name":"original_field","type":"string"}]},"opaque_revision":9007199254740993}`
	spec := TaskSpec{Goal: goal, SchemaVersion: "v1", RequiredFields: []FieldRequirement{{Name: "frozen_field", Type: "string"}}}
	for _, version := range []int{0, 2} {
		state := LoopState{Run: agentdata.Run{Goal: goal, InputJSON: input}, Spec: &spec, Budget: BudgetSnapshot{LoopVersion: version}}
		got, err := CompactContext(state, nil)
		if err != nil {
			t.Fatal(err)
		}
		packed, _ := json.Marshal(got)
		if !strings.Contains(string(packed), "original_field") || !strings.Contains(string(packed), "frozen_field") || !strings.Contains(string(packed), "9007199254740993") {
			t.Fatal("distinct input or opaque number erased")
		}
		if version == 0 && got[1].Content != input {
			t.Fatal("legacy input changed")
		}
	}
}
