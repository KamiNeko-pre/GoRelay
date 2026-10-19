package agentbench

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/redis/go-redis/v9"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/config"
	"go-lua-crawler/internal/queue"
	"go-lua-crawler/internal/repository"
	"go-lua-crawler/internal/service"
	task "go-lua-crawler/internal/task"
	"go-lua-crawler/internal/worker"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Opt-in live test driver. All production publish/consume/claim/commit/ACK code
// is used. The only injected fault is one execution outage before external IO.
type liveQueueWorker struct {
	service      *aiapp.Service
	client       *redis.Client
	processor    *worker.Processor
	stream       string
	recoveryCase string
	faultTask    uint64
	retainedTask uint64
	restarted    bool
	evidenceDir  string
}

func newLiveQueueWorker(t *testing.T, s *aiapp.Service, dir string) *liveQueueWorker {
	t.Helper()
	addr := os.Getenv("GORELAY_LIVE_REDIS_ADDR")
	if addr == "" {
		return nil
	}
	p := &liveQueueWorker{service: s, client: redis.NewClient(&redis.Options{Addr: addr}), stream: "gorelay-live-" + agentdata.Token(), recoveryCase: os.Getenv("GORELAY_LIVE_RECOVERY_CASE"), evidenceDir: dir}
	ctx := context.Background()
	if err := p.client.Ping(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	if err := s.Data.DB.AutoMigrate(&task.TaskEvent{}); err != nil {
		t.Fatal(err)
	}
	// The schema and stream are isolated; the publisher never scans project data.
	configDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(configDir, "configs"), 0700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"redis": map[string]any{"addr": addr, "stream": p.stream}})
	if err := os.WriteFile(filepath.Join(configDir, "configs", "config.yaml"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(configDir)
	config.InitConfig()
	old := queue.Client
	queue.Client = p.client
	t.Cleanup(func() { queue.Client = old; p.client.Del(context.Background(), p.stream); p.client.Close() })
	if err := p.client.XGroupCreateMkStream(ctx, p.stream, "live", "0").Err(); err != nil {
		t.Fatal(err)
	}
	cfg := worker.DefaultConfig(p.stream, "live", "worker")
	var err error
	p.processor, err = worker.NewProcessor(p.client, cfg, func(ctx context.Context, child task.CrawlTask) (repository.PersistTaskResult, error) {
		if p.recoveryCase != "" && p.faultTask == 0 {
			r, err := p.service.Data.Run(ctx, child.RunID)
			if err != nil {
				return nil, err
			}
			if strings.Contains(r.RequestKey, ":"+p.recoveryCase+":") {
				rows, err := p.service.ChildResults(ctx, r.ID)
				if err != nil {
					return nil, err
				}
				for _, row := range rows {
					if row.Task.ID != child.ID && row.Task.Status == task.StatusSucceeded {
						p.faultTask = child.ID
						p.retainedTask = row.Task.ID
						return nil, fmt.Errorf("experiment: injected one worker execution outage before source B external IO")
					}
				}
			}
		}
		return p.service.ExecuteTask(ctx, child)
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func verifyRetainedChild(retained task.CrawlTask, children []aiapp.ChildResult, ops []agentdata.Operation) error {
	attempts, completedA := 0, 0
	for _, op := range ops {
		if op.Kind == "worker_attempt" {
			var out struct {
				TaskID uint64 `json:"task_id"`
			}
			if err := json.Unmarshal([]byte(op.ResultJSON), &out); err != nil {
				return err
			}
			if out.TaskID == retained.ID {
				attempts++
			}
		}
	}
	for _, row := range children {
		if row.Task.URL == retained.URL && row.Task.Status == task.StatusSucceeded {
			completedA++
		}
	}
	if retained.Status != task.StatusSucceeded || attempts != 1 || completedA != 1 {
		return fmt.Errorf("recovery duplicated/lost successful A: attempts=%d completed_same_url=%d", attempts, completedA)
	}
	return nil
}

func TestRecordedRecoveryRejectsDuplicateRetainedChild(t *testing.T) {
	path := os.Getenv("GORELAY_RECOVERY_REPLAY_EVIDENCE")
	if path == "" {
		t.Skip("recorded live recovery evidence required; no inference is dispatched")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var trial Trial
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if err = json.Unmarshal([]byte(line), &trial); err != nil {
			t.Fatal(err)
		}
	}
	var view struct {
		Children []aiapp.ChildResult `json:"children"`
	}
	if err = json.Unmarshal(trial.Snapshot, &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Children) != 3 || view.Children[0].Task.Status != task.StatusSucceeded || view.Children[1].Task.Status != task.StatusFailed || view.Children[2].Task.Status != task.StatusSucceeded {
		t.Fatal("expected observed A/B-failed/B-recovered evidence")
	}
	retained := view.Children[0].Task
	if err = verifyRetainedChild(retained, view.Children, trial.Operations); err != nil {
		t.Fatal(err)
	}
	duplicate := view.Children[0]
	duplicate.Task.ID += 10000
	if verifyRetainedChild(retained, append(view.Children, duplicate), trial.Operations) == nil {
		t.Fatal("duplicate A accepted")
	}
	// A different newly successful source is legitimate; do not suppress B/C.
	duplicate.Task.URL = "https://another.example/releases"
	if err = verifyRetainedChild(retained, append(view.Children, duplicate), trial.Operations); err != nil {
		t.Fatal("independent successful source rejected", err)
	}
}

func (p *liveQueueWorker) once(ctx context.Context) error {
	if _, err := service.PublishPendingTaskOutboxes(ctx, 8); err != nil {
		return err
	}
	groups, err := p.client.XReadGroup(ctx, &redis.XReadGroupArgs{Group: "live", Consumer: "worker", Streams: []string{p.stream, ">"}, Count: 1, Block: -1}).Result()
	if err == redis.Nil {
		return nil
	}
	if err != nil {
		return err
	}
	for _, group := range groups {
		for _, message := range group.Messages {
			if err = p.processor.ProcessNewMessage(ctx, message); err != nil {
				return err
			}
			if p.faultTask != 0 && !p.restarted {
				var child task.CrawlTask
				if err = p.service.Data.DB.First(&child, p.faultTask).Error; err != nil {
					return err
				}
				before, err := p.service.Data.Run(ctx, child.RunID)
				if err != nil {
					return err
				}
				children, err := p.service.ChildResults(ctx, child.RunID)
				if err != nil {
					return err
				}
				db := p.service.Data.DB
				p.service = aiapp.NewService(db)
				p.service.Configure()
				after, err := p.service.Data.Run(ctx, child.RunID)
				if err != nil {
					return err
				}
				if before.BudgetJSON != after.BudgetJSON || before.ID != after.ID {
					return fmt.Errorf("restart changed durable run/budget")
				}
				raw, _ := json.MarshalIndent(map[string]any{"mode": "real_model_redis_runner_service_instance_restart", "fault": "one worker outage before B external IO", "full_process_restart": false, "before": before, "after": after, "children": children, "fault_task_id": p.faultTask}, "", "  ")
				if err = os.WriteFile(filepath.Join(p.evidenceDir, "recovery-checkpoint.json"), raw, 0600); err != nil {
					return err
				}
				p.restarted = true
			}
		}
	}
	return nil
}
