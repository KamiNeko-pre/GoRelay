package information

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/engine"
	"go-lua-crawler/internal/knowledge"
	"go-lua-crawler/internal/programrunner"
)

func TestWarmBrowserDoesNotQueueIndependentOrCanceledCalls(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		if cancelled {
			cancel()
		}
		var runner BrowserRunner
		runner.workerMu.Lock()
		done := make(chan error, 1)
		go func() {
			_, err := runner.runWorkerPayload(ctx, "unused-browser", nil)
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil || cancelled != errors.Is(err, context.Canceled) {
				t.Errorf("cancelled=%t, got %v", cancelled, err)
			}
		case <-time.After(time.Second):
			t.Errorf("cancelled=%t waited behind an unrelated Browser call", cancelled)
		}
		cancel()
		runner.workerMu.Unlock()
	}
}

func TestLuaRecordsKeepShortStaticDataEvenWithScriptHints(t *testing.T) {
	for _, markup := range []string{
		`<body><div id="app"><a href="/role/1">Go岗位</a></div><script src="/app.js"></script></body>`,
		`<body><ul><li>Published</li></ul><script>document.write('<script src="app.js"><\/script>')</script></body>`,
		`<body>Verify you are human</body>`,
	} {
		resp := acquisition.Response{Body: markup, Receipt: acquisition.Receipt{
			URL: "https://example.org/index", Status: http.StatusOK, FetchedAt: time.Now(),
		}}
		captures := []acquisition.Response{resp}
		rows, _ := luaRecordsFromItems([]map[string]any{{"title": "Go"}}, engine.ProgramResult{SourceEvidence: captures}, resp.Receipt.URL, 0)
		want := !strings.Contains(markup, "Verify you are human")
		if (len(rows) > 0) != want {
			t.Errorf("want usable=%t for %s, got %+v", want, markup, rows)
		}
		responses, _, err := acquisitionEnvelope(1, rows, captures)
		if err != nil || len(responses) != 1 || responses[0].Blocked == want {
			t.Errorf("want usable snapshot=%t for %s, got %+v, %v", want, markup, responses, err)
		}
	}
}

func TestRateLimitReceiptGivesRetryAdviceWithoutBrowserFallback(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusNotFound} {
		response := acquisition.Response{Receipt: acquisition.Receipt{
			Status: status, Headers: map[string]string{"retry-after": "30"},
		}}
		entry := map[string]any{}
		addRateLimitAdvice(entry, response.Receipt)
		if _, ok := browserFallbackReason(response); ok {
			t.Fatalf("HTTP %d should not enable Browser on status alone", status)
		}
		if status == http.StatusTooManyRequests {
			if entry["retry_after"] != "30" || !strings.Contains(entry["next_step"].(string), "reduce request frequency") {
				t.Fatalf("missing actionable rate-limit fields: %+v", entry)
			}
		} else if len(entry) != 0 {
			t.Fatalf("unexpected rate-limit advice for HTTP %d: %+v", status, entry)
		}
	}
}

