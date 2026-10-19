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

func TestSuccessfulNewReadsCannotEraseDeliveryDebt(t *testing.T) {
	s := progressReadState()
	s.Budget.LoopVersion = 5
	s.HostProgress = advanceHostProgressForTest(HostProgress{}, s)
	for i := 1; i <= 4; i++ {
		id := uint64(len(s.Operations) + 1)
		s.Operations = append(s.Operations, recordedModelRound(s, id))
		out, _ := toolData(ToolCall{ID: fmt.Sprint(id), Function: FunctionCall{Name: "inspect_source"}}, inspectResponse(acquisition.Response{Body: fmt.Sprintf("<article><h2>Different source %d</h2></article>", i), Receipt: acquisition.Receipt{Method: "GET", URL: fmt.Sprintf("https://public.example/%d", i), Status: 200, FetchedAt: time.Now()}}, acquisition.Stats{}))
		raw, _ := json.Marshal(out)
		s.Operations = append(s.Operations, agentdata.Operation{ID: id + 1, Key: "tool:" + out.CallID, Kind: "inspect_source", Stage: "succeeded", ResultJSON: string(raw)})
		s.HostProgress = advanceHostProgressForTest(s.HostProgress, s)
		if s.HostProgress.NoProgressModelRounds < int64(i) {
			t.Fatalf("successful fetch erased output debt: round=%d debt=%d", i, s.HostProgress.NoProgressModelRounds)
		}
	}
}

func TestSuccessfulStructuralPagesContinueUntilEvidenceStopsOrDeadline(t *testing.T) {
	s, recovery := evidenceRecoveryState()
	for n := int64(0); n < explorationReviewWindow(s)+deliveryIdleRoundLimit+2; n++ {
		page := inspectResponse(acquisition.Response{Body: fmt.Sprintf("<h2>Source %d</h2>", n), Receipt: acquisition.Receipt{Method: "GET", URL: fmt.Sprintf("https://public.example/%d", n), Status: 200, FetchedAt: time.Now()}}, acquisition.Stats{})
		appendEvidenceRound(&s, "correcting", "inspect_source", fmt.Sprintf(`{"url":"https://public.example/%d"}`, n), page)
		recovery = advanceDeliveryRecovery(recovery, s, HostProgress{}, false, "")
		if recovery.Phase != "correcting" || recovery.NoEvidenceRounds != 0 {
			t.Fatal("fresh observed structure was treated as evidence-free looping", recovery)
		}
	}
	s.Budget.WorkDeadline = time.Now().Add(-time.Second)
	if expired := advanceDeliveryRecovery(recovery, s, HostProgress{}, false, ""); expired.Phase != "concluding" {
		t.Fatal("new evidence extended the explicit deadline", expired)
	}
}

func TestSuccessfulThinkingIsComparedEvenWhenStructureChanges(t *testing.T) {
	e := &fixedEmbedder{groups: [][]string{{"I will keep reading the listing"}}, vectors: [][]float32{{1, 0, 0}}}
	svc := serviceWithEmbedder(e)
	s := progressReadState()
	s.Budget.LoopVersion = 5
	h := svc.advanceHostProgress(context.Background(), HostProgress{}, s)
	for i := 1; i <= 3; i++ {
		id := uint64(len(s.Operations) + 1)
		call := ToolCall{ID: fmt.Sprintf("successful-%d", i), Type: "function", Function: FunctionCall{Name: "read_data", Arguments: fmt.Sprintf(`{"handle":"seed","view":"json","pointer":"/text","offset":%d}`, i)}}
		raw, _ := json.Marshal(ChatResponse{Message: Message{Role: "assistant", ReasoningContent: "I will keep reading the listing to decide later", ToolCalls: []ToolCall{call}}})
		s.Operations = append(s.Operations, agentdata.Operation{ID: id, Kind: "model", Stage: "succeeded", ResultJSON: string(raw)})
		out, _ := toolData(call, map[string]any{"handle": "seed", "view_version": "plain-text-v1", "offset": i, "text": fmt.Sprint(i)})
		raw, _ = json.Marshal(out)
		s.Operations = append(s.Operations, agentdata.Operation{ID: id + 1, Key: "tool:" + call.ID, Kind: "read_data", Stage: "succeeded", ResultJSON: string(raw)})
		s.HostProgress = h
		h = svc.advanceHostProgress(context.Background(), h, s)
		if i >= 2 && e.calls != i-1 {
			t.Fatalf("round %d not embedded despite successful calls/changed structure: calls=%d", i, e.calls)
		}
	}
	if h.Recovery.Phase != "correcting" {
		t.Fatalf("high similarity without delivery produced no correction: %+v", h.Recovery)
	}
	before := e.calls
	s.HostProgress = h
	_ = svc.advanceHostProgress(context.Background(), h, s)
	if e.calls != before {
		t.Fatal("same completed round paid for another comparison")
	}
	s.HostProgress = h
	facts, _ := contextFacts(s)
	if !json.Valid(facts) {
		t.Fatal("invalid next model context")
	}
	brief := buildCorrectionBrief(s, h.Recovery.Phase, h.Recovery.Reason)
	for _, key := range []string{"direction_is_wrong", "still_required", "saved_but_unused", "do_this_next"} {
		if brief[key] == nil {
			t.Errorf("loop feedback missing %s", key)
		}
	}
}
