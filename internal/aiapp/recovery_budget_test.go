package aiapp

import (
	"go-lua-crawler/internal/agentdata"
	"testing"
)

func TestRecoveryFitsInsideTheExistingModelWindow(t *testing.T) {
	for _, limit := range []int64{8, 12} {
		s := efficiencyState()
		s.Budget.Limits = DefaultBudgetLimits()
		s.Budget.Limits.ModelCalls = limit
		s.Budget.Spent.ModelCalls = limit - 3
		s.Operations = []agentdata.Operation{{ID: 1, Kind: "model", Stage: "succeeded", ResultJSON: `{"message":{"role":"assistant"}}`}}
		host := HostProgress{NoProgressModelRounds: limit - 3}
		p := advanceDeliveryRecovery(DeliveryRecovery{}, s, host, false, "")
		if p.Phase != "correcting" {
			t.Fatalf("limit %d exhausted before correction: %+v", limit, p)
		}
		s.Budget.Spent.ModelCalls = limit - 1
		p = advanceDeliveryRecovery(p, s, host, false, "")
		if p.Phase != "concluding" {
			t.Fatalf("no call reserved for conclusion: %+v", p)
		}
		// Accepted output renews the host-owned window. Old lifetime spending must
		// not prematurely conclude productive work.
		s.Budget.ProgressBase = s.Budget.Spent
		host.NoProgressModelRounds = 0
		if p = advanceDeliveryRecovery(DeliveryRecovery{}, s, host, false, ""); p.Phase != "" {
			t.Fatal("productive renewed window was preempted", p)
		}
		s.Budget.ProgressBase = CallCost{}
		s.Budget.Unlimited = true
		host.NoProgressModelRounds = limit - 1
		if p = advanceDeliveryRecovery(DeliveryRecovery{}, s, host, false, ""); p.Phase != "" {
			t.Fatal("unlimited timeboxed run inherited a finite call ceiling", p)
		}
	}
}