func TestSourceInspectionReusesPerURLReceiptsAcrossBatches(t *testing.T) {
	s, _ := retrievalService(t)
	ctx := context.Background()
	req, err := s.Create(ctx, Input{RequestKey: "inspection-alias", Goal: "Inspect", SourceScope: []string{"example.org"}})
	if err != nil {
		t.Fatal(err)
	}
	host := &informationHost{service: s, request: req, input: Input{SourceScope: []string{"example.org"}}}
	for _, u := range []string{"https://example.org/one", "https://example.org/two"} {
		step, _, err := s.reserveStep(ctx, req, "inspect_api_candidate", map[string]any{"url": u, "scope": host.input.SourceScope})
		if err != nil {
			t.Fatal(err)
		}
		if err = s.finishStep(ctx, step, map[string]any{"url": u, "status": 200, "references": []StoredReference{{DocID: u, Revision: "rev"}}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	s.NewFetcher = func([]string) (*acquisition.PublicFetcher, error) {
		return nil, errors.New("cached URL unexpectedly scheduled for a second fetch")
	}
	for _, tc := range []struct {
		urls []string
		want int
	}{
		{[]string{"example.org/one"}, 1},
		{[]string{"https://example.org/two", "https://example.org/one", "https://EXAMPLE.org/one#anchor"}, 2},
	} {
		batch, err := host.observeBatch(ctx, tc.urls, false)
		if err != nil {
			t.Errorf("URLs %v: %v", tc.urls, err)
			continue
		}
		if sources, ok := batch["sources"].([]map[string]any); !ok || len(sources) != tc.want {
			t.Errorf("unexpected batch result for %v: %+v", tc.urls, batch)
		}
	}
}

func TestPrefixCacheStableMessagesKeepSingleSystemAndByteStablePrefix(t *testing.T) {
	baseHistory := []*schema.Message{
		schema.SystemMessage("stale old policy"),
		schema.UserMessage("User request goal"),
	}
	facts1 := map[string]any{"current_time": "2026-11-19T00:00:01Z", "facts_revision": 1}
	turn1 := append(informationStableHistoryMessages(baseHistory), buildRuntimeContextMessage(facts1, "[STATUS: PLAN]", ""))

	// Simulate Eino appending assistant(1) + tool(1) onto turn1.
	turn1AfterTool := append(append([]*schema.Message{}, turn1...),
		&schema.Message{Role: schema.Assistant, Content: "inspecting", ToolCalls: []schema.ToolCall{{ID: "c1"}}},
		&schema.Message{Role: schema.Tool, ToolCallID: "c1", Content: `{"ok":true}`},
	)
	facts2 := map[string]any{"current_time": "2026-11-19T00:00:15Z", "facts_revision": 2}
	turn2 := append(informationStableHistoryMessages(turn1AfterTool), buildRuntimeContextMessage(facts2, "[STATUS: ACQUIRE]", ""))

	// Simulate Eino appending assistant(2) + tool(2) onto turn2.
	turn2AfterTool := append(append([]*schema.Message{}, turn2...),
		&schema.Message{Role: schema.Assistant, Content: "extracting", ToolCalls: []schema.ToolCall{{ID: "c2"}}},
		&schema.Message{Role: schema.Tool, ToolCallID: "c2", Content: `{"rows":5}`},
	)
	facts3 := map[string]any{"current_time": "2026-11-19T00:00:30Z", "facts_revision": 3}
	turn3 := append(informationStableHistoryMessages(turn2AfterTool), buildRuntimeContextMessage(facts3, "[STATUS: READY]", ""))

	for label, msgs := range map[string][]*schema.Message{"turn1": turn1, "turn2": turn2, "turn3": turn3} {
		sysCount := 0
		for i, m := range msgs {
			if m.Role == schema.System {
				sysCount++
				if i != 0 {
					t.Fatalf("%s: unexpected system message at index %d: %q", label, i, m.Content)
				}
			}
		}
		if sysCount != 1 {
			t.Fatalf("%s: want exactly 1 system message, got %d", label, sysCount)
		}
		last := msgs[len(msgs)-1]
		if last.Role != schema.User || !strings.HasPrefix(last.Content, informationFactsPrefix) {
			t.Fatalf("%s: expected trailing user runtime snapshot, got role=%s", label, last.Role)
		}
	}

	// Prefix stability: every message in turn2 before its volatile tail MUST be byte-identical to turn3.
	stableLen := len(turn2) - 1
	for i := 0; i < stableLen; i++ {
		b2, _ := json.Marshal(turn2[i])
		b3, _ := json.Marshal(turn3[i])
		if string(b2) != string(b3) {
			t.Fatalf("prefix mismatch at message %d:\nturn2=%s\nturn3=%s", i, string(b2), string(b3))
		}
	}
}

func TestRunLuaProbeModeZeroPersistenceAndSnapshotReuse(t *testing.T) {
	s, _ := retrievalService(t)
	db := s.DB
	ctx := context.Background()
	var httpFetches atomic.Int32
	s.LuaRunner = func(_ context.Context, req programrunner.Request) (engine.ProgramResult, error) {
		return engine.RunProgramWithRequester(ctx, req.Program, req.Limits, func(_ context.Context, opts acquisition.RequestOptions) (acquisition.Response, error) {
			httpFetches.Add(1)
			return acquisition.Response{
				Body: `{"total":34,"cities":{"beijing":"CT_11"},"jobs":[{"id":"J1","title":"Go Engineer"}]}`,
				Receipt: acquisition.Receipt{
					Method:       opts.Method,
					RequestedURL: opts.URL,
					URL:          opts.URL,
					Status:       http.StatusOK,
					FetchedAt:    time.Now().UTC(),
					Headers:      map[string]string{"content-type": "application/json"},
					Cookies:      map[string]string{"csrf": "tok1"},
					SnapshotKey:  "test-api-jobs",
				},
			}, nil
		})
	}
	req, err := s.Create(ctx, Input{RequestKey: "probe-reuse-test", Goal: "Probe then extract", SourceScope: []string{"example.org"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&Request{}).Where("id = ?", req.ID).Updates(map[string]any{
		"status":      "running",
		"lease_token": "lease-probe",
		"lease_until": time.Now().Add(5 * time.Minute),
	}).Error; err != nil {
		t.Fatal(err)
	}
	req, _ = s.Request(ctx, req.ID)
	host := &informationHost{
		service: s,
		owner:   requestExecutionOwner{RequestID: req.ID, Token: "lease-probe"},
		request: req,
		input:   Input{Goal: "Probe then extract", SourceScope: []string{"example.org"}},
	}

	// 1. Probe mode: inspects structure without persisting any observations, while saving reusable snapshot reference.
	probeArgs := mustMarshal(map[string]any{
		"mode": "probe",
		"url":  "https://example.org/api/jobs",
		"code": `local r = http_request({url=TARGET_URL}); local d = json_decode(r.body); return true, {total=d.total, cities=d.cities, count=#d.jobs}`,
	})
	rawProbe, err := host.invoke(ctx, "probe-1", "run_lua", string(probeArgs))
	if err != nil {
		t.Fatalf("probe invoke err: %v", err)
	}
	var probeReceipt informationReceipt
	if err := json.Unmarshal([]byte(rawProbe), &probeReceipt); err != nil {
		t.Fatal(err)
	}
	if probeReceipt.Status != "succeeded" {
		t.Fatalf("expected probe succeeded, got: %s", rawProbe)
	}
	var obsCount int64
	if err := db.Model(&taskObservation{}).Where("request_id = ?", req.ID).Count(&obsCount).Error; err != nil {
		t.Fatal(err)
	}
	if obsCount != 0 {
		t.Fatalf("expected 0 observations persisted by probe mode, got %d", obsCount)
	}
	dataMap, _ := probeReceipt.Data.(map[string]any)
	refs, _ := dataMap["references"].([]any)
	if len(refs) != 1 {
		t.Fatalf("expected 1 reusable snapshot reference from probe, got %+v", dataMap)
	}
	ref0, _ := refs[0].(map[string]any)
	snapshots, err := host.acquisitionSnapshots(ctx)
	if err != nil || !strings.Contains(snapshots.responses["test-api-jobs"].Body, `"total":34`) {
		t.Fatalf("JSON without a separate raw body must remain reusable: %v", err)
	}
	page, err := s.QueryTaskDocumentExcerpts(ctx, req.ID, host.input.SourceScope, MaterialReadQuery{Query: "total"})
	if err != nil {
		t.Fatal(err)
	}
	matches := page["matches"].([]excerptMatchEntry)
	if len(matches) != 1 || matches[0].EvidenceID == "" {
		t.Fatalf("expected an evidence handle: %+v", matches)
	}
	excerpt, _, err := resolveDeliveryEvidence(ctx, db, req.ID, matches[0].EvidenceID, knowledge.RetrievalScope{URLHosts: host.input.SourceScope}, map[[2]string]knowledge.Document{})
	if err != nil || !containsDeliveryNumber(excerpt, 34) {
		t.Fatalf("cannot recover exact evidence: %q %v", excerpt, err)
	}
	if httpFetches.Load() != 1 {
		t.Fatalf("expected 1 HTTP fetch during probe, got %d", httpFetches.Load())
	}

	// 2. Sample extraction using source_ref from probe must persist records with ZERO additional HTTP fetches.
	extractArgs := mustMarshal(map[string]any{
		"mode":       "sample",
		"source_ref": map[string]any{"doc_id": ref0["doc_id"], "revision": ref0["revision"]},
		"code":       `local d = json_decode(SOURCE_RESPONSE.body); return true, {items={{id=d.jobs[1].id, title=d.jobs[1].title}}}`,
	})
	rawExtract, err := host.invoke(ctx, "extract-1", "run_lua", string(extractArgs))
	if err != nil {
		t.Fatalf("extract invoke err: %v", err)
	}
	var extractReceipt informationReceipt
	if err := json.Unmarshal([]byte(rawExtract), &extractReceipt); err != nil {
		t.Fatal(err)
	}
	if extractReceipt.Status != "succeeded" {
		t.Fatalf("expected extract succeeded, got: %s", rawExtract)
	}
	if httpFetches.Load() != 1 {
		t.Fatalf("expected source_ref extract to perform 0 extra HTTP fetches, got total %d", httpFetches.Load())
	}
	if err := db.Model(&taskObservation{}).Where("request_id = ?", req.ID).Count(&obsCount).Error; err != nil {
		t.Fatal(err)
	}
	if obsCount != 1 {
		t.Fatalf("expected 1 observation persisted after sample extract, got %d", obsCount)
	}
	draft := DeliveryDraft{AcquisitionReceiptIDs: []uint64{probeReceipt.ReceiptID, extractReceipt.ReceiptID}}
	if _, err := bindDeliveryBatches(db, req.ID, &draft); err != nil || len(draft.Tables) != 1 || len(draft.Tables[0].Rows) != 1 {
		t.Fatalf("a probe must not block a valid extraction batch: %v %+v", err, draft.Tables)
	}
}

func TestEnsureAcquisitionFromFieldNames(t *testing.T) {
	acq := ensureAcquisitionFromFieldNames(nil, "Extract jobs", []string{"title", "link", "source_url", "title"})
	if acq.Goal != "Extract jobs" || len(acq.Fields) != 2 {
		t.Fatalf("unexpected auto-derived acquisition: %+v", acq)
	}
	if _, err := withLocalAcquisition(context.Background(), acq); err != nil {
		t.Fatalf("expected auto-derived acquisition to validate cleanly, got %v", err)
	}
}

func TestExtractAppScriptSrcsIncludesDynamicLoaders(t *testing.T) {
	html := `<html><head>
<script src="/static/analytics-gtm.js"></script>
<script src="/static/main.js"></script>
<script>
var _b = './';
document.write('<script src="' + _b + 'assets/js/api.js?v=' + Date.now() + '"><\/script>');
document.write('<script src="' + _b + 'assets/js/jobs.js?v=2"><\/script>');
</script>
</head><body></body></html>`
	meta := acquisitionResponseMetadata(acquisition.Response{
		Body:    html,
		Receipt: acquisition.Receipt{URL: "https://hr.example.com/opportunities.html", Status: 200, Headers: map[string]string{"content-type": "text/html"}},
	})
	outline, _ := meta["html_outline"].(map[string]any)
	srcs, _ := outline["script_srcs"].([]string)
	joined := strings.Join(srcs, " ")
	for _, want := range []string{"https://hr.example.com/static/main.js", "https://hr.example.com/assets/js/api.js", "https://hr.example.com/assets/js/jobs.js"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("expected %s in script_srcs, got %+v", want, srcs)
		}
	}
}

func TestAcquisitionResponseMetadataXMLAtomOutline(t *testing.T) {
	atom := `<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <title>Release notes</title>
  <entry>
    <id>tag:github.com,2008:Repository/1/v1.31.0</id>
    <updated>2026-10-01T12:00:00Z</updated>
    <link rel="alternate" type="text/html" href="https://github.com/org/repo/releases/tag/v1.31.0"/>
    <title>v1.31.0</title>
  </entry>
  <entry>
    <id>tag:github.com,2008:Repository/1/v1.30.0</id>
    <updated>2026-09-01T12:00:00Z</updated>
    <link rel="alternate" type="text/html" href="https://github.com/org/repo/releases/tag/v1.30.0"/>
    <title>v1.30.0</title>
  </entry>
</feed>`
	meta := acquisitionResponseMetadata(acquisition.Response{
		Body:    atom,
		Receipt: acquisition.Receipt{URL: "https://github.com/org/repo/releases.atom", Status: 200, Headers: map[string]string{"content-type": "application/atom+xml"}},
	})
	if meta["is_xml"] != true {
		t.Fatalf("expected is_xml=true, got %+v", meta)
	}
	outline, _ := meta["xml_outline"].(map[string]any)
	if outline["root_tag"] != "feed" {
		t.Fatalf("unexpected xml_outline: %+v", outline)
	}
}

func TestCompactBrowserNetworkAPIsPrioritizesStatefulAndCollectionEndpoints(t *testing.T) {
	// Build a large config dictionary response schema (~4.5KB) that previously starved later endpoints.
	bigConfig := map[string]any{}
	for i := 0; i < 40; i++ {
		bigConfig["key_"+string(rune('a'+i%26))+string(rune('0'+i/10))] = map[string]any{
			"id": i, "label": "option", "nested": map[string]any{"x": 1, "y": 2, "z": 3},
		}
	}
	bigSchema := json.RawMessage(mustMarshal(observedJSONPaths(bigConfig, "")))
	searchSchema := json.RawMessage(mustMarshal(observedJSONPaths(map[string]any{
		"code": 0,
		"data": map[string]any{
			"count": 1046,
			"job_post_list": []any{
				map[string]any{"id": "123", "title": "Go Backend", "city_info": map[string]any{"name": "Beijing"}},
			},
		},
	}, "")))
	scalarSchema := json.RawMessage(mustMarshal(observedJSONPaths(map[string]any{"ok": true, "ip": "1.2.3.4"}, "")))

	apis := []acquisition.NetworkAPICapture{
		{Method: "GET", URL: "https://jobs.example.com/api/v1/login_status", Status: 200, ResponseSchema: scalarSchema},
		{Method: "GET", URL: "https://jobs.example.com/api/v1/ip/location", Status: 200, ResponseSchema: scalarSchema},
		{Method: "GET", URL: "https://jobs.example.com/api/v1/config/filters/2", Status: 200, ResponseSchema: bigSchema},
		{Method: "POST", URL: "https://jobs.example.com/api/v1/csrf/token", Status: 200, SetsCookie: true, ResponseSchema: scalarSchema},
		{Method: "POST", URL: "https://jobs.example.com/api/v1/search/job/posts", Status: 200, RequestBody: `{"keyword":"Go","limit":10}`, SentCookies: true, ResponseSchema: searchSchema},
	}
	compacted := compactBrowserNetworkAPIs(apis, "https://jobs.example.com/experienced", 6000)
	seen := map[string]bool{}
	for _, a := range compacted {
		seen[a.Method+" "+a.URL] = true
	}
	if !seen["POST https://jobs.example.com/api/v1/csrf/token"] {
		t.Fatalf("expected stateful csrf/token endpoint preserved, got %+v", compacted)
	}
	if !seen["POST https://jobs.example.com/api/v1/search/job/posts"] {
		t.Fatalf("expected parameterized search endpoint preserved, got %+v", compacted)
	}
	if raw, _ := json.Marshal(compacted); len(raw) > 6000 {
		t.Fatalf("expected compacted network_apis <= 6000 bytes, got %d", len(raw))
	}
}

func TestExtractBrowserCollectionSupportsSelfSelector(t *testing.T) {
	html := `<html><body><div class="list">
<a class="card" href="/job/101"><span class="title">Go Engineer</span></a>
<a class="card" href="/job/102"><span class="title">Backend Lead</span></a>
</div></body></html>`
	runtime := engine.ProgramResult{
		SourceEvidence: []acquisition.Response{{
			Body:    html,
			Receipt: acquisition.Receipt{URL: "https://jobs.example.com/list", Status: 200, FetchedAt: time.Now().UTC()},
		}},
	}
	plan := BrowserPlan{
		URL:          "https://jobs.example.com/list",
		RowsSelector: "div.list > a.card",
		Fields: map[string]BrowserField{
			"title":   {Selector: "span.title"},
			"href":    {Selector: ".", Attribute: "href"},
			"altHref": {Selector: "a.card", Attribute: "href"},
		},
	}
	if err := extractBrowserCollection(plan, &runtime); err != nil {
		t.Fatalf("expected self selector '.' and row.Is fallback to succeed, got %v", err)
	}
	items, _ := runtime.Collection["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("expected 2 extracted rows, got %d", len(items))
	}
	row0, _ := items[0].(map[string]any)
	if row0["href"] != "/job/101" || row0["altHref"] != "/job/101" || row0["title"] != "Go Engineer" {
		t.Fatalf("unexpected row0: %+v", row0)
	}
}

func TestBrowserFallbackReasonDetectsHeadTitleAndThinDynamicShells(t *testing.T) {
	// 1. SPA with <head><title> and empty #app mount (previously misclassified because doc.Text() included <title>).
	spaWithTitle := acquisition.Response{
		Body:    `<!doctype html><html><head><title>Careers Portal</title></head><body><div id="app"></div><script src="/app.js"></script></body></html>`,
		Receipt: acquisition.Receipt{URL: "https://jobs.example.com/", Status: 200},
	}
	if _, ok := browserFallbackReason(spaWithTitle); !ok {
		t.Fatal("expected SPA shell with <title> and empty #app to trigger browser fallback")
	}

	// 2. Thin shell with document.write dynamic JS loader and minimal heading text.
	thinLoader := acquisition.Response{
		Body: `<!doctype html><html><head><title>Jobs</title></head><body>
<header><nav>Home About Careers</nav></header>
<div class="banner"><h1>Open Positions</h1><p>Loading positions...</p></div>
<script>document.write('<script src="assets/js/jobs.js"><\/script>');</script>
</body></html>`,
		Receipt: acquisition.Receipt{URL: "https://hr.example.com/opportunities.html", Status: 200},
	}
	if _, ok := browserFallbackReason(thinLoader); !ok {
		t.Fatal("expected thin document.write JS loader shell to trigger browser fallback")
	}

	// 3. Real static content page with scripts MUST NOT trigger browser fallback.
	staticPage := acquisition.Response{
		Body: `<!doctype html><html><head><title>Releases</title><script src="/site.js"></script></head><body>
<main><h1>PostgreSQL Releases</h1>
<table><tr><th>Version</th><th>Date</th></tr><tr><td>18.7</td><td>2026-11-12</td></tr><tr><td>17.9</td><td>2026-11-12</td></tr></table>
<p>PostgreSQL is an advanced open source relational database with full documentation and release notes available directly in static HTML.</p>
</main></body></html>`,
		Receipt: acquisition.Receipt{URL: "https://www.postgresql.org/docs/release/", Status: 200},
	}
	if reason, ok := browserFallbackReason(staticPage); ok {
		t.Fatalf("expected real static HTML page NOT to trigger browser fallback, got reason=%q", reason)
	}
}

func TestReadMaterialsExcerptsAcrossBodyAndRawBody(t *testing.T) {
	s, _ := retrievalService(t)
	db := s.DB
	ctx := context.Background()
	req, err := s.Create(ctx, Input{RequestKey: "excerpts-test", Goal: "Compare pricing quotes", SourceScope: []string{"example.org"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&Request{}).Where("id = ?", req.ID).Updates(map[string]any{
		"status":      "running",
		"lease_token": "lease-exc",
		"lease_until": time.Now().Add(5 * time.Minute),
	}).Error; err != nil {
		t.Fatal(err)
	}
	req, _ = s.Request(ctx, req.ID)
	step := Step{RequestID: req.ID, Key: "seed-excerpts", Kind: "acquire_sources", Stage: "succeeded"}
	if err := db.Create(&step).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	_, err = s.ingestAcquiredRecords(ctx, req.ID, step.ID, nil, []acquisition.Response{
		{
			Body:    `<html><body><p>Cloudflare R2 Free tier includes 10 GB-month per month.</p><span>$0.015 / GB-month</span></body></html>`,
			Receipt: acquisition.Receipt{Method: "GET", URL: "https://example.org/r2/pricing", Status: 200, FetchedAt: now},
		},
		{
			Body:    `{"terms":{"GET":"0.0000004000","PUT":"0.0000050000"}}`,
			Receipt: acquisition.Receipt{Method: "GET", URL: "https://example.org/s3.json", Status: 200, FetchedAt: now},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	host := &informationHost{
		service: s,
		owner:   requestExecutionOwner{RequestID: req.ID, Token: "lease-exc"},
		request: req,
		input:   Input{Goal: "Compare pricing quotes", SourceScope: []string{"example.org"}},
	}
	raw, err := host.invoke(ctx, "read-exc-1", "read_materials", string(mustMarshal(MaterialReadQuery{
		Kind:    "excerpts",
		Queries: []string{"Free tier", "<span>$0.015 / GB-month</span>", "0.0000004000", "nonexistent_token"},
	})))
	if err != nil {
		t.Fatalf("read_materials excerpts failed: %v", err)
	}
	var rec informationReceipt
	if err := json.Unmarshal([]byte(raw), &rec); err != nil || rec.Status != "succeeded" {
		t.Fatalf("unexpected receipt: %s err=%v", raw, err)
	}
	data, _ := rec.Data.(map[string]any)
	matches, _ := data["matches"].([]any)
	if len(matches) < 2 {
		t.Fatalf("expected excerpts from both Body and RawBody snapshots, got %+v", data)
	}
	missing, _ := data["missing_queries"].([]any)
	if len(missing) != 1 || missing[0] != "nonexistent_token" {
		t.Fatalf("expected missing_queries=['nonexistent_token'], got %+v", missing)
	}
}

func TestInspectionBindsExcerptsToItsOwnSuccessfulCaptures(t *testing.T) {
	var requests atomic.Int32
	publicSource(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path == "/offline" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><title>Rail service</title><p>Weekend travel starts at 12 dollars.</p></html>`))
	}))
	s, _ := retrievalService(t)
	db := s.DB
	ctx := context.Background()
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	input := Input{Goal: "Find weekend rail fares", SourceScope: []string{"8.8.8.8"}}
	r, err := s.Create(ctx, input)
	check(err)
	check(db.Model(&r).Updates(map[string]any{"status": "running", "lease_token": "excerpt-inspection", "lease_until": time.Now().Add(time.Minute)}).Error)
	r, err = s.Request(ctx, r.ID)
	check(err)
	h := &informationHost{service: s, request: r, owner: requestExecutionOwner{RequestID: r.ID, Token: r.LeaseToken}, input: input}
	invoke := func(id, tool string, args any) informationReceipt {
		t.Helper()
		result, err := h.invoke(ctx, id, tool, string(mustMarshal(args)))
		check(err)
		var receipt informationReceipt
		check(json.Unmarshal([]byte(result), &receipt))
		if receipt.Status != "succeeded" {
			t.Fatalf("%s failed: %s", tool, result)
		}
		return receipt
	}
	for _, kind := range []string{"assets", "documents"} {
		invoke("empty-"+kind, "read_materials", MaterialReadQuery{Kind: kind})
	}
	other := Step{RequestID: r.ID, Key: "unrelated-capture", Kind: "acquire_sources", Stage: "succeeded"}
	check(db.Create(&other).Error)
	_, err = s.ingestAcquiredRecords(ctx, r.ID, other.ID, nil, []acquisition.Response{{
		Body:    "Archived weekday fares from a different timetable were 99 dollars.",
		Receipt: acquisition.Receipt{Method: "GET", URL: "http://8.8.8.8/old", Status: 200, FetchedAt: time.Now().UTC(), SnapshotKey: "unrelated-cache-key"},
	}})
	check(err)
	var unrelated taskDocument
	check(db.Where("request_id = ? AND receipt_id = ?", r.ID, other.ID).Take(&unrelated).Error)
	check(knowledge.NewStore(db).Invalidate(ctx, unrelated.DocID))
	args := informationAcquireArgs{InspectURLs: []string{"http://8.8.8.8/weekend", "http://8.8.8.8/offline"}, ExcerptQueries: []string{"weekend travel", "weekday fares"}}
	receipt := invoke("inspect-with-evidence", "acquire_sources", args)
	var out struct {
		Status   string `json:"status"`
		Evidence struct {
			Matches []excerptMatchEntry `json:"matches"`
			Missing []string            `json:"missing_queries"`
		} `json:"evidence"`
	}
	check(json.Unmarshal(mustMarshal(receipt.Data), &out))
	if out.Status != "partial" || len(out.Evidence.Matches) != 1 || !slices.Equal(out.Evidence.Missing, []string{"weekday fares"}) || requests.Load() != 2 {
		t.Fatalf("expected two independent requests, one usable excerpt and a real gap: %+v", out)
	}
	match := out.Evidence.Matches[0]
	excerpt, _, err := resolveDeliveryEvidence(ctx, db, r.ID, match.EvidenceID, knowledge.RetrievalScope{URLHosts: input.SourceScope}, map[[2]string]knowledge.Document{})
	check(err)
	if match.URL != "http://8.8.8.8/weekend" || !containsDeliveryNumber(excerpt, 12) {
		t.Fatalf("inline evidence did not resolve to the original fare statement: %s", excerpt)
	}
	invoke("inspect-with-evidence", "acquire_sources", args)
	if requests.Load() != 2 {
		t.Fatal("reusing the same outer call issued fresh requests")
	}
	check(db.Model(&Step{}).Where("request_id = ? AND id = ?", r.ID, receipt.ReceiptID).Updates(map[string]any{"stage": "reserved", "result_json": ""}).Error)
	recovered := invoke("inspect-with-evidence", "acquire_sources", args)
	if requests.Load() != 2 || !strings.Contains(string(mustMarshal(recovered.Data)), match.EvidenceID) {
		t.Fatal("recovering the admitted inspection did not reuse its completed captures")
	}
	captured := invoke("capture-by-known-url", "acquire_sources", informationAcquireArgs{URLs: []string{match.URL}})
	if requests.Load() != 2 || !strings.Contains(string(mustMarshal(captured.Data)), "reused_snapshot") {
		t.Fatal("known GET did not reuse the exact captured response")
	}
	args = informationAcquireArgs{URLs: []string{match.URL}, ExcerptQueries: []string{"weekend travel"}}
	result, err := h.invoke(ctx, "same-capture-other-query", "acquire_sources", string(mustMarshal(args)))
	check(err)
	var duplicate informationReceipt
	check(json.Unmarshal([]byte(result), &duplicate))
	if duplicate.Code != "ACTION_ALREADY_CALLED" || duplicate.PriorStepID != captured.ReceiptID || requests.Load() != 2 {
		t.Fatalf("changing only presentation must keep the original capture: %s", result)
	}
}

func TestVerifyDeliveryCalculationsBatchedErrorsAndRawBodyQuotes(t *testing.T) {
	ctx := context.Background()
	s, _ := retrievalService(t)
	db := s.DB
	req, err := s.Create(ctx, Input{RequestKey: "calc-raw-batch", Goal: "Calc check for 5 units", SourceScope: []string{"example.org"}})
	if err != nil {
		t.Fatal(err)
	}
	step := Step{RequestID: req.ID, Key: "cap-calc", Kind: "acquire_sources", Stage: "succeeded"}
	if err := db.Create(&step).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	_, err = s.ingestAcquiredRecords(ctx, req.ID, step.ID, nil, []acquisition.Response{
		{
			Body:    `<html><body><p>Rate is <span>6.95</span>/TB/30-days</p></body></html>`,
			Receipt: acquisition.Receipt{Method: "GET", URL: "https://example.org/b2", Status: 200, FetchedAt: now},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var docRow taskDocument
	if err := db.First(&docRow, "request_id = ?", req.ID).Error; err != nil {
		t.Fatal(err)
	}
	ref := StoredReference{DocID: docRow.DocID, Revision: docRow.Revision}

	// 1. RawBody HTML fragment quote must validate cleanly via SubmitDelivery.
	if err := db.Model(&Request{}).Where("id = ?", req.ID).Update("status", "running").Error; err != nil {
		t.Fatal(err)
	}
	_, err = s.SubmitDelivery(ctx, req.ID, step.ID, DeliveryDraft{
		Outcome: "completed",
		Report:  "B2 cost is {{b2_storage}}",
		Calculations: []DeliveryCalculation{{
			ID:           "b2_storage",
			Formula:      "qty * rate",
			PromptInputs: map[string]PromptCalculationInput{"qty": {Value: 5, Label: "units"}},
			Inputs: map[string]CalculationInput{
				"rate": {Value: 6.95, Quote: "<span>6.95</span>/TB/30-days", Reference: ref},
			},
		}},
	})
	if err != nil {
		t.Fatalf("expected RawBody quote to pass SubmitDelivery validation, got %v", err)
	}

	// 2. Multiple calculation input errors across independent calculations must ALL be returned in one error.
	bad := DeliveryDraft{
		Report: "{{a}} {{b}} {{sum}}",
		Calculations: []DeliveryCalculation{
			{ID: "a", Formula: "2 * x", Inputs: map[string]CalculationInput{
				"x": {Value: 99, Quote: "Rate is 6.95/TB/30-days", Reference: ref},
			}},
			{ID: "b", Formula: "3 * y", Inputs: map[string]CalculationInput{
				"y": {Value: 1, Quote: "missing quote sentence", Reference: ref},
			}},
			{ID: "sum", Formula: "a + b"},
		},
	}
	err = verifyDeliveryCalculationsWithSources(&bad, func(StoredReference) ([]string, error) {
		return []string{"Rate is 6.95/TB/30-days", "<span>6.95</span>/TB/30-days"}, nil
	})
	if err == nil {
		t.Fatal("expected batched error")
	}
	msg := err.Error()
	if !strings.Contains(msg, `"a"`) || !strings.Contains(msg, `"b"`) || strings.Contains(msg, `"sum"`) {
		t.Fatalf("expected BOTH independent failures reported together without cascading upstream noise on 'sum', got: %s", msg)
	}
}

func TestResolveLuaTargetsProbeAllowsLocalMath(t *testing.T) {
	if _, err := resolveLuaTargetsWithOptions(luaRunArgs{Code: `print(5000*0.015)`}, nil, []string{"example.org"}, true); err != nil {
		t.Fatalf("expected probe mode to allow URL-less local Lua checks, got %v", err)
	}
	if _, err := resolveLuaTargetsWithOptions(luaRunArgs{Code: `return true, {items={{v=1}}}`}, nil, []string{"example.org"}, false); err == nil {
		t.Fatal("expected non-probe mode to still require source targets")
	}
}
