package information

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"go-lua-crawler/internal/agentdata"
)

// One integration boundary check covers the experimental switch matrix, the
// protected default path and admission replay. It is not a task-success score.
func TestInformationExperimentControlsCoverSQLAndRetrievalAndStayFrozen(t *testing.T) {
	s, v := retrievalService(t)
	ctx := context.Background()
	initial := Asset{ID: "initial-native", Version: "v1", Kind: "api", SourceID: "catalog", ScopeJSON: `["source.example"]`, Active: true, VerifiedAt: time.Now()}
	if err := s.DB.Create(&initial).Error; err != nil {
		t.Fatal(err)
	}
	for _, flags := range [][2]bool{{false, false}, {true, false}, {false, true}, {true, true}} {
		t.Setenv("GORELAY_DISABLE_CAPABILITY_REUSE", fmt.Sprint(flags[0]))
		t.Setenv("GORELAY_DISABLE_KNOWLEDGE_REUSE", fmt.Sprint(flags[1]))
		t.Setenv("GORELAY_ACTION_MODE", "code")
		in := Input{RequestKey: fmt.Sprintf("matrix-%t-%t", flags[0], flags[1]), Goal: "collect observed release", SourceScope: []string{"source.example"}}
		r, err := s.Create(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		before := v.queries
		lookup, err := s.Preflight(ctx, r.ID)
		if err != nil {
			t.Fatal(err)
		}
		if flags[1] && (v.queries != before || lookup.RetrievalStatus != "disabled_by_experiment") || !flags[1] && v.queries != before+1 {
			t.Fatalf("lookup reuse switch ineffective: %v %+v", flags, lookup)
		}
		found := false
		for _, a := range lookup.Assets {
			if a.ID == initial.ID {
				found = true
			}
		}
		if found == flags[0] {
			t.Fatalf("SQL catalog bypassed capability switch: %v %+v", flags, lookup.Assets)
		}
		if allowed, err := s.canUseAsset(ctx, r, initial); err != nil || allowed == flags[0] {
			t.Fatalf("execution boundary bypass: %v %v", allowed, err)
		}
		fresh := Asset{ID: agentdata.Token(), Version: "fresh", Kind: "api"}
		if allowed, err := s.canUseAsset(ctx, r, fresh); err != nil || !allowed {
			t.Fatal("new construction blocked")
		}
		candidate, _ := json.Marshal(initial)
		if err := s.DB.Create(&AssetTrial{ID: agentdata.Token(), RequestID: r.ID, AssetJSON: string(candidate)}).Error; err != nil {
			t.Fatal(err)
		}
		if allowed, err := s.canUseAsset(ctx, r, initial); err != nil || !allowed {
			t.Fatal("newly trialled equivalent plan blocked")
		}
		t.Setenv("GORELAY_DISABLE_CAPABILITY_REUSE", "invalid-current-value")
		recovered, err := s.Create(ctx, in)
		if err != nil || recovered.ID != r.ID || recovered.ExperimentJSON != r.ExperimentJSON {
			t.Fatalf("admission replay reinterpreted frozen controls: %v", err)
		}
	}
}
