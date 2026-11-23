package information

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestStudioLiveParentDoesNotDisplayHistoricalErrorAsPause(t *testing.T) {
	active := time.Now().Add(time.Minute)
	expired := time.Now().Add(-time.Minute)
	for _, tc := range []struct {
		status  string
		stage   string
		lease   *time.Time
		outcome string
		want    string
	}{
		{"running", "inspect_api", &active, "", "running"},
		{"running", "inspect_api", &expired, "", "queued"},
		{"blocked_provider", "inspect_api", &active, "", "blocked_provider"},
		{"completed", "completed", nil, "partial", "partial"},
		{"completed", "completed", nil, "completed", "completed"},
		{"cancelled", "inspect_api", nil, "", "cancelled"},
	} {
		got := studioRun(Request{Status: tc.status, Stage: tc.stage, LeaseUntil: tc.lease, LastError: "historical child wait", ResultJSON: `{"outcome":"invented"}`}, tc.outcome)
		if got["status"] != tc.want {
			t.Fatalf("stage %s: got %s want %s", tc.stage, got["status"], tc.want)
		}
	}
}

func TestStudioStatusDoesNotEmbedDebugLedger(t *testing.T) {
	s, _ := retrievalService(t)
	ctx := context.Background()
	r := Request{ID: "status-debug-boundary", Stage: "failed"}
	debug := strings.Repeat("PRIVATE_DEBUG_PAYLOAD", 64)
	if err := s.DB.Create(&Step{RequestID: r.ID, Key: "debug", Kind: "route_model", Stage: "succeeded", InputJSON: string(mustMarshal(map[string]any{"private": debug})), ResultJSON: string(mustMarshal(map[string]any{"private": debug}))}).Error; err != nil {
		t.Fatal(err)
	}
	out, err := s.studioSnapshot(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(mustMarshal(out)), "PRIVATE_DEBUG_PAYLOAD") {
		t.Fatal("default user status serialized raw request/result ledger")
	}
}

func TestStudioDebugPaginationDoesNotRepeatExhaustedStreams(t *testing.T) {
	s, _ := retrievalService(t)
	ctx := context.Background()
	r := Request{ID: "debug-page", Stage: "failed"}
	for i := 0; i < 5; i++ {
		if err := s.DB.Create(&Step{RequestID: r.ID, Key: string(rune('a' + i)), Kind: "route_model", Stage: "succeeded", InputJSON: "{}", ResultJSON: "{}"}).Error; err != nil {
			t.Fatal(err)
		}
	}
	seen := map[uint64]bool{}
	page := studioDebugPage{Limit: 2}
	for n := 0; n < 3; n++ {
		out, err := s.studioDetails(ctx, r, page)
		if err != nil {
			t.Fatal(err)
		}
		for _, step := range out["routing_steps"].([]Step) {
			if seen[step.ID] {
				t.Fatal("page repeated a diagnostic record")
			}
			seen[step.ID] = true
		}
		next := out["next"].(map[string]uint64)
		page.StepBefore = next["step_before"]
		if n < 2 && !out["has_more"].(bool) {
			t.Fatal("history ended early")
		}
		if n == 2 && out["has_more"].(bool) {
			t.Fatal("history has no end")
		}
	}
	if len(seen) != 5 {
		t.Fatal("diagnostic history lost")
	}
}
