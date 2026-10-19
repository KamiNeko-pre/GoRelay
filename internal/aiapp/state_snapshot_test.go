package aiapp

import (
	"context"
	"encoding/json"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/repository"
	task "go-lua-crawler/internal/task"
	"go-lua-crawler/internal/testdb"
	"gorm.io/gorm"
	"testing"
	"time"
)

func TestStateAndRoundReadChildrenOncePerSnapshot(t *testing.T) {
	for _, mode := range []string{"state", "round", "no_spec"} {
		t.Run(mode, func(t *testing.T) {
			s := NewService(testdb.Open(t))
			if err := s.Migrate(); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			spec := TaskSpec{Goal: "collect facts", SchemaVersion: "v1", RequiredFields: []FieldRequirement{{Name: "name", Type: "string"}}, JoinKeys: []string{"name"}}
			request := CreateRunRequest{RequestKey: mode, Goal: spec.Goal}
			if mode != "no_spec" {
				request.Spec = &spec
			}
			r, err := s.Create(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			child := task.CrawlTask{RunID: r.ID, RequestKey: "child", Status: task.StatusSucceeded, URL: "https://example.com/docs"}
			if err := s.Data.DB.Create(&child).Error; err != nil {
				t.Fatal(err)
			}
			if err := s.Data.DB.Create(&agentdata.RunTask{RunID: r.ID, TaskID: child.ID}).Error; err != nil {
				t.Fatal(err)
			}
			record := Record{Key: "fact", SchemaVersion: "v1", Fields: map[string]any{"name": "A"}, Provenance: Provenance{URL: child.URL, SourceID: "fixture", FetchedAt: time.Now(), DataVersion: "v1", TaskID: child.ID}}
			raw, _ := json.Marshal(AcquisitionResult{Records: []Record{record}})
			if err := s.Data.DB.Create(&repository.CrawlResult{TaskID: child.ID, Payload: string(raw)}).Error; err != nil {
				t.Fatal(err)
			}
			counts := map[string]int{}
			if err := s.Data.DB.Callback().Query().After("gorm:query").Register("test:snapshot_count", func(db *gorm.DB) { counts[db.Statement.Table]++ }); err != nil {
				t.Fatal(err)
			}
			if mode == "round" {
				round := LoopRound{RoundID: agentdata.Token(), StartedAt: time.Now(), Action: "wait"}
				if err := s.finishLoopRound(ctx, r.ID, &round, nil); err != nil {
					t.Fatal(err)
				}
				if round.CoverageAfter == nil || !round.CoverageAfter.Satisfied {
					t.Fatalf("round lost coverage: %+v", round)
				}
			} else {
				state, err := s.state(ctx, r)
				if err != nil {
					t.Fatal(err)
				}
				if len(state.TaskResults) != 1 || len(state.TaskResults[0].Records) != 1 {
					t.Fatalf("snapshot lost raw receipts: %+v", state.TaskResults)
				}
				if mode == "state" && (state.Result == nil || !state.Result.Coverage.Satisfied) {
					t.Fatalf("coverage lost: %+v", state.Result)
				}
				if mode == "no_spec" && state.Result != nil {
					t.Fatal("unplanned run was evaluated")
				}
			}
			t.Logf("snapshot query counts: %+v", counts)
			for _, table := range []string{"run_tasks", "crawl_tasks", "crawl_results", "operations"} {
				if counts[table] != 1 {
					t.Errorf("%s read %d times, want one", table, counts[table])
				}
			}
			// A new snapshot must re-read the database; no cross-round caching is allowed.
			counts = map[string]int{}
			if err := s.Data.DB.Create(&agentdata.Operation{RunID: r.ID, Key: "new-receipt", Kind: "fixture", Stage: "succeeded"}).Error; err != nil {
				t.Fatal(err)
			}
			child.Status = task.StatusFailed
			if err := s.Data.DB.Model(&child).Update("status", child.Status).Error; err != nil {
				t.Fatal(err)
			}
			fresh, err := s.state(ctx, r)
			if err != nil {
				t.Fatal(err)
			}
			if len(fresh.TaskResults) != 1 || fresh.TaskResults[0].Task.Status != task.StatusFailed || len(fresh.TaskResults[0].Records) != 0 {
				t.Fatalf("stale child result reused: %+v", fresh.TaskResults)
			}
			if fresh.Result != nil && fresh.Result.Coverage.Satisfied {
				t.Fatal("failed child still satisfies coverage")
			}
			if len(fresh.Operations) != 1 || fresh.Operations[0].Key != "new-receipt" {
				t.Fatal("new snapshot reused an old operation list")
			}
		})
	}
}
