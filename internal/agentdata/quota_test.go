package agentdata

import (
	"context"
	"errors"
	"go-lua-crawler/internal/testdb"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDailyAdmissionQuotaIsDurableConcurrentAndIdempotent(t *testing.T) {
	s := NewStore(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now()
	r, err := s.CreateRunLimited(ctx, "first", "goal", "{}", "{}", now.Add(time.Hour), 2, now)
	if err != nil {
		t.Fatal(err)
	}
	if same, err := s.CreateRunLimited(ctx, "first", "goal", "{}", "{}", now.Add(time.Hour), 2, now); err != nil || same.ID != r.ID {
		t.Fatal("retry charged quota")
	}
	var successes atomic.Int32
	var wg sync.WaitGroup
	for n := 0; n < 8; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.CreateRunLimited(ctx, Token(), "goal", "{}", "{}", now.Add(time.Hour), 2, now)
			if err == nil {
				successes.Add(1)
			} else if !errors.Is(err, ErrDailyQuota) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatalf("quota raced: %d", successes.Load())
	}
	if _, err := NewStore(s.DB).CreateRunLimited(ctx, Token(), "goal", "{}", "{}", now.Add(time.Hour), 2, now); !errors.Is(err, ErrDailyQuota) {
		t.Fatal("restart reset quota")
	}
	if _, err := s.CreateRunLimited(ctx, "next-day", "goal", "{}", "{}", now.Add(48*time.Hour), 2, now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
}
