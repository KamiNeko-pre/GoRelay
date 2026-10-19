package information

import (
	"context"
	"errors"
	"fmt"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/testdb"
	"testing"
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
