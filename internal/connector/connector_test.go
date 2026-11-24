package connector

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gorelay/internal/acquisition"
	"gorelay/internal/contract"
	"gorelay/internal/engine"
	"gorelay/internal/fetchgov"
	"gorelay/internal/pipeline"
	"gorelay/internal/probe"
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

	// Single-page spec with pagination="none" and {page} param MUST respect caller params["page"]=2.
	hits.Store(0)
	var seenURL string
	cfg.RequesterFactory = func(acquisition.Policy) (fetchgov.Requester, error) {
		return &apiStubRequester{fn: func(_ context.Context, opts acquisition.RequestOptions) (acquisition.Response, error) {
			seenURL = opts.URL
			return acquisition.Response{
				Body:    `{"items":[{"id":"p2","title":"Page 2"}]}`,
				Receipt: acquisition.Receipt{URL: opts.URL, Status: 200, Headers: map[string]string{"content-type": "application/json"}},
			}, nil
		}}, nil
	}
	gov2, err := fetchgov.NewSafeClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	noPagSpec := APISpec{
		URLTemplate:  "https://api.example.com/items?page={page}",
		Method:       http.MethodGet,
		ItemsPath:    "/items",
		FieldMapping: map[string]string{"id": "/id", "title": "/title"},
		Pagination:   "none",
	}
	rows2, _, err := ExecuteHTTPAPI(context.Background(), gov2, noPagSpec, map[string]any{"page": 2}, "", 0, nil)
	if err != nil || !strings.Contains(seenURL, "page=2") || len(rows2) != 1 || rows2[0]["id"] != "p2" {
		t.Fatalf("pagination=none clobbered caller params[page]: url=%q rows=%+v err=%v", seenURL, rows2, err)
	}
}

