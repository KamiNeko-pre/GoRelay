package information

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func (r Request) GoalForTest() string {
	var in Input
	_ = json.Unmarshal([]byte(r.InputJSON), &in)
	return in.Goal
}

func TestInformationReadinessSchedulesExpiredRequests(t *testing.T) {
	now := time.Now()
	future := now.Add(time.Hour)
	wait := string(mustMarshal(informationWait{WakeAt: &future}))
	for _, tc := range []struct {
		name, stage string
		deadline    time.Time
		wait        string
		ready       bool
	}{
		{"expired waiting", "wait_crawler", now.Add(-time.Minute), wait, true},
		{"expired blocked", "blocked_provider", now.Add(-time.Minute), "", true},
		{"expired input", "needs_input", now.Add(-time.Minute), "", true},
		{"future waiting", "wait_crawler", future, wait, false},
		{"retired index wait", "await_evidence", future, string(mustMarshal(informationWait{Stage: "await_evidence", WakeAt: &future})), true},
		{"no deadline blocked", "blocked_provider", time.Time{}, "", false},
		{"terminal expired", "completed", now.Add(-time.Minute), "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := (&Service{}).informationReady(context.Background(), Request{Stage: tc.stage, Deadline: tc.deadline, WaitJSON: tc.wait})
			if err != nil || got != tc.ready {
				t.Fatalf("ready=%v want=%v error=%v", got, tc.ready, err)
			}
		})
	}
}
