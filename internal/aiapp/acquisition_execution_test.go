package aiapp

import (
	"context"
	"encoding/json"
	"errors"
	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/engine"
	"go-lua-crawler/internal/knowledge"
	"go-lua-crawler/internal/repository"
	task "go-lua-crawler/internal/task"
	"go-lua-crawler/internal/testdb"
	"gorm.io/gorm"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAcquisitionQuarantinesSchemaFailureButProtectsTransientAndNewVersion(t *testing.T) {
	for _, name := range []string{"wrong-type", "missing-field", "valid", "timeout", "superseded"} {
		t.Run(name, func(t *testing.T) {
			db := testdb.Open(t)
			s := NewService(db)
			if err := s.Migrate(); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			r, err := s.Create(ctx, CreateRunRequest{RequestKey: "schema", Goal: "collect"})
			if err != nil {
				t.Fatal(err)
			}
			spec := TaskSpec{Goal: "collect", SchemaVersion: "v1", RequiredFields: []FieldRequirement{{Name: "name", Type: "string"}}}
			schema, _ := json.Marshal(spec)
			a, err := s.Data.SaveArtifact(ctx, agentdata.ProgramArtifact{Code: "code", Kind: "adapter", OutputSchema: string(schema), ScopeJSON: `["example.com"]`})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.Data.Publish(ctx, agentdata.SourceVersion{SourceID: "source", ArtifactHash: a.Hash}, "", 0); err != nil {
				t.Fatal(err)
			}
			doc, err := knowledge.NewDocument(knowledge.Input{Source: "source", SourceKey: "item", URL: "https://example.com/", Title: "item", Body: "previous facts", Format: "text"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.Knowledge.Put(ctx, doc, s.Fingerprint); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if name == "timeout" {
					w.WriteHeader(http.StatusBadGateway)
					json.NewEncoder(w).Encode(RunnerResponse{Error: &RunnerError{Code: "http_timeout", Message: "transient"}})
					return
				}
				row := map[string]any{"name": "A", "source_url": "https://example.com/"}
				if name == "wrong-type" || name == "superseded" {
					row["name"] = 42
				}
				if name == "missing-field" {
					delete(row, "name")
				}
				if name == "superseded" {
					next, e := s.Data.SaveArtifact(ctx, agentdata.ProgramArtifact{Code: "new code", Kind: "adapter", OutputSchema: string(schema), ScopeJSON: a.ScopeJSON})
					if e == nil {
						_, e = s.Data.Publish(ctx, agentdata.SourceVersion{SourceID: "source", ArtifactHash: next.Hash}, a.Hash, 1)
					}
					if e != nil {
						t.Error(e)
					}
				}
				json.NewEncoder(w).Encode(RunnerResponse{Result: &engine.ProgramResult{Collection: map[string]any{"items": []any{row}}, Stats: acquisition.Stats{Requests: 2, Bytes: 75}, Requests: []acquisition.Receipt{{URL: "https://example.com/", Method: http.MethodGet, Status: 200, FetchedAt: time.Now()}}}})
			}))
			defer server.Close()
			s.Runner, err = NewRunnerClient(server.URL, "test-token")
			if err != nil {
				t.Fatal(err)
			}
			params, _ := json.Marshal(childParameters{Spec: spec, SourceID: "source"})
			persist, err := s.ExecuteTask(ctx, task.CrawlTask{ID: 1, RunID: r.ID, URL: "https://example.com/", ProgramHash: a.Hash, ProgramParams: string(params)})
			if name == "valid" {
				if err != nil || persist == nil {
					t.Fatalf("valid rejected: %v", err)
				}
			} else if err == nil {
				t.Fatal("invalid acquisition accepted")
			}
			current, _ := s.Data.Run(ctx, r.ID)
			var budget BudgetSnapshot
			json.Unmarshal([]byte(current.BudgetJSON), &budget)
			wantRequests, wantBytes := int64(2), int64(75)
			if name == "timeout" {
				limits := programLimitsForRun(r)
				wantRequests, wantBytes = int64(limits.MaxRequests), limits.MaxBytes
			}
			if budget.Spent.HTTPRequests != wantRequests || budget.Spent.HTTPBytes != wantBytes {
				t.Fatalf("worker settlement incorrect after %s: %+v", name, budget.Spent)
			}
			head, err := s.Data.SourceHead(ctx, "source")
			if err != nil {
				t.Fatal(err)
			}
			knowledgeHead, err := s.Knowledge.Head(ctx, doc.ID)
			if err != nil {
				t.Fatal(err)
			}
			bad := name == "wrong-type" || name == "missing-field"
			if bad && (head.Status != "quarantined" || knowledgeHead.Active) {
				t.Fatalf("bad version still usable: %+v %+v", head, knowledgeHead)
			}
			if !bad && (head.Status != "active" || !knowledgeHead.Active) {
				t.Fatalf("normal/transient/new version incorrectly quarantined: %+v %+v", head, knowledgeHead)
			}
			if name == "superseded" && head.ArtifactHash == a.Hash {
				t.Fatal("new head lost")
			}
		})
	}
}

