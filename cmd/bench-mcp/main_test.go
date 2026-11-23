package main

import (
	"context"
	"testing"
)

func TestBenchSimulationInvariants(t *testing.T) {
	rep, err := runSimulation(context.Background())
	if err != nil {
		t.Fatalf("runSimulation failed: %v", err)
	}
	if !rep.HTTPAPIPromoted || !rep.CheckpointResumedPage2 || !rep.DriftBlockedOverwrite {
		t.Fatalf("simulation invariants failed: %+v", rep)
	}
	if rep.PayloadReductionPct <= 90 {
		t.Fatalf("expected >90%% payload projection reduction, got %.2f%%", rep.PayloadReductionPct)
	}
}
