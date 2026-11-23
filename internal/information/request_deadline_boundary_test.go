package information

import (
	"context"
	"errors"
	"fmt"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/testdb"
	"testing"
	"time"
)

func TestCreatedRequestMatchesStoredDeadlineAndIdempotentRetry(t *testing.T) {
	s := &Service{DB: testdb.Open(t)}
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	for _, seconds := range []int64{90, 1200, 0} {
		t.Run(fmt.Sprint(seconds), func(t *testing.T) {
			input := Input{RequestKey: fmt.Sprintf("deadline-%d", seconds), Goal: "collect independently versioned facts", TimeoutSeconds: &seconds}
			fresh, err := s.Create(context.Background(), input)
			if err != nil {
				t.Fatal(err)
			}
			stored, err := s.Request(context.Background(), fresh.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !fresh.Deadline.Equal(stored.Deadline) {
				t.Errorf("first response differs from authoritative SQL deadline: fresh=%s stored=%s delta=%s", fresh.Deadline, stored.Deadline, fresh.Deadline.Sub(stored.Deadline))
			}
			retry, err := s.Create(context.Background(), input)
			if err != nil || retry.ID != fresh.ID || !retry.Deadline.Equal(fresh.Deadline) {
				t.Errorf("identical request returned a different deadline: err=%v first=%s retry=%s", err, fresh.Deadline, retry.Deadline)
			}
			changed := input
			changed.Goal = "different legitimate task contract"
			if _, err := s.Create(context.Background(), changed); !errors.Is(err, agentdata.ErrConflict) {
				t.Fatalf("different input silently reused request key: %v", err)
			}
		})
	}
}

func TestExpiredInformationRequestsSettleWithoutDependencyWake(t *testing.T) {
	s, _ := retrievalService(t)
	ctx := context.Background()
	now := time.Now()
	future := now.Add(time.Hour)
	wait := string(mustMarshal(informationWait{WakeAt: &future}))
	cases := []struct {
		status   string
		stage    string
		deadline time.Time
		wait     string
		expire   bool
	}{
		{"waiting_dependency", "wait_crawler", now.Add(-time.Minute), wait, true},
		{"running", "blocked_provider", now.Add(-time.Minute), "", true},
		{"needs_input", "needs_input", now.Add(-time.Minute), "", true},
		{"waiting_dependency", "wait_crawler", future, wait, false},
		{"running", "blocked_provider", time.Time{}, "", false},
		{"needs_input", "needs_input", future, "", false},
		{"needs_input", "needs_input", time.Time{}, "", false},
		{"completed", "completed", now.Add(-time.Minute), "", false},
	}
	for i, tc := range cases {
		r := Request{ID: agentdata.Token(), RequestKey: fmt.Sprintf("expiry-%d", i), Status: tc.status, Stage: tc.stage, Deadline: tc.deadline, WaitJSON: tc.wait, InputJSON: "{}", DiscoveryJSON: `{"query":"retained"}`, ResultJSON: `{"retained":true}`}
		if err := s.DB.Create(&r).Error; err != nil {
			t.Fatal(err)
		}
		casesReady, err := s.readyRequests(ctx, 20)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, candidate := range casesReady {
			if candidate.ID == r.ID {
				found = true
			}
		}
		if found != tc.expire {
			t.Fatalf("stage=%s scheduled=%v want=%v", tc.stage, found, tc.expire)
		}
		if tc.expire {
			if err = s.Advance(ctx, r.ID); err != nil {
				t.Fatal(err)
			}
			stored, err := s.Request(ctx, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Stage != "expired" || stored.DiscoveryJSON != r.DiscoveryJSON || stored.ResultJSON != r.ResultJSON {
				t.Fatalf("expiry did not preserve evidence: %+v", stored)
			}
		}
	}
}