func TestAcquisitionResultDocumentWorkAndTerminalCommitTogether(t *testing.T) {
	db := testdb.Open(t)
	s := NewService(db)
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	old := repository.DB
	repository.DB = db
	t.Cleanup(func() { repository.DB = old })
	ctx := context.Background()
	r, _ := s.Create(ctx, CreateRunRequest{RequestKey: "projection", Goal: "collect"})
	child := task.CrawlTask{RequestKey: "child", RunID: r.ID, Status: task.StatusRunning, RunToken: "owner", ProgramHash: "version"}
	if err := db.Create(&child).Error; err != nil {
		t.Fatal(err)
	}
	records := []Record{{Key: "entity", SchemaVersion: "v1", Fields: map[string]any{"title": "release", "body": "public changelog body"}, Provenance: Provenance{SourceID: "composition", URL: "https://example.com/release", FetchedAt: time.Now(), DataVersion: "version", TaskID: child.ID}}}
	persist := s.persistAcquisition(child, AcquisitionResult{Records: records})
	injected := func(tx *gorm.DB) error {
		if err := persist(tx); err != nil {
			return err
		}
		return errors.New("cut before commit")
	}
	if err := repository.CompleteCrawlTask(ctx, child.ID, task.StatusSucceeded, "", "", time.Now(), "owner", injected); err == nil {
		t.Fatal("rollback fault ignored")
	}
	for _, model := range []any{&repository.CrawlResult{}, &knowledge.DocumentVersion{}, &knowledge.IndexWork{}} {
		var n int64
		db.Model(model).Count(&n)
		if n != 0 {
			t.Fatalf("partial commit: %T %d", model, n)
		}
	}
	var current task.CrawlTask
	db.First(&current, child.ID)
	if current.Status != task.StatusRunning {
		t.Fatal("terminal state leaked")
	}
	if err := repository.CompleteCrawlTask(ctx, child.ID, task.StatusSucceeded, "", "", time.Now(), "owner", persist); err != nil {
		t.Fatal(err)
	}
	for _, model := range []any{&repository.CrawlResult{}, &knowledge.DocumentVersion{}, &knowledge.IndexWork{}} {
		var n int64
		db.Model(model).Count(&n)
		if n != 1 {
			t.Fatalf("missing projection: %T %d", model, n)
		}
	}
	if err := repository.CompleteCrawlTask(ctx, child.ID, task.StatusSucceeded, "", "", time.Now(), "old-token", persist); !errors.Is(err, repository.ErrTaskTransitionConflict) {
		t.Fatalf("old owner: %v", err)
	}
}
func TestCancelledRunRejectsLateAcquisitionProjection(t *testing.T) {
	db := testdb.Open(t)
	s := NewService(db)
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r, _ := s.Create(ctx, CreateRunRequest{RequestKey: "cancel", Goal: "goal"})
	s.Data.Cancel(ctx, r.ID)
	err := db.Transaction(func(tx *gorm.DB) error {
		return s.persistAcquisition(task.CrawlTask{ID: 1, RunID: r.ID}, AcquisitionResult{})(tx)
	})
	if !errors.Is(err, agentdata.ErrConflict) {
		t.Fatalf("cancel ignored: %v", err)
	}
}
