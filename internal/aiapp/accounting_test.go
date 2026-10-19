package aiapp

import (
	"context"
	"go-lua-crawler/internal/knowledge"
	"go-lua-crawler/internal/testdb"
	"testing"
)

func TestRunAccountingIncludesIndexWorkAndExplicitInterventions(t *testing.T) {
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r, err := s.Create(ctx, CreateRunRequest{RequestKey: "accounting", Goal: "goal"})
	if err != nil {
		t.Fatal(err)
	}
	s.Data.DB.Create(&knowledge.IndexWork{DocID: "doc", Epoch: 1, Generation: "g", Fingerprint: "f", OriginRunID: r.ID, Status: "pending"})
	event := HumanIntervention{Key: "h1", Kind: "source_hint", Reason: "operator supplied a correct source"}
	first, err := s.RecordIntervention(ctx, r, event)
	if err != nil {
		t.Fatal(err)
	}
	if same, err := s.RecordIntervention(ctx, r, event); err != nil || same.ID != first.ID {
		t.Fatal("human event replay duplicated")
	}
	got, err := s.Accounting(ctx, r)
	if err != nil || len(got.Interventions) != 1 || len(got.IndexWork) != 1 || got.IndexSettled {
		t.Fatalf("pending indexing/counted intervention lost: %+v %v", got, err)
	}
	event.Kind = "goal_input"
	event.Key = "bad"
	if _, err = s.RecordIntervention(ctx, r, event); err == nil {
		t.Fatal("initial goal counted as material intervention")
	}
}
