package aiapp

import (
	"context"
	"encoding/json"
	"fmt"
	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/agentdata"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestExplorationFieldsAreIndependentOfDelivery(t *testing.T) {
	for _, field := range []string{"title", "price"} {
		spec := TaskSpec{RequiredFields: []FieldRequirement{{Name: field, Type: "string"}}}
		if err := probeFieldGuard(spec, []string{"href", field}); err != nil {
			t.Fatalf("valid exploration rejected: %v", err)
		}
		if err := probeFieldGuard(spec, []string{"href", "href"}); err == nil {
			t.Fatal("duplicate declaration accepted")
		}
	}
}

func TestProbeKeepsAllRowsAvailableBeyondPreview(t *testing.T) {
	s, run, _ := storedHTMLReader(t, `<p>source</p>`)
	items := []any{}
	for n := 0; n < 25; n++ {
		items = append(items, map[string]any{"title": fmt.Sprintf("row %d", n), "href": fmt.Sprintf("https://fixture.example/%d", n)})
	}
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"collection": map[string]any{"items": items}}})
	}))
	defer stub.Close()
	s.Runner, _ = NewRunnerClient(stub.URL, "test")
	out, err := s.trialProbe(context.Background(), run, ToolCall{ID: "probe-one", Function: FunctionCall{Name: "validate_program"}}, ToolResult{CallID: "probe-one", Name: "validate_program"}, trialArgs{Code: "return true,{items={}}", URL: "https://fixture.example", ProducedFields: []string{"title", "href"}}, TaskSpec{})
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	_ = json.Unmarshal(out.Data, &data)
	all, ok := data["items"].([]any)
	if !ok || len(all) != 25 {
		t.Fatal("probe permanently lost rows beyond preview")
	}
	if all[24].(map[string]any)["title"] != "row 24" {
		t.Fatal("last row changed")
	}
}

func TestModelReadingReferencesKeepSourceStructureAndContinuation(t *testing.T) {
	budget, _ := json.Marshal(BudgetSnapshot{LoopVersion: 5})
	run := agentdata.Run{BudgetJSON: string(budget)}
	body := "<main><a class='observed-link' href='/detail'>Observed title</a></main>"
	for _, name := range []string{"inspect_source", "read_data"} {
		var data any = map[string]any{"handle": "source", "view_version": "dom-structure-v1", "selector_catalog": []DOMBlock{{Selector: ".observed-link", Matches: 1, Sample: "Observed title"}}, "next_read": map[string]any{"handle": "source", "view": "dom", "offset": 16}}
		if name == "inspect_source" {
			data = inspectResponse(acquisition.Response{Body: body, Receipt: acquisition.Receipt{URL: "https://fixture.example/list", Status: 200}}, acquisition.Stats{})
		}
		encoded, _ := json.Marshal(data)
		out := modelObservationForRun(run, ToolResult{CallID: "source", Name: name, Data: encoded}, nil)
		var visible map[string]any
		if err := json.Unmarshal(out.Data, &visible); err != nil {
			t.Fatal(err)
		}
		if visible["text"] != nil || visible["information"] != nil {
			t.Fatalf("crawler reader exposed the retired prose interface: %s", out.Data)
		}
		if name == "inspect_source" {
			reader, ok := visible["read_with"].(map[string]any)
			if !ok || reader["view"] != "dom" || reader["handle"] != "source" {
				t.Fatalf("inspection lost canonical structure reader: %s", out.Data)
			}
		}
		if name == "read_data" {
			next, ok := visible["next_read"].(map[string]any)
			if !ok || next["handle"] != "source" || next["view"] != "dom" || next["offset"] != float64(16) {
				t.Fatalf("structure continuation lost: %s", out.Data)
			}
		}
	}
}

