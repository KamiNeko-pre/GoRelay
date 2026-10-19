package aiapp

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/config"
	"go-lua-crawler/internal/engine"
	"go-lua-crawler/internal/queue"
	"go-lua-crawler/internal/repository"
	"go-lua-crawler/internal/service"
	"go-lua-crawler/internal/task"
	"go-lua-crawler/internal/testdb"
	"go-lua-crawler/internal/worker"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type monitorClosureChat struct {
	mu      sync.Mutex
	fixture closureChat
}

func (p *monitorClosureChat) Chat(ctx context.Context, r ChatRequest) (ChatResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.fixture.Chat(ctx, r)
}
func (p *monitorClosureChat) Calls() int { p.mu.Lock(); defer p.mu.Unlock(); return p.fixture.calls }

// Normal HTTP submission followed only by observation. The model and upstream
// HTTP are labeled fixtures; SQL, Redis, Lua execution, Worker and Agent loop are real.
func TestMonitorNormalEntryRuntimeWorkerAndReuseClosure(t *testing.T) {
	runMonitorReplayClosure(t, monitorReplayCase{
		goal: "collect release facts", firstBody: `{"rows":[{"name":"Alpha","version":"1.0"}]}`, secondBody: `{"rows":[{"name":"Alpha","version":"2.0"}]}`,
		condition: MonitorCondition{Field: "version", Operator: "eq", Value: json.RawMessage(`"2.0"`)}, changedField: "version", changedValue: "2.0", inboxCount: 1, evidenceFile: "monitor-reuse-closure.json",
	})
}

func TestMonitorHTMLDiscoveryNewIdentityAndTypedFieldsReuseClosure(t *testing.T) {
	goal := "discover public event availability"
	// An unplanned Run is host-bound to schema v1; field names/types can vary.
	// A model proposal cannot rewrite that immutable version during plan_task.
	spec := TaskSpec{Goal: goal, SchemaVersion: "v1", EntityField: "event_id", JoinKeys: []string{"event_id"}, RequiredFields: []FieldRequirement{{Name: "event_id", Type: "string"}, {Name: "seats", Type: "number"}, {Name: "available", Type: "boolean"}}}
	runMonitorReplayClosure(t, monitorReplayCase{goal: goal, spec: &spec,
		code:       `local body=assert(http_get(TARGET_URL));local id=assert(html_find(body,"#event-id"));local raw=assert(html_find(body,"#seats"));local available=assert(html_find(body,"#available"));local seats=assert(tonumber(raw));return true,{items={{event_id=id,seats=seats,available=available=="true",source_url=TARGET_URL}}}`,
		firstBody:  `<html><span id="event-id">first-event</span><span id="seats">2</span><span id="available">true</span></html>`,
		secondBody: `<html><span id="event-id">new-event</span><span id="seats">5</span><span id="available">true</span></html>`,
		condition:  MonitorCondition{Field: "seats", Operator: "gte", Value: json.RawMessage(`4`)}, changedField: "event_id", changedValue: "new-event", inboxCount: 2, evidenceFile: "monitor-html-discovery-closure.json",
	})
}

type monitorReplayCase struct {
	goal, code, firstBody, secondBody, changedField, changedValue, evidenceFile string
	spec                                                                        *TaskSpec
	condition                                                                   MonitorCondition
	inboxCount                                                                  int
}

