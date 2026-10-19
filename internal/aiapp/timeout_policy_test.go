package aiapp

import (
	"encoding/json"
	"testing"
	"time"

	"go-lua-crawler/internal/agentdata"
)

func TestDisabledTaskTimeboxDoesNotExpireNewCrawlerBudget(t *testing.T) {
	t.Setenv("GORELAY_RUN_TIMEOUT", "0s")
	duration, err := configuredRunTimeout()
	if err != nil || duration != 0 {
		t.Fatal("disabled task timebox rejected", duration, err)
	}
	if _, err := NewRunBudget(BudgetSnapshot{LoopVersion: 5, Limits: DefaultBudgetLimits()}); err != nil {
		t.Fatal("normal crawler requires an arbitrary timebox", err)
	}
	t.Setenv("GORELAY_RUN_TIMEOUT", "19m")
	if duration, err := configuredRunTimeout(); err != nil || duration.Minutes() != 19 {
		t.Fatal("configured difficulty budget ignored", duration, err)
	}
}

func TestParentDeadlineIsNotAnExternalBusinessKeyFactor(t *testing.T) {
	first, second := time.Now(), time.Now().Add(time.Hour)
	a := CreateRunRequest{RequestKey: "same-task", Goal: "collect facts", EntryURLs: []string{"https://source.example/items"}, ParentDeadline: &first}
	b := a
	b.ParentDeadline = &second
	if agentdata.Hash(a) != agentdata.Hash(b) {
		t.Fatal("elapsed parent time changed business request identity")
	}
	raw, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	var restored CreateRunRequest
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if len(restored.EntryURLs) != 1 || restored.EntryURLs[0] != a.EntryURLs[0] || restored.ParentDeadline != nil {
		t.Fatal("original URLs lost or private execution metadata entered model input")
	}
}