func TestReceiptModeQueryPushdownTargetOverrideAndProbeReuseDB(t *testing.T) {
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

	var upstreamHits atomic.Int32
	cfg := fetchgov.DefaultConfig()
	cfg.RespectRobots = false
	cfg.RequestsPerSec = 100
	cfg.Burst = 100
	cfg.RequesterFactory = func(acquisition.Policy) (fetchgov.Requester, error) {
		return &apiStubRequester{fn: func(_ context.Context, opts acquisition.RequestOptions) (acquisition.Response, error) {
			upstreamHits.Add(1)
			if strings.Contains(opts.URL, "page=999") {
				return acquisition.Response{
					Body:    `{"message":"Only the first 1000 results are available.","status":"422"}`,
					Receipt: acquisition.Receipt{URL: opts.URL, Status: 422, Headers: map[string]string{"content-type": "application/json"}},
				}, nil
			}
			return acquisition.Response{
				Body: `{"items":[
					{"tag":"v3.7.4","price":54.23,"body":"Release notes https://github.com/etcd-io/etcd/blob/main/CHANGELOG/CHANGELOG-3.7.md and ` + strings.Repeat("x", 400) + `"},
					{"tag":"v3.7.3","price":12.00,"body":"Minor fix"},
					{"tag":"v3.6.5","price":57.25,"body":"Older branch"},
					{"tag":"v3.5.9","price":22.00,"body":"Legacy"}
				]}`,
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
		}
		return repository.CompleteCrawlTask(ctx, tRow.ID, st, fcode, lerr, time.Now(), token, persist)
	}

	// 1. Probe caches response in memory for 60s; immediate DeployAndRun reuses it (total upstream hits = 1!).
	if _, err := svc.Prober.Probe(context.Background(), probe.ProbeRequest{URL: "https://api.example.org/releases", DetectAPI: true}); err != nil {
		t.Fatal(err)
	}
	if upstreamHits.Load() != 1 {
		t.Fatalf("expected 1 hit from Probe, got %d", upstreamHits.Load())
	}

	specJSON, _ := json.Marshal(APISpec{
		URLTemplate:  "https://api.example.org/releases",
		Method:       http.MethodGet,
		ItemsPath:    "/items",
		FieldMapping: map[string]string{"tag": "/tag", "price": "/price", "body": "/body"},
		Pagination:   "none",
	})
	outSchema := json.RawMessage(`{"entity_keys":["tag"],"min_rows":1,"fields":[{"name":"tag","type":"string","required":true},{"name":"price","type":"number","required":true},{"name":"body","type":"source_text"}]}`)

	deployRes, err := svc.DeployAndRun(context.Background(), DeployAndRunInput{
		SourceID:     "etcd_rel",
		Kind:         KindData,
		Executor:     ExecutorHTTPAPI,
		TargetURL:    "https://api.example.org/releases",
		SpecOrCode:   string(specJSON),
		OutputSchema: outSchema,
		WaitMS:       1000,
	}, "http://localhost:8080")
	if err != nil {
		t.Fatalf("deploy failed: %v", err)
	}
	if upstreamHits.Load() != 1 {
		t.Fatalf("expected DeployAndRun trial to reuse 60s probe cache (want 1 total HTTP hit, got %d)", upstreamHits.Load())
	}
	if deployRes.Data == nil || deployRes.Data.Mode != "receipt" || deployRes.Data.Total != 4 || deployRes.Data.Returned != 2 {
		t.Fatalf("expected compact receipt (total=4, returned=2), got %+v", deployRes.Data)
	}
	if len(deployRes.Data.ExportURLs) != 3 {
		t.Fatalf("expected 3 export URLs on receipt, got %+v", deployRes.Data.ExportURLs)
	}

	// 2. Cached query pushdown: filter price > 50, sort DESC, group by tag prefix, extract CHANGELOG URL (0 upstream HTTP!).
	q, err := pipeline.ParseRecordQuery(map[string]any{
		"where":        []map[string]any{{"field": "price", "op": "gt", "value": 50}},
		"order_by":     "-price",
		"fields":       []string{"tag", "price"},
		"group_by":     "tag",
		"group_prefix": 4,
		"limit":        2,
		"extract_urls": map[string]any{"field": "body", "pattern": "CHANGELOG"},
	})
	if err != nil {
		t.Fatal(err)
	}
	qRes, err := svc.RunConnector(context.Background(), RunConnectorInput{
		SourceID:        "etcd_rel",
		FreshnessTTLSec: 600,
		Query:           q,
	}, "http://localhost:8080")
	if err != nil || qRes.HitType != HitCacheHit || upstreamHits.Load() != 1 {
		t.Fatalf("expected 0-HTTP cache_hit query, got res=%+v hits=%d err=%v", qRes, upstreamHits.Load(), err)
	}
	if qRes.Data == nil || qRes.Data.Matched != 2 || qRes.Data.Groups["v3.7"] != 1 || qRes.Data.Records[0]["tag"] != "v3.6.5" {
		t.Fatalf("unexpected query result: %+v", qRes.Data)
	}
	if len(qRes.Data.ExtractedURLs) != 1 || !strings.Contains(qRes.Data.ExtractedURLs[0], "CHANGELOG-3.7.md") {
		t.Fatalf("expected extracted CHANGELOG URL, got %v", qRes.Data.ExtractedURLs)
	}

	// 3. target_url override on RunConnector bypasses cache, executes ?page=999 directly,
	//    returns HTTP 422 + JSON excerpt + active_version=1 + intact_cached_rows=4 without degrading active connector.
	overrideRes, err := svc.RunConnector(context.Background(), RunConnectorInput{
		SourceID:  "etcd_rel",
		TargetURL: "https://api.example.org/releases?page=999",
		WaitMS:    1000,
	}, "http://localhost:8080")
	if err != nil || overrideRes.Status != "failed" || overrideRes.ActiveVersion != 1 || overrideRes.IntactCachedRows != 4 {
		t.Fatalf("expected target_url override failure with intact cache info, got res=%+v err=%v", overrideRes, err)
	}
	if !strings.Contains(overrideRes.Error, "422") || !strings.Contains(overrideRes.Error, "Only the first 1000 results") {
		t.Fatalf("expected HTTP 422 JSON excerpt in error, got %q", overrideRes.Error)
	}
	var connRow Connector
	if err := db.First(&connRow, "source_id = ?", "etcd_rel").Error; err != nil || connRow.Status != StatusActive {
		t.Fatalf("target_url override must NOT degrade active connector, got %+v err=%v", connRow, err)
	}

	// 4. Trial failure with wrong schema mapping MUST return structured Drift.SampleRow (truncated) + intact_cached_rows.
	badSpecJSON, _ := json.Marshal(APISpec{
		URLTemplate:  "https://api.example.org/releases",
		Method:       http.MethodGet,
		Headers:      map[string]string{"Accept": "application/vnd.github+json"},
		ItemsPath:    "/items",
		FieldMapping: map[string]string{"wrong_col": "/tag", "body": "/body"},
		Pagination:   "none",
	})
	hitsBeforeCustomHeader := upstreamHits.Load()
	badRes, badErr := svc.DeployAndRun(context.Background(), DeployAndRunInput{
		SourceID:     "etcd_rel",
		Kind:         KindData,
		Executor:     ExecutorHTTPAPI,
		TargetURL:    "https://api.example.org/releases",
		SpecOrCode:   string(badSpecJSON),
		OutputSchema: outSchema,
		WaitMS:       1000,
	}, "http://localhost:8080")
	if badErr == nil {
		t.Fatal("expected trial validation error on missing required fields")
	}
	if upstreamHits.Load() != hitsBeforeCustomHeader+1 {
		t.Fatalf("expected custom-header APISpec to bypass plain probe cache (before=%d after=%d)", hitsBeforeCustomHeader, upstreamHits.Load())
	}
	if badRes.Drift == nil || badRes.Drift.Code != "SCHEMA_DRIFT_MISSING_FIELDS" || len(badRes.Drift.SampleRow) == 0 {
		t.Fatalf("expected structured Drift.SampleRow on trial failure for human-style debugging, got %+v", badRes.Drift)
	}
	if bodySample, _ := badRes.Drift.SampleRow["body"].(string); len(bodySample) > pipeline.DefaultReceiptFieldChars+10 {
		t.Fatalf("expected truncated sample row field, got %d chars", len(bodySample))
	}
	if badRes.ActiveVersion != 1 || badRes.IntactCachedRows != 4 {
		t.Fatalf("expected intact state preserved on failed trial, got v=%d rows=%d", badRes.ActiveVersion, badRes.IntactCachedRows)
	}
	svc.Pipeline.WaitSnapshots()
}

func TestMultiPageTrialMinRowsAndMultiQueryViews(t *testing.T) {
	db := testdb.Open(t)
	if err := db.AutoMigrate(&taskmodel.CrawlTask{}, &taskmodel.TaskOutbox{}); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	if err := pipeline.Migrate(db); err != nil {
		t.Fatal(err)
	}
	prevDB := repository.DB
	repository.DB = db
	t.Cleanup(func() { repository.DB = prevDB })

	var pageHits atomic.Int32
	cfg := fetchgov.DefaultConfig()
	cfg.RespectRobots = false
	cfg.RequestsPerSec = 100
	cfg.Burst = 100
	cfg.RequesterFactory = func(acquisition.Policy) (fetchgov.Requester, error) {
		return &apiStubRequester{fn: func(_ context.Context, opts acquisition.RequestOptions) (acquisition.Response, error) {
			pageHits.Add(1)
			parsedURL, _ := url.Parse(opts.URL)
			if parsedURL != nil && parsedURL.Query().Get("page") == "2" {
				if parsedURL.Query().Get("short") == "1" {
					return acquisition.Response{
						Body:    `[]`,
						Receipt: acquisition.Receipt{URL: opts.URL, Status: 200, Headers: map[string]string{"content-type": "application/json"}},
					}, nil
				}
				return acquisition.Response{
					Body: `[
						{"tag_name":"v3.7.2","published_at":"2026-09-01T00:00:00Z","body":"Release 3.7.2"},
						{"tag_name":"v3.7.1","published_at":"2026-07-23T00:00:00Z","body":"Oldest release"}
					]`,
					Receipt: acquisition.Receipt{URL: opts.URL, Status: 200, Headers: map[string]string{"content-type": "application/json"}},
				}, nil
			}
			return acquisition.Response{
				Body: `[
					{"tag_name":"v3.7.4","published_at":"2026-11-19T00:00:00Z","body":"See https://github.com/etcd-io/etcd/blob/main/CHANGELOG/CHANGELOG-3.7.md"},
					{"tag_name":"v3.6.17","published_at":"2026-10-01T00:00:00Z","body":"Release 3.6.17"}
				]`,
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
		}
		return repository.CompleteCrawlTask(ctx, tRow.ID, st, fcode, lerr, time.Now(), token, persist)
	}

	outSchema := json.RawMessage(`{
		"fields": [
			{"name":"tag_name","type":"string","required":true},
			{"name":"published_at","type":"string","required":true},
			{"name":"body","type":"source_text"}
		],
		"entity_keys": ["tag_name"],
		"min_rows": 4
	}`)
	specJSON, _ := json.Marshal(APISpec{
		URLTemplate:  "https://api.example.org/releases?per_page=2",
		Method:       http.MethodGet,
		ItemsPath:    "",
		FieldMapping: map[string]string{"tag_name": "tag_name", "published_at": "published_at", "body": "body"},
		Pagination:   "page",
		PageParam:    "page",
		MaxPages:     2,
	})
	// Deploy with min_rows=4 across 2 pages (2 rows/page) + multi-query views in ONE call.
	res, err := svc.DeployAndRun(context.Background(), DeployAndRunInput{
		SourceID:     "etcd_mp",
		Kind:         KindData,
		Executor:     ExecutorHTTPAPI,
		SpecOrCode:   string(specJSON),
		OutputSchema: outSchema,
		WaitMS:       2000,
		Queries: map[string]*pipeline.RecordQuery{
			"timeline": {Fields: []string{"tag_name", "published_at"}, OrderBy: "published_at", GroupBy: "tag_name", GroupPrefix: 4, Limit: 10},
			"v374":     {Where: []pipeline.FilterCondition{{Field: "tag_name", Op: "eq", Value: "v3.7.4"}}, ExtractURLs: &pipeline.URLExtractSpec{Field: "body"}, Limit: 1},
		},
	}, "http://localhost:8080")
	if err != nil {
		t.Fatalf("multi-page DeployAndRun with min_rows=4 should pass page-1 trial and fetch page 2, got err=%v res=%+v", err, res)
	}
	if res.Status != "succeeded" || res.Data == nil || res.Data.Mode != "multi_query" || res.Data.Total != 4 {
		t.Fatalf("expected succeeded multi_query with 4 total rows, got res=%+v data=%+v", res, res.Data)
	}
	if pageHits.Load() != 2 {
		t.Fatalf("expected exactly 2 upstream page fetches (no double fetch of page 1), got %d", pageHits.Load())
	}
	if res.Data.Views["timeline"].Records[0]["tag_name"] != "v3.7.1" || res.Data.Views["timeline"].Groups["v3.7"] != 3 {
		t.Fatalf("unexpected timeline view: %+v", res.Data.Views["timeline"])
	}
	if len(res.Data.Views["v374"].ExtractedURLs) != 1 || !strings.Contains(res.Data.Views["v374"].ExtractedURLs[0], "CHANGELOG-3.7.md") {
		t.Fatalf("unexpected v374 URL view: %+v", res.Data.Views["v374"])
	}

	// Warm cached multi-query RunConnector: 0 upstream HTTP calls!
	warmRes, err := svc.RunConnector(context.Background(), RunConnectorInput{
		SourceID:        "etcd_mp",
		FreshnessTTLSec: 3600,
		Queries: map[string]*pipeline.RecordQuery{
			"oldest": {Fields: []string{"tag_name", "published_at"}, OrderBy: "published_at", Limit: 1},
			"latest": {Fields: []string{"tag_name", "published_at"}, OrderBy: "-published_at", Limit: 1},
		},
	}, "http://localhost:8080")
	if err != nil || warmRes.HitType != HitCacheHit || pageHits.Load() != 2 {
		t.Fatalf("expected 0-HTTP multi_query cache hit, got res=%+v hits=%d err=%v", warmRes, pageHits.Load(), err)
	}
	if warmRes.Data.Views["oldest"].Records[0]["tag_name"] != "v3.7.1" || warmRes.Data.Views["latest"].Records[0]["tag_name"] != "v3.7.4" {
		t.Fatalf("unexpected warm multi_query views: %+v", warmRes.Data.Views)
	}

	// Short multi-page run (page 2 returns 0 rows -> total 2 < min_rows 4) MUST still trigger drift after all pages finish.
	shortSpecJSON, _ := json.Marshal(APISpec{
		URLTemplate:  "https://api.example.org/releases?per_page=2&short=1",
		Method:       http.MethodGet,
		ItemsPath:    "",
		FieldMapping: map[string]string{"tag_name": "tag_name", "published_at": "published_at", "body": "body"},
		Pagination:   "page",
		PageParam:    "page",
		MaxPages:     2,
	})
	shortRes, _ := svc.DeployAndRun(context.Background(), DeployAndRunInput{
		SourceID:     "etcd_short",
		Kind:         KindData,
		Executor:     ExecutorHTTPAPI,
		SpecOrCode:   string(shortSpecJSON),
		OutputSchema: outSchema,
		WaitMS:       2000,
	}, "http://localhost:8080")
	if shortRes.Status != "failed" || !strings.Contains(shortRes.Error, "below expected min_rows 4") {
		t.Fatalf("expected full-run min_rows drift on short multi-page connector, got %+v", shortRes)
	}
	svc.Pipeline.WaitSnapshots()
}
