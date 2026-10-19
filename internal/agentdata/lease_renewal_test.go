package agentdata

import (
	"context"
	"errors"
	"go-lua-crawler/internal/testdb"
	"testing"
	"time"
)

func TestRunLeaseRenewalPreservesOwnerAndCannotReviveExpiredOrCancelledRun(t *testing.T) {
	s := NewStore(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, key := range []string{"slow search batch", "slow acquisition batch"} {
		r, err := s.CreateRun(ctx, key, "goal", `{}`, `{}`, time.Now().Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		owner, err := s.Claim(ctx, r.ID, 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.RenewRunLease(ctx, r.ID, "wrong-token", time.Minute); !errors.Is(err, ErrConflict) {
			t.Fatalf("wrong owner: %v", err)
		}
		if err := s.RenewRunLease(ctx, r.ID, owner.Token, time.Minute); err != nil {
			t.Fatal(err)
		}
		after, _ := s.Run(ctx, r.ID)
		if after.Token != owner.Token || !after.LeaseUntil.After(*owner.LeaseUntil) {
			t.Fatal("lease/owner not preserved")
		}
		if err := s.Cancel(ctx, r.ID); err != nil {
			t.Fatal(err)
		}
		if err := s.RenewRunLease(ctx, r.ID, owner.Token, time.Minute); !errors.Is(err, ErrConflict) {
			t.Fatalf("cancelled run revived: %v", err)
		}
	}
	r, _ := s.CreateRun(ctx, "expired", "goal", `{}`, `{}`, time.Now().Add(time.Minute))
	owner, _ := s.Claim(ctx, r.ID, time.Second)
	if err := s.DB.Model(&Run{}).Where("id = ?", r.ID).Update("lease_until", time.Now().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.RenewRunLease(ctx, r.ID, owner.Token, time.Minute); !errors.Is(err, ErrConflict) {
		t.Fatalf("expired lease revived: %v", err)
	}
}
