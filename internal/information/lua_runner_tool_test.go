package information

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/engine"
	"go-lua-crawler/internal/knowledge"
	"go-lua-crawler/internal/programrunner"
)

func TestLuaReusesCapturedResponse(t *testing.T) {
	host := &informationHost{input: Input{EntryURLs: []string{"https://example.org/another-page"}}}
	raw := `{"code":"return true, {items={}}","source_ref":{"doc_id":"doc","revision":"rev"}}`
	bound, err := host.bindCurrentRead(context.Background(), "run_lua", raw)
	if err != nil || bound != raw {
		t.Fatalf("host replaced the snapshot with default network targets: %s, %v", bound, err)
	}
	captured := acquisition.Response{
		Receipt: acquisition.Receipt{URL: "https://example.org/list", Status: 200, FetchedAt: time.Unix(1, 0)},
		Body:    `<li><a href="/candidate">Candidate</a></li>`,
	}
	doc, err := knowledge.NewDocument(knowledge.Input{
		Source: "information-response", SourceKey: "fixture", URL: captured.Receipt.URL, Title: "List",
		Body: "Candidate", RawBody: captured.Body, Format: "text", FetchedAt: captured.Receipt.FetchedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := engine.ProgramRequest{TargetURL: doc.URL, SavedResponse: &captured}
	for _, field := range []struct{ code, name, want string }{
		{`html_find(r.body, "li")`, "title", "Candidate"},
		{`html_attr_all(r.body, "li a", "href")[1]`, "link", "/candidate"},
	} {
		req.Code = fmt.Sprintf("local r = SOURCE_RESPONSE; return true, {items={{%s=%s, source_response=r.source_response}}}", field.name, field.code)
		result, err := engine.RunProgramWithRequester(context.Background(), req, engine.DefaultProgramLimits(), nil)
		if err != nil {
			t.Fatal(err)
		}
		items, _ := normalizeLuaItems(result.Collection["items"].([]any))
		rows, _ := luaRecordsFromItems(items, result, doc.URL, 0)
		if len(rows) != 1 || rows[0].Fields[field.name] != field.want || len(result.Requests) != 0 ||
			!rows[0].Provenance.FetchedAt.Equal(doc.FetchedAt) {
			t.Fatalf("snapshot did not supply the field and its original provenance: %+v", rows)
		}
		captures, records, err := acquisitionEnvelope(2, rows, result.SourceEvidence, map[int]knowledge.Document{0: doc})
		wantRef := StoredReference{DocID: doc.ID, Revision: doc.Revision}
		if err != nil || len(captures) != 1 || !captures[0].reused || len(records) != 1 ||
			len(records[0].References) != 1 || records[0].References[0] != wantRef {
			t.Fatalf("snapshot provenance did not reach the persisted record contract: %+v, %v", records, err)
		}
	}
}

func TestLuaReceiptBindsRepeatedURLToExactResponse(t *testing.T) {
	const target = "https://example.org/api"
	code := `local first = http_request(TARGET_URL)
local second = http_request(TARGET_URL)
local function rate(response)
  return tonumber(json_decode(response.body).prices["standard/unit"])
end
local paths = {value="/prices/standard~1unit", wrong="/prices/standard~1unit"}
return true, {items={
  {value=rate(first), wrong=rate(second), source_response=first.source_response, source_json_pointers=paths},
  {value=rate(second), wrong=rate(first), source_response=second.source_response, source_json_pointers=paths},
}}`
	calls := 0
	result, err := engine.RunProgramWithRequester(context.Background(), engine.ProgramRequest{Code: code, TargetURL: target}, engine.DefaultProgramLimits(), func(_ context.Context, opts acquisition.RequestOptions) (acquisition.Response, error) {
		calls++
		return acquisition.Response{Receipt: acquisition.Receipt{URL: opts.URL, Status: 200, FetchedAt: time.Unix(1, 0)}, Body: fmt.Sprintf(`{"prices":{"standard/unit":"0.00%d"}}`, calls)}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	items, omitted := normalizeLuaItems(result.Collection["items"].([]any))
	if len(omitted) != 0 {
		t.Fatalf("fields were omitted: %v", omitted)
	}
	rows, _ := luaRecordsFromItems(items, result, target, 0)
	captures, records, err := acquisitionEnvelope(1, rows, result.SourceEvidence)
	if err != nil || len(records) != 2 || records[0].ResponseKey != "response:0" || records[1].ResponseKey != "response:1" {
		t.Fatalf("same URL must retain exact response identity, got %+v: %v", records, err)
	}
	refs := map[string]StoredReference{}
	for _, capture := range captures {
		refs[capture.Key] = StoredReference{DocID: capture.Document.ID, Revision: capture.Document.Revision}
	}
	proof := sampleEvidenceFor(rows, refs, len(rows))
	if len(proof) != 2 || proof[0].Fields["value"].SourceRef == proof[1].Fields["value"].SourceRef {
		t.Fatalf("same URL pages must have distinct proof references: %+v", proof)
	}
	for i, sample := range proof {
		capture := captures[i]
		value, ok := sample.Fields["value"]
		if !ok || sample.RowIndex != i || len(sample.Fields) != 1 || len(records[i].References) != 2 ||
			value.JSONPointer != "/prices/standard~1unit" || value.SourceRef.DocID != capture.Document.ID || value.SourceRef.Revision != capture.Document.Revision || rows[i].Fields["source_json_pointers"] != nil {
			t.Fatalf("proof includes unverified fields, loses the exact source, or leaks into the record: %+v, %+v", sample, rows[i])
		}
	}
}

func TestLuaSampleKeepsUsableRowsFromMixedResult(t *testing.T) {
	const target = "https://example.org/list"
	code := `local r = http_request(TARGET_URL)
local title = html_find(r.body, "li")
local href = html_attr_all(r.body, "li", "href")[1]
return true, {items={{title=title, price=0, available=false, link=href, rating=0/0, source_response=r.source_response}, "unexpected value", {source_response=r.source_response}}}`
	result, err := engine.RunProgramWithRequester(context.Background(), engine.ProgramRequest{Code: code, TargetURL: target, AllowPartialItems: true}, engine.DefaultProgramLimits(), func(_ context.Context, opts acquisition.RequestOptions) (acquisition.Response, error) {
		return acquisition.Response{Receipt: acquisition.Receipt{URL: opts.URL, Status: 200, FetchedAt: time.Unix(1, 0)}, Body: `<li><a href="/candidate">Candidate</a></li>`}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	items, omitted := normalizeLuaItems(result.Collection["items"].([]any))
	records, _ := luaRecordsFromItems(items, result, target, 0)
	if len(records) != 1 || records[0].Fields["title"] != "Candidate" ||
		records[0].Fields["price"] != float64(0) || records[0].Fields["available"] != false ||
		omitted["link"] != 1 || omitted["rating"] != 1 ||
		len(result.ExecutionDiagnostics) != 1 || result.ExecutionDiagnostics[0].Code != "DOM_ATTRIBUTE_MISSING" {
		t.Fatalf("lost usable values or diagnostics: records=%#v, omitted=%v, diagnostics=%v", records, omitted, result.ExecutionDiagnostics)
	}
}

func TestLuaProbeFeedbackKeepsMetadataSeparateFromRecords(t *testing.T) {
	const target = "https://example.org/list"
	result, err := engine.RunProgramWithRequester(context.Background(), engine.ProgramRequest{
		Code:      `local r = http_request(TARGET_URL); return true, {items={}, meta={row_count=#html_find_all(r.body, "li")}}`,
		TargetURL: target,
	}, engine.DefaultProgramLimits(), func(_ context.Context, opts acquisition.RequestOptions) (acquisition.Response, error) {
		return acquisition.Response{Receipt: acquisition.Receipt{URL: opts.URL, Status: 200, FetchedAt: time.Unix(1, 0)}, Body: "<li>A</li><li>B</li>"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	items, _ := normalizeLuaItems(result.Collection["items"].([]any))
	feedback := luaProbeFeedback(result, true)
	meta, _ := feedback["probe"].(map[string]any)
	matches, _ := feedback["dom_matches"].([]luaDOMMatch)
	if len(items) != 0 || meta["row_count"] != float64(2) || len(matches) != 1 || matches[0].Matches != 2 {
		t.Fatalf("probe feedback lost or went into items: items=%v, feedback=%v", items, feedback)
	}
}

func TestRunLuaSampleAndBatchExecution(t *testing.T) {
	s, _ := retrievalService(t)
	s.Crawler.Fingerprint = "run-lua-sample-batch"
	ctx := context.Background()

	var calls atomic.Int32
	s.LuaRunner = func(_ context.Context, req programrunner.Request) (engine.ProgramResult, error) {
		n := calls.Add(1)
		target := req.Program.TargetURL
		if !req.Program.AllowPartialItems {
			t.Error("run_lua must allow usable records in both sample and batch")
		}
		if req.Program.SavedResponse != nil {
			return engine.RunProgramWithRequester(ctx, req.Program, req.Limits, nil)
		}
		if strings.Contains(target, "bad-empty") {
			return engine.ProgramResult{
				Collection: map[string]any{"items": []any{map[string]any{"title": "", "price": "   "}}},
				SourceEvidence: []acquisition.Response{{
					Receipt: acquisition.Receipt{URL: target, Status: 200, FetchedAt: time.Now().UTC().Truncate(time.Millisecond)},
					Body:    "<html><body><p>Empty fields test</p></body></html>",
				}},
			}, nil
		}
		return engine.ProgramResult{
			Collection: map[string]any{
				"items": []any{
					map[string]any{"title": fmt.Sprintf("Item from %s (#%d)", target, n), "price": 99.5, "source_url": target},
				},
			},
			SourceEvidence: []acquisition.Response{{
				Receipt: acquisition.Receipt{URL: target, Status: 200, FetchedAt: time.Now().UTC().Truncate(time.Millisecond)},
				Body:    fmt.Sprintf("<html><body><h1>%s</h1></body></html>", target),
			}},
			ExecutionDiagnostics: []engine.DOMDiagnostic{{Code: "DOM_ATTRIBUTE_MISSING", Attribute: "href"}},
		}, nil
	}

	r, err := s.Create(ctx, Input{
		RequestKey:  "lua-sample-batch-req",
		Goal:        "使用 Lua 抽样并批量抓取列表页",
		SourceScope: []string{"example.org"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Model(&Request{}).Where("id = ?", r.ID).Updates(map[string]any{"status": "running", "lease_token": "lease-lua", "lease_until": time.Now().Add(5 * time.Minute)}).Error; err != nil {
		t.Fatal(err)
	}
	r, _ = s.Request(ctx, r.ID)
	host := &informationHost{service: s, owner: requestExecutionOwner{RequestID: r.ID, Token: "lease-lua"}, request: r, input: Input{Goal: r.GoalForTest(), SourceScope: []string{"example.org"}}}

	// 1. Sample mode rejects items with zero non-empty scalar values cleanly.
	badRaw, err := host.invoke(ctx, "lua-bad-sample", "run_lua", `{"mode":"sample","url":"https://example.org/bad-empty","code":"return true, {}"}`)
	if err != nil {
		t.Fatal(err)
	}
	var badRec informationReceipt
	_ = json.Unmarshal([]byte(badRaw), &badRec)
	if badRec.Status == "succeeded" {
		t.Fatalf("expected all-empty sample row to fail lightweight plausibility check, got: %s", badRaw)
	}

	// 2. Sample mode succeeds cleanly on valid item, returning field_types and persisted observation count.
	goodSampleRaw, err := host.invoke(ctx, "lua-good-sample", "run_lua", `{"mode":"sample","url":"https://example.org/p1","code":"return true, {items={{title='A',price=99.5}}}"}`)
	if err != nil {
		t.Fatal(err)
	}
	var goodSample informationReceipt
	_ = json.Unmarshal([]byte(goodSampleRaw), &goodSample)
	if goodSample.Status != "succeeded" {
		t.Fatalf("expected valid sample to succeed, got: %s", goodSampleRaw)
	}
	dataMap := goodSample.Data.(map[string]any)
	if dataMap["mode"] != "sample" || dataMap["field_types"] == nil {
		t.Fatalf("expected sample validation metadata (mode/field_types), got: %+v", dataMap)
	}
	if !strings.Contains(goodSampleRaw, "target_diagnostics") || !strings.Contains(goodSampleRaw, "DOM_ATTRIBUTE_MISSING") {
		t.Fatalf("successful sampling did not return the runtime diagnostic: %s", goodSampleRaw)
	}

	// 3. Batch mode executes concurrently across multiple targets and persists observations.
	batchArgs := luaRunArgs{
		Mode: "batch",
		Code: "return true, {items={{title='Batch',price=99.5}}}",
		URLs: []string{"https://example.org/p2", "https://example.org/p3", "https://example.org/p4"},
	}
	batchOutRaw, err := host.invoke(ctx, "lua-batch-1", "run_lua", string(mustMarshal(batchArgs)))
	if err != nil {
		t.Fatal(err)
	}
	var batchRec informationReceipt
	_ = json.Unmarshal([]byte(batchOutRaw), &batchRec)
	if batchRec.Status != "succeeded" {
		t.Fatalf("expected batch run_lua to succeed, got: %s", batchOutRaw)
	}
	page, err := s.QueryObservations(ctx, r.ID, ObservationQuery{Limit: 20})
	if err != nil || page.Total != 4 {
		t.Fatalf("expected 4 total persisted observations (1 sample + 3 batch), got total=%d err=%v", page.Total, err)
	}
	refs := dataMap["references"].([]any)
	ref := refs[0].(map[string]any)
	replayRaw, err := host.invoke(ctx, "lua-snapshot", "run_lua", string(mustMarshal(map[string]any{
		"source_ref": ref,
		"code":       `local r=SOURCE_RESPONSE; return true,{items={{heading=html_find(r.body,"h1"),source_response=r.source_response}}}`,
	})))
	if err != nil {
		t.Fatal(err)
	}
	var replay struct {
		Status string `json:"status"`
		Data   struct {
			References []StoredReference     `json:"references"`
			Requests   []acquisition.Receipt `json:"requests"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(replayRaw), &replay); err != nil || replay.Status != "succeeded" || len(replay.Data.Requests) != 0 ||
		len(replay.Data.References) != 1 || replay.Data.References[0].DocID != ref["doc_id"] {
		t.Fatalf("snapshot did not preserve the original capture: %s (%v)", replayRaw, err)
	}
	filterRaw, err := host.invoke(ctx, "read-one-source", "read_materials", string(mustMarshal(MaterialReadQuery{
		DocumentIDs: []string{replay.Data.References[0].DocID},
		SourceIDs:   []string{agentdata.Hash("https://example.org/p1")},
		Limit:       1,
	})))
	if err != nil || !strings.Contains(filterRaw, `"total":2`) || !strings.Contains(filterRaw, `"next_cursor":`) {
		t.Fatalf("source and document filters did not apply before pagination: %s (%v)", filterRaw, err)
	}
}

func TestFaultRecoveryPrioritizesCompatibilityBeforeBrowserFallback(t *testing.T) {
	s, _ := retrievalService(t)
	ctx := context.Background()
	r, err := s.Create(ctx, Input{RequestKey: "browser-grant-order", Goal: "测试 403 先重试再降级 Browser", SourceScope: []string{"example.org"}})
	if err != nil {
		t.Fatal(err)
	}

	resp403 := acquisition.Response{Receipt: acquisition.Receipt{URL: "https://example.org/protected", Status: 403, FetchedAt: time.Now().UTC(), RequestProfile: "compatibility-retry"}, Body: `<html><body>Verify you are human</body></html>`}

	// First 403: should NOT unlock Browser immediately; agent should try header/Referer/Lua adjustment first.
	if approvedURL, ok := s.maybeApproveBrowserFromFetch(ctx, &r, Step{InputHash: "compatibility-retry"}, []string{"example.org"}, resp403); ok {
		t.Fatalf("expected first HTTP 403 NOT to jump straight to browser fallback, got approvedURL=%q", approvedURL)
	}

	// Simulate one recorded HTTP failure receipt for the request -> second 403 grants browser_extract!
	priorStep := Step{RequestID: r.ID, Key: "prior-http-retry", InputHash: "original", Kind: "acquire_sources", Stage: "failed"}
	if err := s.DB.Create(&priorStep).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Create(&taskDocument{RequestID: r.ID, ReceiptID: priorStep.ID, SourceURL: resp403.Receipt.URL, Status: "failed", HTTPStatus: 403, ResponseKey: "response:0", RequestProfile: "original"}).Error; err != nil {
		t.Fatal(err)
	}
	unchanged := resp403
	unchanged.Receipt.RequestProfile = "original"
	if _, ok := s.maybeApproveBrowserFromFetch(ctx, &r, Step{InputHash: "different-script"}, []string{"example.org"}, unchanged); ok {
		t.Fatal("changing the Lua script alone unlocked Browser while the actual HTTP request stayed the same")
	}
	approvedURL, ok := s.maybeApproveBrowserFromFetch(ctx, &r, Step{InputHash: "compatibility-retry"}, []string{"example.org"}, resp403)
	if !ok || approvedURL != "https://example.org/protected" {
		t.Fatalf("expected HTTP 403 after prior retry to unlock browser fallback, got ok=%v url=%q", ok, approvedURL)
	}

	// A script-only shell can justify rendering without an unrelated HTTP retry.
	rJS, err := s.Create(ctx, Input{RequestKey: "browser-grant-js", Goal: "测试纯 JS 壳页", SourceScope: []string{"example.org"}})
	if err != nil {
		t.Fatal(err)
	}
	respJS := acquisition.Response{
		Receipt: acquisition.Receipt{URL: "https://example.org/spa", Status: 200, FetchedAt: time.Now().UTC()},
		Body:    `<html><body><div id="root"></div><script src="/bundle.js"></script></body></html>`,
	}
	if jsURL, jsOK := s.maybeApproveBrowserFromFetch(ctx, &rJS, Step{}, []string{"example.org"}, respJS); !jsOK || jsURL != "https://example.org/spa" {
		t.Fatalf("expected JS-only shell page to unlock browser fallback immediately, got ok=%v url=%q", jsOK, jsURL)
	}
}

func TestDiscoverSourcesSpawnsSymmetricSubtask(t *testing.T) {
	s, _ := retrievalService(t)
	s.Search = nil
	ctx := context.Background()

	parent, err := s.Create(ctx, Input{RequestKey: "sym-parent", Goal: "Find official Go download URL", SourceScope: []string{"go.dev"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Model(&Request{}).Where("id = ?", parent.ID).Updates(map[string]any{"status": "running", "lease_token": "lease-sym", "lease_until": time.Now().Add(5 * time.Minute)}).Error; err != nil {
		t.Fatal(err)
	}
	parent, _ = s.Request(ctx, parent.ID)
	host := &informationHost{service: s, owner: requestExecutionOwner{RequestID: parent.ID, Token: "lease-sym"}, request: parent, input: Input{Goal: parent.GoalForTest(), SourceScope: []string{"go.dev"}}}

	// 1. Calling discover_sources without Search provider spawns an independent Request with purpose='source_discovery' and waits on it.
	raw1, err := host.invoke(ctx, "disc-1", "discover_sources", `{"query":"golang downloads"}`)
	if err == nil {
		var waitRec informationReceipt
		_ = json.Unmarshal([]byte(raw1), &waitRec)
		if waitRec.Status != "waiting" {
			t.Fatalf("expected waiting receipt when source_discovery subtask spawned, got: %s", raw1)
		}
	}
	var sub Request
	if err := s.DB.Where("purpose = ?", "source_discovery").First(&sub).Error; err != nil {
		t.Fatalf("expected symmetric Request with purpose='source_discovery' created: %v", err)
	}

	subHost := &informationHost{service: s, request: sub}
	subTools, _ := subHost.stateMachineTools(map[string]any{})
	for _, tool := range subTools {
		if tool.Function.Name == "discover_sources" {
			return
		}
	}
	t.Fatal("discovery worker cannot look up sources directly")
}

func TestAcquisitionMetadataProvidesCompactHTMLOutlineWithoutProse(t *testing.T) {
	longBody := strings.Repeat("Extensive full prose paragraph that must never be dumped to the model. ", 12)
	html := `<html><head><title>Releases Overview</title></head><body>
		<nav><a href="/home">Home</a><a href="/docs">Docs</a></nav>
		<main>
			<h1>Supported Releases</h1>
			<ul class="release-list hover:bg-blue-500">
				<li class="release-item"><a href="/dl/2.1.0">Version 2.1.0</a><time>2026-11-10</time><p>` + longBody + `</p></li>
				<li class="release-item"><a href="/dl/2.0.9">Version 2.0.9</a><time>2026-10-01</time><p>Second release notes</p></li>
			</ul>
			<table class="matrix"><thead><tr><th>Branch</th><th>Status</th></tr></thead>
				<tbody><tr><td>17</td><td>Current</td></tr><tr><td>16</td><td>Supported</td></tr></tbody>
			</table>
		</main>
	</body></html>`
	meta := acquisitionResponseMetadata(acquisition.Response{
		Receipt: acquisition.Receipt{URL: "https://example.org/releases", Status: 200, Headers: map[string]string{"content-type": "text/html"}},
		Body:    html,
	})
	if meta["is_html"] != true {
		t.Fatalf("expected is_html=true, got %+v", meta)
	}
	encoded, _ := json.Marshal(meta)
	raw := string(encoded)
	if len(raw) > 3000 {
		t.Fatalf("expected compact DOM outline instead of full-page dump, len=%d: %s", len(raw), raw)
	}
	for _, forbidden := range []string{"Releases Overview", "Supported Releases", "must never be dumped", "Version 2.1.0", "Current", "Second release notes"} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("expected raw HTML text %q never leaked in structure metadata: %s", forbidden, raw)
		}
	}
	outline, _ := meta["html_outline"].(map[string]any)
	blocks, _ := outline["blocks"].([]map[string]any)
	if len(blocks) < 2 {
		t.Fatalf("expected >=2 structural blocks in html_outline, got %+v", outline)
	}
	if !strings.Contains(raw, "release-item") || !strings.Contains(raw, "table.matrix tbody tr") {
		t.Fatalf("expected exact container CSS selectors in html_outline, got %s", raw)
	}
	sanitized := sanitizeProbeMeta(map[string]any{"sample": longBody, "row_count": 2})
	if len([]rune(sanitized["sample"].(string))) > 125 || sanitized["row_count"] != 2 {
		t.Fatalf("expected probe meta string truncated cleanly, got %+v", sanitized)
	}

	dynamicHTML := `<html><body>
		<div id="mobile-menu"><div class="mobile-submenu"><a href="/blog/news">Blog</a><a href="/partners">Partners</a></div></div>
		<div class="bzfooterlinks"><a href="/legal/terms">Terms</a></div>
		<main>
			<div data-token-prefix="https://cdn.example.org/maps" data-token-paths='["svc/a1","svc/a2","svc/a3","svc/a4","svc/a5","svc/a6","svc/a7","svc/a8","svc/a9","svc/opaque01234567890123456789012345","svc/xfer"]' data-template="{tpl!a!b} {tpl!c!d}">{tpl!x!y}</div>
			<p>Read <a href="/app/tx-classes">details</a> or <a href="/blog/release">blog</a>.</p>
			<script>const PRICES = {'b2-tb': 6.95, 'b2-gb': 0.00695,}; window.AppCfg = {"rate": 1}; window.AppPrices = PRICES;</script>
		</main>
	</body></html>`
	dynMeta := acquisitionResponseMetadata(acquisition.Response{
		Receipt: acquisition.Receipt{URL: "https://example.org/app/pricing", Status: 200, Headers: map[string]string{"content-type": "text/html"}},
		Body:    dynamicHTML,
	})
	dynRaw := string(mustMarshal(dynMeta))
	if !strings.Contains(dynRaw, "https://cdn.example.org/maps") ||
		!strings.Contains(dynRaw, "svc/xfer") ||
		!strings.Contains(dynRaw, "https://example.org/app/tx-classes") ||
		!strings.Contains(dynRaw, "window.AppCfg") ||
		!strings.Contains(dynRaw, "window.AppPrices") ||
		!strings.Contains(dynRaw, "b2-tb") ||
		!strings.Contains(dynRaw, `"script_index":1`) ||
		!strings.Contains(dynRaw, "unrendered_templates") ||
		strings.Contains(dynRaw, "/legal/terms") ||
		strings.Contains(dynRaw, "/partners") {
		t.Fatalf("expected data_endpoints, >8 data_endpoint_paths, prioritized related_links (without menu/footer noise), inline script keys/index, and unrendered_templates in html_outline, got %s", dynRaw)
	}
	if !strings.Contains(raw, "header_names") || !strings.Contains(raw, "https://example.org/dl/2.1.0") {
		t.Fatalf("expected table header_names and related_links in html_outline, got %s", raw)
	}

	largeDict := map[string]any{}
	for i := 0; i < 60; i++ {
		skus := map[string]any{}
		for j := 0; j < 80; j++ {
			skus[fmt.Sprintf("SKU-%03d", j)] = map[string]any{"price": "0.023", "unit": "GB"}
		}
		largeDict[fmt.Sprintf("Region-%02d", i)] = skus
	}
	dictMeta := acquisitionResponseMetadata(acquisition.Response{
		Receipt: acquisition.Receipt{URL: "https://example.org/catalog.json", Status: 200, Headers: map[string]string{"content-type": "application/json"}},
		Body:    string(mustMarshal(map[string]any{"regions": largeDict})),
	})
	dictRaw := string(mustMarshal(dictMeta))
	if len(dictRaw) > 6000 || !strings.Contains(dictRaw, `"key_count":60`) || !strings.Contains(dictRaw, "sample_keys") {
		t.Fatalf("expected large dictionary JSON inspection to stay compact with key_count and sample_keys, len=%d: %s", len(dictRaw), dictRaw)
	}
}

func TestRunLuaRejectsProbeOnlyAndExposesHTMLOutlineOnFailure(t *testing.T) {
	s, _ := retrievalService(t)
	s.Crawler.Fingerprint = "run-lua-no-probe"
	ctx := context.Background()

	s.LuaRunner = func(_ context.Context, req programrunner.Request) (engine.ProgramResult, error) {
		return engine.RunProgramWithRequester(ctx, req.Program, req.Limits, func(_ context.Context, opts acquisition.RequestOptions) (acquisition.Response, error) {
			return acquisition.Response{
				Receipt: acquisition.Receipt{URL: opts.URL, Status: 200, Headers: map[string]string{"content-type": "text/html"}, FetchedAt: time.Now().UTC()},
				Body:    `<html><head><title>DL</title></head><body><main><ul class="dl-list"><li><a href="/v1">v1.0</a></li><li><a href="/v2">v2.0</a></li></ul></main></body></html>`,
			}, nil
		})
	}
	r, err := s.Create(ctx, Input{RequestKey: "lua-reject-probe", Goal: "提取下载列表", SourceScope: []string{"example.org"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Model(&Request{}).Where("id = ?", r.ID).Updates(map[string]any{"status": "running", "lease_token": "lease-np", "lease_until": time.Now().Add(5 * time.Minute)}).Error; err != nil {
		t.Fatal(err)
	}
	r, _ = s.Request(ctx, r.ID)
	host := &informationHost{service: s, owner: requestExecutionOwner{RequestID: r.ID, Token: "lease-np"}, request: r, input: Input{Goal: r.GoalForTest(), SourceScope: []string{"example.org"}}}

	// Empty items + meta without empty_reason must fail and return compact html_outline diagnostics.
	raw, err := host.invoke(ctx, "lua-probe-reject", "run_lua", `{"mode":"sample","url":"https://example.org/dl","code":"local r=http_request(TARGET_URL); local _=html_find(r.body, '.missing'); return true, {items={}, meta={sample=clean_text(r.body)}}"}`)
	if err != nil {
		t.Fatal(err)
	}
	var rec informationReceipt
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Status != "failed" || rec.Code != "LUA_EXECUTION_FAILED" {
		t.Fatalf("expected probe-only empty items to fail with LUA_EXECUTION_FAILED, got: %s", raw)
	}
	if !strings.Contains(raw, "html_outline") || !strings.Contains(raw, "dl-list") {
		t.Fatalf("expected html_outline structural diagnostic on failure, got: %s", raw)
	}

	rawDump, err := host.invoke(ctx, "lua-raw-dump-reject", "run_lua", `{"mode":"sample","url":"https://example.org/dl","code":"local r=http_request(TARGET_URL); return true, {items={{url=TARGET_URL, text=string.rep(\"a\", 3000)}}}"}`)
	if err != nil {
		t.Fatal(err)
	}
	var dumpRec informationReceipt
	if err := json.Unmarshal([]byte(rawDump), &dumpRec); err != nil {
		t.Fatal(err)
	}
	if dumpRec.Status != "failed" || dumpRec.Code != "LUA_EXECUTION_FAILED" || !strings.Contains(rawDump, "oversized raw text/body dump") {
		t.Fatalf("expected oversized raw text dump to be rejected cleanly, got: %s", rawDump)
	}
}

func TestResolveLuaTargetsAllowsSelfContainedHTTPAndUpdatePlanNormalizes(t *testing.T) {
	targets, err := resolveLuaTargets(luaRunArgs{
		Code: `local r = http_request({url="https://example.org/data"}); return true, {items={{v="1", source_response=r.source_response}}}`,
	}, nil, []string{"example.org"})
	if err != nil || len(targets) != 1 {
		t.Fatalf("expected self-contained http_request script to run once without top-level url, got targets=%+v err=%v", targets, err)
	}
	if _, err := resolveLuaTargets(luaRunArgs{Code: `return true, {items={}}`}, nil, []string{"example.org"}); err == nil {
		t.Fatal("expected non-HTTP script without url/targets to still fail with SOURCE_URLS_REQUIRED")
	}
	if err := validateInformationToolArgs("update_plan", `{"interpretation":"ok","plan":"summary","sources":[]}`); err != nil {
		t.Fatalf("expected update_plan to accept/normalize optional plan field cleanly, got: %v", err)
	}
}

func TestScriptSrcsSummaryPreservationAndByteBudgetedPreviews(t *testing.T) {
	portalHTML := `<html><head><script src="https://cdn.example.com/analytics.js"></script><script src="/assets/js/api.js"></script><script src="./assets/js/jobs.js"></script></head><body><main><h1>Jobs</h1></main></body></html>`
	meta := acquisitionResponseMetadata(acquisition.Response{
		Receipt: acquisition.Receipt{URL: "https://hr.example.org/position", Status: 200, Headers: map[string]string{"content-type": "text/html"}},
		Body:    portalHTML,
	})
	rawMeta := string(mustMarshal(meta))
	if !strings.Contains(rawMeta, `"script_srcs"`) || !strings.Contains(rawMeta, "https://hr.example.org/assets/js/api.js") || !strings.Contains(rawMeta, "https://hr.example.org/assets/js/jobs.js") {
		t.Fatalf("expected same-host script_srcs in html_outline, got %s", rawMeta)
	}

	now := time.Now().UTC()
	longSummary := strings.Repeat("Go backend requirement line.\n", 8)
	var obs []Observation
	for i := 0; i < 60; i++ {
		obs = append(obs, Observation{
			ID:        fmt.Sprintf("obs-%02d", i),
			ReceiptID: 1,
			FetchedAt: now,
			Fields: map[string]any{
				"source_url": "https://hr.example.org/website/api/search?kw=Go&page=1",
				"title":      fmt.Sprintf("Go Engineer %02d", i),
				"summary":    longSummary,
			},
		})
	}
	if shared, ok := sharedObservationSourceURL(obs); !ok || shared == "" {
		t.Fatalf("expected shared_source_url detected for single-source batch")
	}
	summaryPreviews := compactObservationPreviewsWithLimits(obs[:1], 8192, 0)
	if len(summaryPreviews) != 1 || summaryPreviews[0]["summary"] == nil || !strings.Contains(summaryPreviews[0]["summary"].(string), "\n") {
		t.Fatalf("expected multiline >180-rune summary field preserved, got %+v", summaryPreviews)
	}
	if _, hasDupURL := summaryPreviews[0]["source_url"]; hasDupURL {
		t.Fatalf("expected redundant source_url omitted from single-source preview items, got %+v", summaryPreviews[0])
	}

	var compactObs []Observation
	for i := 0; i < 70; i++ {
		compactObs = append(compactObs, Observation{
			ID:        fmt.Sprintf("c-%02d", i),
			ReceiptID: 1,
			FetchedAt: now,
			Fields: map[string]any{
				"source_url": "https://cdn.example.org/very/long/shared/pricing/catalog/v1/rates.json",
				"name":       fmt.Sprintf("SKU_%02d", i),
				"price":      "0.023",
			},
		})
	}
	allFit := compactObservationPreviewsWithLimits(compactObs, 8192, 0)
	if len(allFit) != 70 {
		t.Fatalf("expected all 70 compact single-source rows to fit in 8192B budget without fixed 35-item cap, got %d", len(allFit))
	}

	progRes := engine.ProgramResult{
		SourceEvidence: []acquisition.Response{
			{Receipt: acquisition.Receipt{URL: "https://hr.example.org/a", Status: 200, FetchedAt: now}, Body: "ok"},
			{Receipt: acquisition.Receipt{URL: "https://hr.example.org/b", Status: 200, FetchedAt: now}, Body: "ok"},
		},
	}
	recs, ok := luaRecordsFromItems([]map[string]any{{"title": "fallback row"}}, progRes, "", 0, float64(2))
	if !ok || len(recs) != 1 || recs[0].Fields["source_url"] != "https://hr.example.org/b" {
		t.Fatalf("expected top-level source_response fallback to resolve record provenance, got ok=%v recs=%+v", ok, recs)
	}

	apis := compactBrowserNetworkAPIs([]acquisition.NetworkAPICapture{
		{Method: "POST", URL: "https://tracker.other.com/v1/list", Status: 200, ContentType: "application/json", ResponseSchema: json.RawMessage(`[{"pointer":"","type":"object"}]`)},
		{Method: "POST", URL: "https://jobs.example.com/api/v1/search?_sig=1", Status: 405, ContentType: "application/json"},
		{Method: "POST", URL: "https://jobs.example.com/api/v1/csrf/token", Status: 200, ContentType: "application/json", SetsCookie: true, RequestHeaders: map[string]string{"website-path": "society"}, ResponseSchema: json.RawMessage(`[{"pointer":"/data/token","type":"string"}]`)},
		{Method: "POST", URL: "https://jobs.example.com/api/v1/search?_sig=2", Status: 200, ContentType: "application/json", SentCookies: true, RequestHeaders: map[string]string{"website-path": "society"}, ResponseSchema: json.RawMessage(`[{"pointer":"/data/count","type":"number"}]`)},
	}, "https://jobs.example.com/experienced/position", 4000)
	if len(apis) != 3 || apis[0].URL != "https://jobs.example.com/api/v1/csrf/token" || apis[1].Status != 200 || !apis[1].SentCookies {
		t.Fatalf("expected deduped 200 same-site APIs prioritized over pre-CSRF 405 and cross-site tracker, got %+v", apis)
	}
}