// The host stop at the stagnation limit applies to unlimited diagnostics too.
// Unlimited removes cost ceilings, not the requirement to make progress: a run
// that repeats ineffective behaviour without delivering must still end, or it
// can burn unbounded budget. The recovery feedback (see context.go) is what
// gives the model its chance to change strategy before this fires.
func TestStagnationLimitAppliesToUnlimitedDiagnostics(t *testing.T) {
	state := decisionState()
	state.Budget.LoopVersion = 5
	state.Budget.Unlimited = true
	state.HostProgress.NoProgressModelRounds = 100
	next, err := RunTaskLoop(context.Background(), state)
	if err != nil {
		t.Fatal(err)
	}
	// V5 gives one bounded correction opportunity before stopping: the first
	// round past the stagnation limit must still ask the model to change
	// approach, because the host cannot know whether the model had a real
	// alternative. The stop comes from the recovery phases once that chance is
	// spent (see delivery_recovery.go), so this assertion pins the entry to the
	// bounded path rather than demanding an immediate exit.
	if next.Kind == "finish" {
		if !next.HostStop {
			t.Fatalf("a stopping decision must be marked HostStop: %+v", next)
		}
	} else if next.Kind != "model" && next.Kind != "tools" && next.Kind != "wait" {
		t.Fatalf("stagnation produced an unbounded decision: %+v", next)
	}

	// The same policy must hold for a bounded run: one rule, not two.
	state.Budget.Unlimited = false
	next, err = RunTaskLoop(context.Background(), state)
	if err != nil {
		t.Fatal(err)
	}
	if next.Kind == "finish" && !next.HostStop {
		t.Fatalf("bounded run stopped without HostStop: %+v", next)
	}

	// Once the bounded recovery is exhausted the run must stop, which is what
	// makes the stagnation limit a real ceiling rather than a suggestion.
	state.HostProgress.Recovery.Phase = "closed"
	next, err = RunTaskLoop(context.Background(), state)
	if err != nil {
		t.Fatal(err)
	}
	if next.Kind != "finish" || !next.HostStop {
		t.Fatalf("exhausted recovery did not stop the run: %+v", next)
	}
}

func TestDefaultSourceReadPaginatesSelectorsWithoutProse(t *testing.T) {
	for _, word := range []string{"notice", "game"} {
		body := "<main>"
		for i := 0; i < 40; i++ {
			body += fmt.Sprintf("<section class='%s-card-%d'><a href='/detail/%d'>Observed item %d</a></section>", word, i, i, i)
		}
		body += "<p>" + strings.Repeat(word+" source text. ", 700) + "</p></main>"
		s, run, _ := storedHTMLReader(t, body)
		run, err := s.Data.Claim(context.Background(), run.ID, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		call := ToolCall{ID: "page-one", Type: "function", Function: FunctionCall{Name: "read_data", Arguments: "{\"handle\":\"source\",\"limit\":4}"}}
		out, err := s.executeDurableTool(context.Background(), run, call)
		if err != nil {
			t.Fatal(err)
		}
		var data map[string]any
		if err = json.Unmarshal(out.Data, &data); err != nil {
			t.Fatal(err)
		}
		selectors, ok := data["selector_catalog"].([]any)
		if !ok || len(selectors) == 0 || data["text"] != nil || data["next_read"] == nil {
			t.Fatalf("default read lost selector page or exposed prose: %s", out.Data)
		}
		args, _ := json.Marshal(data["next_read"])
		var continuation map[string]any
		json.Unmarshal(args, &continuation)
		if continuation["view"] != "dom" || continuation["handle"] != "source" {
			t.Fatalf("continuation changed resource or reader: %s", args)
		}
		next, err := s.executeDurableTool(context.Background(), run, ToolCall{ID: "page-two", Type: "function", Function: FunctionCall{Name: "read_data", Arguments: string(args)}})
		if err != nil {
			t.Fatal("structural continuation failed", err)
		}
		var second map[string]any
		if err = json.Unmarshal(next.Data, &second); err != nil {
			t.Fatal(err)
		}
		if second["text"] != nil || second["offset"].(float64) <= data["offset"].(float64) {
			t.Fatal("continuation exposed prose or restarted selectors")
		}
	}
}
