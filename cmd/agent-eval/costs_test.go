package main

import (
	"encoding/json"
	"go-lua-crawler/internal/agentbench"
	"go-lua-crawler/internal/aiapp"
	"os"
	"path/filepath"
	"testing"
)

func TestCostsExportKeepsGroupsAndDeduplicatesRestartObservations(t *testing.T) {
	dir := t.TempDir()
	trials := filepath.Join(dir, "trials.jsonl")
	prices := filepath.Join(dir, "prices.json")
	out := filepath.Join(dir, "costs.json")
	good := true
	row := agentbench.Trial{CaseID: "a", Variant: "code", Mode: "fixture", Correct: &good, Status: "completed", Accounting: &aiapp.RunAccounting{IndexSettled: true}}
	raw, _ := json.Marshal(row)
	os.WriteFile(trials, append(append(raw, '\n'), append(raw, '\n')...), 0600)
	os.WriteFile(prices, []byte(`{"currency":"fixture","date":"fixture","models":{},"search_per_call":null}`), 0600)
	if err := exportCosts(trials, prices, out); err != nil {
		t.Fatal(err)
	}
	var results []struct {
		Trials   int                     `json:"trials"`
		Estimate agentbench.CostEstimate `json:"estimate"`
	}
	data, _ := os.ReadFile(out)
	if err := json.Unmarshal(data, &results); err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Trials != 1 || results[0].Estimate.Total == nil {
		t.Fatalf("restart observation charged twice or lost: %s", data)
	}
}
