package agentbench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gin-gonic/gin"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/knowledge"
	"go-lua-crawler/internal/repository"
	task "go-lua-crawler/internal/task"
	"go-lua-crawler/internal/testdb"
	"gorm.io/gorm"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// Opt-in paid inference experiment. The production Agent/API and isolated Lua
// runner are used. Child SQL claims are pumped locally; Redis delivery is not
// part of this experiment and cannot be claimed from its results.
func TestLiveModelTaskExperiment(t *testing.T) {
	manifestPath := os.Getenv("GORELAY_LIVE_MANIFEST")
	if manifestPath == "" {
		t.Skip("explicit live manifest required; never spend quota in ordinary tests")
	}
	dir := os.Getenv("GORELAY_TEST_EVIDENCE_DIR")
	if dir == "" {
		t.Fatal("evidence output required")
	}
	read := func(path string, out any) {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(raw, out); err != nil {
			t.Fatal(err)
		}
	}
	var manifest Manifest
	read(manifestPath, &manifest)
	gold := map[string]GoldCase{}
	if p := os.Getenv("GORELAY_LIVE_GOLD"); p != "" {
		read(p, &gold)
	}
	db := testdb.Open(t)
	s := aiapp.NewService(db)
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	s.Configure()
	if s.Model == nil || s.Runner == nil {
		t.Fatal("real model and isolated runner must be configured")
	}
	var indexer *knowledge.Indexer
	if os.Getenv("GORELAY_LIVE_INDEXING") == "1" {
		if s.Retriever == nil {
			t.Fatal("real embedding/Qdrant must be configured")
		}
		dimensions, err := strconv.Atoi(os.Getenv("GORELAY_EMBED_DIMENSIONS"))
		if err != nil {
			t.Fatal(err)
		}
		collection := "gorelay_live_" + agentdata.Token()
		t.Setenv("GORELAY_QDRANT_COLLECTION", collection)
		q, err := knowledge.NewQdrant(os.Getenv("GORELAY_QDRANT_URL"), collection, os.Getenv("GORELAY_QDRANT_API_KEY"))
		if err != nil {
			t.Fatal(err)
		}
		s.Retriever.Vectors = q
		if err = q.EnsureCollection(context.Background(), dimensions); err != nil {
			t.Fatal(err)
		}
		chunkSize, chunkOverlap, err := aiapp.ChunkingSettingsFromEnv()
		if err != nil {
			t.Fatal(err)
		}
		// The caller supplies a unique experiment collection, never a project one.
		t.Cleanup(func() {
			req, _ := http.NewRequest("DELETE", os.Getenv("GORELAY_QDRANT_URL")+"/collections/"+os.Getenv("GORELAY_QDRANT_COLLECTION"), nil)
			if key := os.Getenv("GORELAY_QDRANT_API_KEY"); key != "" {
				req.Header.Set("api-key", key)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Error(err)
			} else {
				resp.Body.Close()
			}
		})
		indexer = &knowledge.Indexer{Store: s.Knowledge, Embed: s.Retriever.Embed, Vectors: q, Fingerprint: s.Fingerprint, ChunkSize: chunkSize, ChunkOverlap: chunkOverlap}
	}
	old := repository.DB
	repository.DB = db
	t.Cleanup(func() { repository.DB = old })
	queueWorker := newLiveQueueWorker(t, s, dir)
	loopService := s
	gin.SetMode(gin.TestMode)
	api := gin.New()
	aiapp.RegisterRoutes(api, s)
	server := httptest.NewServer(api)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			runs, err := s.Data.Pending(ctx, 8)
			if err != nil {
				t.Log(err)
				return
			}
			for _, run := range runs {
				if err = loopService.Advance(ctx, run.ID); err != nil {
					t.Logf("advance: %v", err)
				}
				if queueWorker != nil {
					continue
				}
				children, err := s.ChildResults(ctx, run.ID)
				if err != nil {
					t.Log(err)
					return
				}
				for _, row := range children {
					child := row.Task
					if child.Status != task.StatusQueued {
						continue
					}
					token, err := repository.ClaimCrawlTask(ctx, child, time.Minute, false)
					if err != nil {
						t.Log(err)
						continue
					}
					db.First(&child, child.ID)
					persist, execErr := s.ExecuteTask(ctx, child)
					status, code, message := task.StatusSucceeded, "", ""
					if execErr != nil {
						status, code, message = task.StatusFailed, "execution_failed", execErr.Error()
					}
					if err = repository.CompleteCrawlTask(ctx, child.ID, status, code, message, time.Now(), token, persist); err != nil {
						t.Log(err)
					}
				}
			}
			if queueWorker != nil {
				if err := queueWorker.once(ctx); err != nil {
					t.Logf("queue worker: %v", err)
					return
				}
				loopService = queueWorker.service
			}
			if indexer != nil {
				if err := indexer.Once(ctx); err != nil && !errors.Is(err, gorm.ErrRecordNotFound) && !errors.Is(err, context.Canceled) {
					t.Logf("indexer: %v", err)
				}
			}
		}
	}()
	file, err := os.OpenFile(filepath.Join(dir, "live.trials.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	cfg := ClientConfig{Endpoint: server.URL, Token: os.Getenv("GORELAY_ADMIN_TOKEN"), Batch: filepath.Base(dir), Variant: manifest.ExpectedActionMode, Repetitions: 1, PollInterval: 200 * time.Millisecond}
	emit := func(row Trial) error { return json.NewEncoder(file).Encode(row) }
	rows, err := Execute(ctx, cfg, manifest, gold, emit)
	if err == nil && os.Getenv("GORELAY_LIVE_FOLLOWUP_MANIFEST") != "" {
		// Follow-up state shares the capabilities and indexed evidence actually
		// created above. Only frozen host reuse controls change, never AI choices.
		var next Manifest
		read(os.Getenv("GORELAY_LIVE_FOLLOWUP_MANIFEST"), &next)
		t.Setenv("GORELAY_DISABLE_KNOWLEDGE_REUSE", "false")
		more, followErr := Execute(ctx, cfg, next, gold, emit)
		rows = append(rows, more...)
		err = followErr
	}
	if err == nil && indexer != nil {
		// Reconcile charged asynchronous work after it settles, retaining the
		// original delivery time and judgment. Never trigger another paid run.
		for n := 0; n < 100; n++ {
			works, e := s.Knowledge.Works(ctx)
			if e != nil {
				err = e
				break
			}
			pending := false
			for _, w := range works {
				pending = pending || w.Status == "pending" || w.Status == "running"
			}
			if !pending {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if err == nil {
			rows, err = RefreshAccounting(ctx, cfg, rows)
			if err == nil {
				raw, _ := json.MarshalIndent(rows, "", "  ")
				err = os.WriteFile(filepath.Join(dir, "live.reconciled-trials.json"), raw, 0600)
			}
		}
	}
	cancel()
	<-done
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.MarshalIndent(Summarize(rows), "", "  ")
	if err = os.WriteFile(filepath.Join(dir, "live.summary.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		t.Logf("case=%s status=%s correct=%v duration_ms=%d", row.CaseID, row.Status, row.Correct, row.DurationMS)
		if row.Status != "completed" || row.Correct == nil || !*row.Correct {
			t.Error(fmt.Sprintf("real experiment did not deliver verified correct facts: %s %v", row.Status, row.Reasons))
		}
	}
	if queueWorker != nil && queueWorker.recoveryCase != "" && !queueWorker.restarted {
		t.Error("requested multi-source recovery checkpoint was never reached")
	}
	if queueWorker != nil && queueWorker.restarted {
		var retained task.CrawlTask
		if err = db.First(&retained, queueWorker.retainedTask).Error; err != nil {
			t.Fatal(err)
		}
		ops, err := s.Data.Operations(context.Background(), retained.RunID)
		if err != nil {
			t.Fatal(err)
		}
		children, err := s.ChildResults(context.Background(), retained.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if err = verifyRetainedChild(retained, children, ops); err != nil {
			t.Error(err)
		}
	}
}