func runMonitorReplayClosure(t *testing.T, fixture monitorReplayCase) {
	t.Helper()
	addr := os.Getenv("GORELAY_LIVE_REDIS_ADDR")
	if addr == "" {
		t.Skip("GORELAY_LIVE_REDIS_ADDR required")
	}
	db := testdb.Open(t)
	s := NewService(db)
	s.Fingerprint = "monitor-replay"
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&task.TaskEvent{}); err != nil {
		t.Fatal(err)
	}
	oldDB := repository.DB
	repository.DB = db
	t.Cleanup(func() { repository.DB = oldDB })
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	client := redis.NewClient(&redis.Options{Addr: addr})
	defer client.Close()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	stream := "monitor-closure-" + agentdata.Token()
	defer client.Del(context.Background(), stream)
	oldClient := queue.Client
	queue.Client = client
	t.Cleanup(func() { queue.Client = oldClient })
	dir := t.TempDir()
	raw, _ := json.Marshal(map[string]any{"redis": map[string]any{"stream": stream, "addr": addr}})
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LUA_SPIDER_CONFIG", configPath)
	config.InitConfig()
	if err := client.XGroupCreateMkStream(ctx, stream, "monitor", "0").Err(); err != nil {
		t.Fatal(err)
	}
	var changed atomic.Bool
	runner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in RunnerRequest
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		body := fixture.firstBody
		if changed.Load() {
			body = fixture.secondBody
		}
		replay := acquisition.NewReplay([]acquisition.ReplayEntry{{Method: "GET", URL: in.Program.TargetURL, Status: 200, Body: body, FetchedAt: time.Now()}})
		in.Limits.Timeout = 10 * time.Second
		result, err := engine.RunProgram(r.Context(), in.Program, in.Limits, replay.Fetch)
		if err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		json.NewEncoder(w).Encode(RunnerResponse{Result: &result})
	}))
	defer runner.Close()
	s.Runner, _ = NewRunnerClient(runner.URL, "replay-token")
	model := &monitorClosureChat{fixture: closureChat{goal: fixture.goal, spec: fixture.spec, code: fixture.code}}
	s.Model = model
	processor, err := worker.NewProcessor(client, worker.DefaultConfig(stream, "monitor", "worker"), s.ExecuteTask)
	if err != nil {
		t.Fatal(err)
	}
	runtimeDone := make(chan error, 1)
	go func() { runtimeDone <- s.Run(ctx) }()
	workerDone := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				workerDone <- ctx.Err()
				return
			case <-ticker.C:
				if _, e := service.PublishPendingTaskOutboxes(ctx, 8); e != nil {
					workerDone <- e
					return
				}
				groups, e := client.XReadGroup(ctx, &redis.XReadGroupArgs{Group: "monitor", Consumer: "worker", Streams: []string{stream, ">"}, Count: 8, Block: -1}).Result()
				if e == redis.Nil {
					continue
				}
				if e != nil {
					workerDone <- e
					return
				}
				for _, group := range groups {
					for _, message := range group.Messages {
						if e = processor.ProcessNewMessage(ctx, message); e != nil {
							workerDone <- e
							return
						}
					}
				}
			}
		}
	}()
	defer func() { cancel(); <-runtimeDone; <-workerDone }()
	gin.SetMode(gin.TestMode)
	t.Setenv("GORELAY_ADMIN_TOKEN", "monitor-closure")
	router := gin.New()
	RegisterRoutes(router, s)
	request := func(method, path, payload string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(payload))
		req.Header.Set("Authorization", "Bearer monitor-closure")
		req.Header.Set("Content-Type", "application/json")
		out := httptest.NewRecorder()
		router.ServeHTTP(out, req)
		return out
	}
	input, _ := json.Marshal(CreateMonitorRequest{RequestKey: "watch", Goal: fixture.goal, SourceScope: []string{"fixture.example"}})
	response := request("POST", "/api/v1/agent/monitors", string(input))
	if response.Code != 201 {
		t.Fatal(response.Code, response.Body)
	}
	var m Monitor
	json.Unmarshal(response.Body.Bytes(), &m)
	refresh := func(key string) agentdata.Run {
		t.Helper()
		out := request("POST", fmt.Sprintf("/api/v1/agent/monitors/%d/refresh", m.ID), fmt.Sprintf(`{"request_key":"%s"}`, key))
		if out.Code != 202 {
			t.Fatal(out.Code, out.Body)
		}
		var r agentdata.Run
		json.Unmarshal(out.Body.Bytes(), &r)
		return r
	}
	await := func(r agentdata.Run) agentdata.Run {
		t.Helper()
		for {
			stored, e := s.Data.Run(ctx, r.ID)
			if e != nil {
				t.Fatal(e)
			}
			if stored.IsTerminal() {
				if stored.Status != "completed" {
					t.Fatal(stored.Status, stored.LastError)
				}
				view, e := s.MonitorView(ctx, m.ID)
				if e != nil {
					t.Fatal(e)
				}
				if view.ActiveRunID == "" {
					return stored
				}
			}
			select {
			case e := <-runtimeDone:
				runtimeDone <- e
				t.Fatal("runtime stopped", e)
			case e := <-workerDone:
				workerDone <- e
				t.Fatal("worker stopped", e)
			case <-ctx.Done():
				t.Fatal("closure timeout", stored.Status)
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	first := await(refresh("first"))
	calls := model.Calls()
	if calls != 6 {
		t.Fatal("scripted setup calls", calls)
	}
	response = request("GET", fmt.Sprintf("/api/v1/agent/monitors/%d", m.ID), "")
	var view MonitorView
	json.Unmarshal(response.Body.Bytes(), &view)
	if view.Config.Spec == nil || !view.ExecutionPlanReady {
		t.Fatal("autonomously frozen schema not exposed for user review")
	}
	// A user changes a post-acquisition filter via the normal API. This must
	// not rerun discovery/generation for an unchanged acquisition contract.
	view.Config.ConditionGroups = [][]MonitorCondition{{fixture.condition}}
	updated, _ := json.Marshal(UpdateMonitorRequest{Config: &view.Config})
	response = request("PATCH", fmt.Sprintf("/api/v1/agent/monitors/%d", m.ID), string(updated))
	if response.Code != 200 {
		t.Fatal("condition update failed", response.Body)
	}
	changed.Store(true)
	second := await(refresh("second"))
	var result TaskResult
	json.Unmarshal([]byte(second.ResultJSON), &result)
	if model.Calls() != calls || len(result.Records) != 1 || fmt.Sprint(result.Records[0].Fields[fixture.changedField]) != fixture.changedValue {
		t.Fatal("reuse did not refresh without model", model.Calls(), result)
	}
	var budget BudgetSnapshot
	json.Unmarshal([]byte(second.BudgetJSON), &budget)
	if budget.Spent.ModelCalls != 0 || budget.Spent.SearchCalls != 0 || budget.Spent.HTTPRequests == 0 {
		t.Fatal("incorrect replay accounting", budget)
	}
	pending, e := client.XPending(ctx, stream, "monitor").Result()
	if e != nil || pending.Count != 0 {
		t.Fatal("Worker did not ACK committed results", pending, e)
	}
	response = request("GET", "/api/v1/agent/inbox", "")
	var inbox []MonitorInbox
	json.Unmarshal(response.Body.Bytes(), &inbox)
	if len(inbox) != fixture.inboxCount || !inbox[0].Active || !inbox[0].Verified {
		t.Fatal("changes missing or duplicated", response.Body)
	}
	if fixture.inboxCount == 2 && (!inbox[1].Active || inbox[1].Verified) {
		t.Fatal("unseen former identity was withdrawn or falsely refreshed", inbox[1])
	}
	if evidenceDir := os.Getenv("GORELAY_TEST_EVIDENCE_DIR"); evidenceDir != "" {
		raw, _ := json.MarshalIndent(map[string]any{"mode": "scripted_model_frozen_http_real_mysql_redis_lua_runtime", "real_model": false, "manual_execution_intervention": false, "first": first, "reused": second, "inbox": inbox, "first_scripted_model_calls": calls, "reuse_model_calls": model.Calls() - calls, "pending": pending.Count}, "", "  ")
		if e = os.WriteFile(filepath.Join(evidenceDir, fixture.evidenceFile), raw, 0600); e != nil {
			t.Fatal(e)
		}
	}
}
