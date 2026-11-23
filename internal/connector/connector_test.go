package connector

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gorelay/internal/acquisition"
	"gorelay/internal/contract"
	"gorelay/internal/engine"
	"gorelay/internal/fetchgov"
	"gorelay/internal/pipeline"
	"gorelay/internal/repository"
	taskmodel "gorelay/internal/task"
	"gorelay/internal/testdb"
)

type apiStubRequester struct {
	fn func(ctx context.Context, opts acquisition.RequestOptions) (acquisition.Response, error)
}

func (s *apiStubRequester) FetchRequest(ctx context.Context, opts acquisition.RequestOptions) (acquisition.Response, error) {
	return s.fn(ctx, opts)
}
func (s *apiStubRequester) Close() {}

func TestExecuteHTTPAPIPaginationCheckpointResumeAndCycleGuard(t *testing.T) {
	var fetchedURLs []string
	cfg := fetchgov.DefaultConfig()
	cfg.RespectRobots = false
	cfg.RequestsPerSec = 100
	cfg.Burst = 100
	cfg.RequesterFactory = func(acquisition.Policy) (fetchgov.Requester, error) {
		return &apiStubRequester{fn: func(_ context.Context, opts acquisition.RequestOptions) (acquisition.Response, error) {
			fetchedURLs = append(fetchedURLs, opts.URL)
			switch {
			case strings.Contains(opts.URL, "cursor=c2"):
				return acquisition.Response{
					Body:    `{"next_cursor":"c2","items":[{"id":"r2","name":"Item 2"}]}`,
					Receipt: acquisition.Receipt{URL: opts.URL, Status: 200, Headers: map[string]string{"content-type": "application/json"}},
				}, nil
			default:
				return acquisition.Response{
					Body:    `{"next_cursor":"c2","items":[{"id":"r1","name":"Item 1"}]}`,
					Receipt: acquisition.Receipt{URL: opts.URL, Status: 200, Headers: map[string]string{"content-type": "application/json"}},
				}, nil
			}
		}}, nil
	}
	client, err := fetchgov.NewSafeClient(cfg)
	if err != nil {
		t.Fatal(err)
	}

	var cur any = map[string]any{"cursor": "c2", "pages_done": 1}
	var savedCount atomic.Int32
	hooks := &engine.CheckpointHooks{
		Get: func(context.Context) (any, error) { return cur, nil },
		Save: func(_ context.Context, next any, rows []map[string]any, _ []acquisition.Response) error {
			cur = next
			savedCount.Add(int32(len(rows)))
			return nil
		},
	}

	spec := APISpec{
		URLTemplate:    "https://api.example.com/releases?org={org}",
		Method:         http.MethodGet,
		ItemsPath:      "/items",
		FieldMapping:   map[string]string{"id": "/id", "name": "/name"},
		Pagination:     "cursor",
		PageParam:      "cursor",
		NextCursorPath: "/next_cursor",
		MaxPages:       5,
	}

	rows, _, err := ExecuteHTTPAPI(context.Background(), client, spec, map[string]any{"org": "golang"}, "", 0, hooks)
	if err != nil {
		t.Fatalf("ExecuteHTTPAPI failed: %v", err)
	}
	if len(fetchedURLs) != 1 || !strings.Contains(fetchedURLs[0], "cursor=c2") {
		t.Fatalf("expected checkpoint resume directly at c2 without re-fetching page 1 or looping, got %v", fetchedURLs)
	}
	if len(rows) != 1 || rows[0]["id"] != "r2" || savedCount.Load() != 1 {
		t.Fatalf("unexpected resumed rows: %+v", rows)
	}
}

