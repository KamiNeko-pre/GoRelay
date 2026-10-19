package aiapp

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/agentdata"
)

func appendExploration(s *LoopState, format string) {
	id := uint64(len(s.Operations) + 1)
	s.Operations = append(s.Operations, agentdata.Operation{ID: id, Kind: "model", Stage: "succeeded"})
	body := fmt.Sprintf(format, id)
	out, _ := toolData(ToolCall{ID: fmt.Sprint(id), Function: FunctionCall{Name: "inspect_source"}}, inspectResponse(acquisition.Response{Body: body, Receipt: acquisition.Receipt{Method: "GET", URL: fmt.Sprintf("https://fixture.example/discovery/%d", id), Status: 200, FetchedAt: time.Now()}}, acquisition.Stats{}))
	raw, _ := json.Marshal(out)
	s.Operations = append(s.Operations, agentdata.Operation{ID: id + 1, Key: "tool:" + out.CallID, Kind: "inspect_source", Stage: "succeeded", ResultJSON: string(raw)})
	s.HostProgress = advanceHostProgressForTest(s.HostProgress, *s)
}

func TestExplorationRecoveryIsAutomaticAndSurvivesRestoration(t *testing.T) {
	for _, format := range []string{"<article>Different navigation %d</article>", `{"unrelated_page":%d}`} {
		t.Run(format, func(t *testing.T) {
			s := progressReadState()
			s.Spec = &TaskSpec{Goal: "deliver supported facts", MinRecords: 3, RequiredFields: []FieldRequirement{{Name: "body", Type: "string"}}}
			s.HostProgress = advanceHostProgressForTest(HostProgress{}, s)
			// The recovery review must arrive while the model can still act on it,
			// and the run must still end if it never does. Both bounds derive from
			// the calibrated window rather than a literal round number.
			reviewAt := int(explorationReviewWindow(s))
			stopAt := int(2 * explorationReviewWindow(s))
			if reviewAt >= stopAt {
				t.Fatalf("review window %d leaves no room before the stop at %d", reviewAt, stopAt)
			}
			sawReview := false
			for n := 0; n < stopAt+5; n++ {
				appendExploration(&s, format)
				// Process restoration cannot erase the host's exploratory debt.
				raw, _ := json.Marshal(s.HostProgress)
				_ = json.Unmarshal(raw, &s.HostProgress)
				action, err := RunTaskLoop(context.Background(), s)
				if err != nil {
					t.Fatal(err)
				}
				facts, err := contextFacts(s)
				if err != nil {
					t.Fatal(err)
				}
				var view map[string]json.RawMessage
				_ = json.Unmarshal(facts, &view)
				if n >= reviewAt && len(view["exploration_recovery"]) > 0 {
					sawReview = true
				}
				if action.Kind == "finish" {
					if !sawReview {
						t.Fatal("run ended without ever issuing recovery feedback", n)
					}
					// The stop must come from the recovery bound, not before the
					// model ever had a window to act on the review.
					if s.HostProgress.ExplorationRounds < int64(stopAt) || action.Status != "failed" || !action.HostStop {
						t.Fatal("no initial investigation/recovery allowance", action, n, s.HostProgress.ExplorationRounds)
					}
					return
				}
			}
			t.Fatal("endless distinct pages still renew the run")
		})
	}
}

func TestReadingCannotErasePreviousExplorationOrHideNewFetches(t *testing.T) {
	s := progressReadState()
	s.Spec = &TaskSpec{Goal: "deliver supported facts", MinRecords: 3}
	s.HostProgress = advanceHostProgressForTest(HostProgress{}, s)
	for n := 0; n < 10; n++ {
		appendExploration(&s, "<p>Navigation %d</p>")
	}
	debt := s.HostProgress.ExplorationRounds
	for n := 0; n < 20; n++ {
		addProgressRead(&s, n*100, "new bounded text")
		s.HostProgress = advanceHostProgressForTest(s.HostProgress, s)
	}
	if s.HostProgress.ExplorationRounds != debt {
		t.Fatal("necessary reading erased previous exploration or accumulated false debt", s.HostProgress)
	}
	// Batching a new fetch with a read must not gain a reading-only exemption, so
	// the delivery review still ends the run. Run past the calibrated stop bound.
	stopAt := int(2 * explorationReviewWindow(s))
	for n := 0; n < stopAt+5; n++ {
		// One proposal fetching AND reading must not gain a reading-only exemption.
		appendExploration(&s, "<p>Another new page %d</p>")
		id := uint64(len(s.Operations) + 1)
		out, _ := toolData(ToolCall{ID: fmt.Sprint(id), Function: FunctionCall{Name: "read_data"}}, map[string]any{"handle": "seed", "offset": 3000 + n*100, "text": "another bounded text", "view_version": "plain-text-v1"})
		raw, _ := json.Marshal(out)
		s.Operations = append(s.Operations, agentdata.Operation{ID: id, Kind: "read_data", Stage: "succeeded", ResultJSON: string(raw)})
		s.HostProgress = advanceHostProgressForTest(s.HostProgress, s)
	}
	action, err := RunTaskLoop(context.Background(), s)
	if err != nil || action.Kind != "finish" {
		t.Fatal("batching every new page with a read bypassed delivery review", action, err)
	}
}

func TestRecordProvenanceChurnCannotRenewDelivery(t *testing.T) {
	s := progressReadState()
	s.Result = &TaskResult{Records: []MergedRecord{{SchemaVersion: "v1", Fields: map[string]any{"body": "same supported body"}, Sources: []Provenance{{TaskID: 1}}}}}
	s.HostProgress = advanceHostProgressForTest(HostProgress{}, s)
	appendExploration(&s, "<p>Navigation %d</p>")
	s.Result.Records[0].Sources = append(s.Result.Records[0].Sources, Provenance{TaskID: 2, FetchedAt: time.Now()})
	s.HostProgress = advanceHostProgressForTest(s.HostProgress, s)
	if s.HostProgress.ExplorationRounds != 1 {
		t.Fatal("new task/fetch identity for unchanged facts renewed delivery", s.HostProgress)
	}
	s.Result.Records[0].Fields["body"] = "new supported body"
	s.HostProgress = advanceHostProgressForTest(s.HostProgress, s)
	if s.HostProgress.ExplorationRounds != 0 {
		t.Fatal("new accepted fact did not renew delivery", s.HostProgress)
	}
}