func TestCheckpointHubAuthAndHandlers(t *testing.T) {
	hub := NewCheckpointHub()
	unreg := hub.Register("42", "tok-secret", &engine.CheckpointHooks{
		Get:  func(context.Context) (any, error) { return map[string]any{"page": 3}, nil },
		Save: func(context.Context, any, []map[string]any, []acquisition.Response) error { return nil },
	})
	defer unreg()

	req := httptest.NewRequest(http.MethodPost, "/internal/checkpoints/42", strings.NewReader(`{"op":"get"}`))
	w := httptest.NewRecorder()
	hub.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without bearer token, got %d", w.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/internal/checkpoints/42", strings.NewReader(`{"op":"get"}`))
	req.Header.Set("Authorization", "Bearer tok-secret")
	w = httptest.NewRecorder()
	hub.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"page":3`) {
		t.Fatalf("expected 200 with page=3 cursor, got %d %s", w.Code, w.Body.String())
	}
}

func TestConnectorServiceFullLifecycleDB(t *testing.T) {
	db := testdb.Open(t)
	if err := repository.AutoMigrate(db, &taskmodel.CrawlTask{}, &taskmodel.TaskOutbox{}, &taskmodel.TaskEvent{}); err != nil {
		t.Fatal(err)
	}
	if err := pipeline.Migrate(db); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	prevDB := repository.DB
	repository.DB = db
	t.Cleanup(func() { repository.DB = prevDB })

	var simulateDrift atomic.Bool
	var upstreamHits atomic.Int32
	cfg := fetchgov.DefaultConfig()
	cfg.RespectRobots = false
	cfg.RequestsPerSec = 100
	cfg.Burst = 100
	cfg.RequesterFactory = func(acquisition.Policy) (fetchgov.Requester, error) {
		return &apiStubRequester{fn: func(_ context.Context, opts acquisition.RequestOptions) (acquisition.Response, error) {
			upstreamHits.Add(1)
			if simulateDrift.Load() {
				return acquisition.Response{
					Body:    `{"items":[{"unexpected_field":"broken"}]}`,
					Receipt: acquisition.Receipt{URL: opts.URL, Status: 200, Headers: map[string]string{"content-type": "application/json"}},
				}, nil
			}
			return acquisition.Response{
				Body:    `{"items":[{"id":"go1.26","notes":"Green tea GC improvements"}]}`,
				Receipt: acquisition.Receipt{URL: opts.URL, Status: 200, Headers: map[string]string{"content-type": "application/json"}},
			}, nil
		}}, nil
	}
	gov, err := fetchgov.NewSafeClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	snapDir := t.TempDir()
	svc, err := NewService(db, gov, pipeline.NewStore(db, snapDir), ServiceConfig{SnapshotDir: snapDir})
	if err != nil {
		t.Fatal(err)
	}
	svc.InlineExec = func(ctx context.Context, taskID uint64) error {
		tRow, err := repository.GetCrawlTask(ctx, taskID)
		if err != nil {
			return err
		}
		token, err := repository.ClaimCrawlTask(ctx, tRow, 30*time.Second, false)
		if err != nil {
			return err
		}
		tRow.RunToken = token
		persist, execErr := svc.ExecuteConnectorTask(ctx, tRow)
		st, fcode, lerr := taskmodel.StatusSucceeded, "", ""
		if execErr != nil {
			st, fcode, lerr = taskmodel.StatusFailed, "connector_exec", execErr.Error()
			if errors.Is(execErr, pipeline.ErrSilentDrift) {
				fcode = "silent_drift"
			}
		}
		return repository.CompleteCrawlTask(ctx, tRow.ID, st, fcode, lerr, time.Now(), token, persist)
	}

	specJSON, _ := json.Marshal(APISpec{
		URLTemplate:  "https://api.example.com/releases?tag={tag}",
		Method:       http.MethodGet,
		ItemsPath:    "/items",
		FieldMapping: map[string]string{"id": "/id", "notes": "/notes"},
	})
	outSchema := json.RawMessage(`{"entity_keys":["id"],"min_rows":1,"fields":[{"name":"id","type":"string","required":true},{"name":"notes","type":"string","required":true}]}`)

	// 1. Cold build v1
	res, err := svc.DeployAndRun(context.Background(), DeployAndRunInput{
		SourceID:     "go_releases",
		Kind:         KindData,
		Executor:     ExecutorHTTPAPI,
		TargetURL:    "https://api.example.com/releases",
		SpecOrCode:   string(specJSON),
		OutputSchema: outSchema,
		Params:       map[string]any{"tag": "latest"},
		WaitMS:       1000,
	}, "http://localhost:8080")
	if err != nil || res.Status != "succeeded" || res.Data == nil || res.Data.Returned != 1 {
		t.Fatalf("cold deploy failed: res=%+v err=%v", res, err)
	}
	if upstreamHits.Load() != 1 {
		t.Fatalf("DeployAndRun double-fetched upstream: got %d hits, want 1", upstreamHits.Load())
	}

	// 2. Fresh cache hit (0 HTTP)
	cacheRes, err := svc.RunConnector(context.Background(), RunConnectorInput{
		SourceID:        "go_releases",
		Params:          map[string]any{"tag": "latest"},
		FreshnessTTLSec: 600,
	}, "http://localhost:8080")
	if err != nil || cacheRes.HitType != HitCacheHit || cacheRes.Data.Returned != 1 {
		t.Fatalf("expected cache_hit, got %+v err=%v", cacheRes, err)
	}

	// 3. Simulate upstream drift -> marks degraded WITHOUT wiping old v1 current record!
	simulateDrift.Store(true)
	driftRes, _ := svc.RunConnector(context.Background(), RunConnectorInput{
		SourceID:     "go_releases",
		Params:       map[string]any{"tag": "latest"},
		ForceRefresh: true,
		WaitMS:       1000,
	}, "http://localhost:8080")
	if driftRes.Status != "failed" || driftRes.Drift == nil {
		t.Fatalf("expected silent drift failure, got %+v", driftRes)
	}
	oldRows, err := svc.Pipeline.DeliverRecordsPage(context.Background(), "go_releases", 0, "", 16384, 20, "http://localhost:8080")
	if err != nil || oldRows.Returned != 1 || oldRows.Records[0]["id"] != "go1.26" {
		t.Fatalf("old valid snapshot was corrupted after silent drift: %+v err=%v", oldRows, err)
	}
	var connRow Connector
	if err := db.First(&connRow, "source_id = ?", "go_releases").Error; err != nil || connRow.Status != StatusDegraded {
		t.Fatalf("expected connector status=degraded committed after drift rollback, got %+v err=%v", connRow, err)
	}
	var driftCount int64
	db.Model(&DriftEvent{}).Where("source_id = ?", "go_releases").Count(&driftCount)
	if driftCount == 0 {
		t.Fatalf("expected DriftEvent committed after rollback, got 0")
	}
}

func TestTrialOutcomeSingleFetchAndMultiPageResume(t *testing.T) {
	var hits atomic.Int32
	cfg := fetchgov.DefaultConfig()
	cfg.RespectRobots = false
	cfg.RequestsPerSec = 100
	cfg.Burst = 100
	cfg.RequesterFactory = func(acquisition.Policy) (fetchgov.Requester, error) {
		return &apiStubRequester{fn: func(_ context.Context, opts acquisition.RequestOptions) (acquisition.Response, error) {
			hits.Add(1)
			if strings.Contains(opts.URL, "page=2") {
				return acquisition.Response{
					Body:    `{"has_more":true,"items":[{"id":"p2","title":"Page 2"}]}`,
					Receipt: acquisition.Receipt{URL: opts.URL, Status: 200, Headers: map[string]string{"content-type": "application/json"}},
				}, nil
			}
			return acquisition.Response{
				Body:    `{"has_more":true,"items":[{"id":"p1","title":"Page 1"}]}`,
				Receipt: acquisition.Receipt{URL: opts.URL, Status: 200, Headers: map[string]string{"content-type": "application/json"}},
			}, nil
		}}, nil
	}
	gov, err := fetchgov.NewSafeClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	svc := &Service{SafeClient: gov}
	_, schema, err := contract.NormalizeOutputSchema(json.RawMessage(`{"entity_keys":["id"],"min_rows":1,"fields":[{"name":"id","type":"string","required":true},{"name":"title","type":"string","required":true}]}`))
	if err != nil {
		t.Fatal(err)
	}

	// Single-page spec: NextCursor must be nil (zero worker follow-up).
	singleSpec := APISpec{
		URLTemplate:  "https://api.example.com/items",
		Method:       http.MethodGet,
		ItemsPath:    "/items",
		FieldMapping: map[string]string{"id": "/id", "title": "/title"},
		Pagination:   "none",
	}
	singleOutcome, err := svc.validateAPITrial(context.Background(), singleSpec, nil, "", schema)
	if err != nil || singleOutcome.NextCursor != nil || len(singleOutcome.Rows) != 1 || hits.Load() != 1 {
		t.Fatalf("unexpected single-page trial outcome: %+v hits=%d err=%v", singleOutcome, hits.Load(), err)
	}

	// Multi-page spec (MaxPages=2): trial fetches page 1 ONLY and returns cursor for page 2.
	hits.Store(0)
	multiSpec := APISpec{
		URLTemplate:    "https://api.example.com/items",
		Method:         http.MethodGet,
		ItemsPath:      "/items",
		FieldMapping:   map[string]string{"id": "/id", "title": "/title"},
		Pagination:     "page",
		PageParam:      "page",
		NextCursorPath: "/has_more",
		MaxPages:       2,
	}
	multiOutcome, err := svc.validateAPITrial(context.Background(), multiSpec, nil, "", schema)
	if err != nil || multiOutcome.NextCursor == nil || len(multiOutcome.Rows) != 1 || hits.Load() != 1 {
		t.Fatalf("unexpected multi-page trial outcome: %+v hits=%d err=%v", multiOutcome, hits.Load(), err)
	}

	// Resuming ExecuteHTTPAPI with trial's NextCursor (PagesDone=1) must fetch ONLY page 2 (total 2 pages, respecting MaxPages=2).
	resumedRows, _, err := ExecuteHTTPAPI(context.Background(), gov, multiSpec, nil, "", 0, &engine.CheckpointHooks{
		Get: func(context.Context) (any, error) { return multiOutcome.NextCursor, nil },
	})
	if err != nil || hits.Load() != 2 || len(resumedRows) != 1 || resumedRows[0]["id"] != "p2" {
		t.Fatalf("resumed multi-page run did not honor PagesDone cap: rows=%+v hits=%d err=%v", resumedRows, hits.Load(), err)
	}

	// Lua trial using checkpoint.save must capture CheckpointedItems + FinalCheckpoint without re-fetching.
	luaCode := `
local body, err = http_get(TARGET_URL)
if err then return false, err end
local data = json_decode(body)
checkpoint.save({next_page = 2}, data.items)
return true, {items = {}}
`
	luaOutcome, err := svc.validateLuaTrial(context.Background(), "https://api.example.com/items", luaCode, nil, "", schema)
	if err != nil || len(luaOutcome.Rows) != 1 || luaOutcome.Rows[0]["id"] != "p1" || luaOutcome.NextCursor == nil {
		t.Fatalf("lua trial failed to capture checkpointed rows/cursor: %+v err=%v", luaOutcome, err)
	}
}
