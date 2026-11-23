package main

import (
	"bytes"
	"context"
	"encoding/json"
	"gorelay/internal/programrunner"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gorelay/internal/acquisition"
	"gorelay/internal/engine"
	"gorelay/internal/fetchgov"
)

func TestRunnerHTTPAuthAndHostBudgetCaps(t *testing.T) {
	called := false
	execute := func(ctx context.Context, in programrunner.Request) (programrunner.Response, error) {
		called = true
		if in.Limits.MaxRequests != 3 || in.Limits.MaxBytes != 1024 || in.Limits.MaxItems != 5 {
			t.Errorf("unbounded limits: %+v", in.Limits)
		}
		return programrunner.Response{Result: &engine.ProgramResult{Collection: map[string]any{"items": []any{}}}}, nil
	}
	h := runnerHandler("key", engine.ProgramLimits{MaxRequests: 3, MaxBytes: 1024, MaxItems: 5, MaxOutputBytes: 2048}, execute)
	req := httptest.NewRequest("POST", "/execute", bytes.NewBufferString(`{"program":{"code":"return true,{items={}}"}}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized || called {
		t.Fatal("unauthenticated runner executed")
	}
	payload, _ := json.Marshal(programrunner.Request{Program: engine.ProgramRequest{Code: "x"}, Limits: engine.ProgramLimits{MaxRequests: 999, MaxBytes: 9999, MaxItems: 999}})
	req = httptest.NewRequest("POST", "/execute", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer key")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 || !called {
		t.Fatalf("execute failed: %d %s", w.Code, w.Body.String())
	}
}

func TestRunnerBoundedOutputBuffer(t *testing.T) {
	b := boundedBuffer{limit: 4}
	if _, err := b.Write([]byte("12345")); err == nil || b.Len() > 4 {
		t.Fatal("child output buffer is unbounded")
	}
}

func TestRunnerReusesExactEvidenceWithoutReturningItsBody(t *testing.T) {
	ctx := context.Background()
	const target = "https://catalog.example/schedule.json"
	fetcher, err := acquisition.NewPublicFetcher(acquisition.Policy{})
	if err != nil {
		t.Fatal(err)
	}
	defer fetcher.Close()
	keys := fetcher.SnapshotCandidates(ctx, acquisition.RequestOptions{URL: target})
	if len(keys) == 0 {
		t.Fatal("missing reusable test snapshot key")
	}
	now := time.Now().UTC()
	saved := acquisition.Response{Body: strings.Repeat("班次 Monday 08:00\n", 1<<15), Receipt: acquisition.Receipt{URL: "https://catalog.example/timetable", Status: 200, FetchedAt: now}}
	other := acquisition.Response{Body: `{"items":[` + strings.Repeat(`{"weekday":"Monday","start":"08:00"},`, 1<<16) + `{}]}`, Receipt: acquisition.Receipt{URL: target, Status: 200, SnapshotKey: keys[0], FetchedAt: now}}
	request := programrunner.Request{Program: engine.ProgramRequest{TargetURL: target, SavedResponse: &saved,
		Code: `local r = http_request({url=TARGET_URL}); return true, {items={{bytes=#r.body, saved=#SOURCE_RESPONSE.body}}}`},
		Snapshots: map[string]acquisition.Response{keys[0]: other}}
	handler := runnerHandler("key", engine.ProgramLimits{}, programrunner.Run)
	baseline := func() int {
		payload, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		incoming := httptest.NewRequest("POST", "/execute", bytes.NewReader(payload))
		incoming.Header.Set("Authorization", "Bearer key")
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, incoming)
		if out.Code != 200 {
			t.Fatalf("legacy protocol failed: %d", out.Code)
		}
		return out.Body.Len()
	}()
	type exchange struct {
		bytes, status int
		requestBytes  int64
		batch         string
	}
	responses := make(chan exchange, 1)
	batchDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/snapshot-batch" {
			handler.ServeHTTP(w, r)
			close(batchDone)
			return
		}
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, r)
		responses <- exchange{out.Body.Len(), out.Code, r.ContentLength, r.Header.Get(programrunner.SnapshotBatchHeader)}
		w.WriteHeader(out.Code)
		_, _ = w.Write(out.Body.Bytes())
	}))
	defer server.Close()
	client, err := programrunner.NewRunnerClient(server.URL, "key")
	if err != nil {
		t.Fatal(err)
	}
	batchCtx, closeBatch, err := client.ShareSnapshots(ctx, request.Snapshots)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBatch()
	var batchID string
	for _, fail := range []bool{false, true} {
		if fail {
			request.Program.Code = `local r = http_request({url=TARGET_URL}); error("fixture failure")`
		}
		got, err := client.Execute(batchCtx, request)
		var wire exchange
		select {
		case wire = <-responses:
		default:
			t.Fatalf("client never reached runner: %v", err)
		}
		if (err != nil) != fail || got.Stats.Requests != 0 || len(got.SourceEvidence) != 2 {
			t.Fatalf("execution or evidence changed: error=%v, HTTP=%d, captures=%d", err, got.Stats.Requests, len(got.SourceEvidence))
		}
		if got.SourceEvidence[0].Body != saved.Body || got.SourceEvidence[1].Body != other.Body || !got.SourceEvidence[1].Receipt.ReusedSnapshot {
			t.Fatal("client did not restore original bodies and receipt order")
		}
		if wire.bytes >= baseline-len(saved.Body)-len(other.Body) {
			t.Fatalf("runner resent cached bodies: baseline %d, actual %d", baseline, wire.bytes)
		}
		if wire.batch == "" || (batchID != "" && wire.batch != batchID) || wire.requestBytes >= int64(len(other.Body)) {
			t.Fatalf("tool targets did not share an uploaded snapshot: %+v", wire)
		}
		batchID = wire.batch
		t.Logf("runner reply: %d legacy bytes, %d with snapshot references (HTTP %d)", baseline, wire.bytes, wire.status)
	}
	closeBatch()
	select {
	case <-batchDone:
	case <-time.After(time.Second):
		t.Fatal("runner did not release the batch when its owner disconnected")
	}
	incoming := httptest.NewRequest("POST", "/execute", strings.NewReader(`{"program":{"code":"return true,{}"}}`))
	incoming.Header.Set("Authorization", "Bearer key")
	incoming.Header.Set(programrunner.SnapshotBatchHeader, batchID)
	out := httptest.NewRecorder()
	handler.ServeHTTP(out, incoming)
	if out.Code != http.StatusConflict || !strings.Contains(out.Body.String(), "snapshot_batch_unavailable") {
		t.Fatalf("expired batch was accepted: %d %s", out.Code, out.Body.String())
	}
}

func TestSnapshotPipeBoundsOutputAndBlocksRepeatedKeys(t *testing.T) {
	snaps := map[string]acquisition.Response{
		"k1": {Body: `{"ok":true}`, Receipt: acquisition.Receipt{URL: "https://example.org/a", Status: 200}},
	}
	reqJSON := []byte(`{"program":{"code":"return true,{}"}}`)

	t.Run("distinct lookup under byte limit succeeds", func(t *testing.T) {
		inR, inW := io.Pipe()
		outR, outW := io.Pipe()
		go func() {
			defer inR.Close()
			defer outW.Close()
			var req programrunner.Request
			_ = json.NewDecoder(inR).Decode(&req)
			enc := json.NewEncoder(outW)
			_ = enc.Encode(childSnapshotFrame{Keys: []string{"k1"}})
			var hits map[string]acquisition.Response
			_ = json.NewDecoder(inR).Decode(&hits)
			res := programrunner.Response{Result: &engine.ProgramResult{Collection: map[string]any{"items": []any{map[string]any{"hits": len(hits)}}}}}
			_ = enc.Encode(childSnapshotFrame{Result: &res})
		}()
		resp, err := exchangeChildSnapshots(inW, outR, reqJSON, snaps, 4096)
		_ = inW.Close()
		_ = outR.Close()
		if err != nil || resp.Result == nil {
			t.Fatalf("expected valid pipe exchange to pass, got resp=%+v err=%v", resp, err)
		}
	})

	t.Run("repeated lookup key rejected", func(t *testing.T) {
		inR, inW := io.Pipe()
		outR, outW := io.Pipe()
		go func() {
			defer inR.Close()
			defer outW.Close()
			var req programrunner.Request
			_ = json.NewDecoder(inR).Decode(&req)
			enc := json.NewEncoder(outW)
			_ = enc.Encode(childSnapshotFrame{Keys: []string{"k1"}})
			var hits map[string]acquisition.Response
			_ = json.NewDecoder(inR).Decode(&hits)
			_ = enc.Encode(childSnapshotFrame{Keys: []string{"k1"}})
		}()
		_, err := exchangeChildSnapshots(inW, outR, reqJSON, snaps, 4096)
		_ = inW.Close()
		_ = outR.Close()
		if err == nil || !strings.Contains(err.Error(), "repeated snapshot lookup") {
			t.Fatalf("expected repeated lookup error, got %v", err)
		}
	})

	t.Run("oversized result frame rejected", func(t *testing.T) {
		var childOut bytes.Buffer
		oversized := programrunner.Response{Result: &engine.ProgramResult{Collection: map[string]any{"blob": strings.Repeat("x", 512)}}}
		_ = json.NewEncoder(&childOut).Encode(childSnapshotFrame{Result: &oversized})
		_, err := exchangeChildSnapshots(io.Discard, &childOut, reqJSON, snaps, 128)
		if err == nil || !strings.Contains(err.Error(), "child output limit exceeded") {
			t.Fatalf("expected child output limit exceeded, got %v", err)
		}
	})
}

func TestRunnerClientSetsSnapshotReferencesBeforeMarshal(t *testing.T) {
	seenJSONFlag := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/execute" {
			http.NotFound(w, r)
			return
		}
		var req programrunner.Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		seenJSONFlag = req.SnapshotReferences
		_ = json.NewEncoder(w).Encode(programrunner.Response{Result: &engine.ProgramResult{Collection: map[string]any{"items": []any{}}}})
	}))
	defer server.Close()
	client, err := programrunner.NewRunnerClient(server.URL, "key")
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Execute(context.Background(), programrunner.Request{
		Program:   engine.ProgramRequest{Code: "return true,{}"},
		Snapshots: map[string]acquisition.Response{"k": {Body: "ok"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !seenJSONFlag {
		t.Fatal("expected SnapshotReferences=true in initial marshaled request JSON")
	}
}

func TestCheckpointPipeRoundTripWithChildProcess(t *testing.T) {
	var cur any = map[string]any{"next_page": 5}
	var saved [][]map[string]any
	cp := &engine.CheckpointHooks{
		Get: func(context.Context) (any, error) { return cur, nil },
		Save: func(_ context.Context, cursor any, records []map[string]any, _ []acquisition.Response) error {
			cur = cursor
			saved = append(saved, records)
			return nil
		},
	}

	req := programrunner.Request{
		HasCheckpoint: true,
		Program: engine.ProgramRequest{
			Code: `
local cp = checkpoint.get()
checkpoint.save({next_page = cp.next_page + 1}, {{id = "p5", url = "https://example.com/5"}})
return true, {items = {{id = "p5", url = "https://example.com/5"}}}
`,
		},
	}
	payload, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	childErr := make(chan error, 1)
	go func() {
		defer outW.Close()
		defer inR.Close()
		childErr <- runSnapshotChild(inR, outW)
	}()

	resp, err := exchangeChildPipe(context.Background(), inW, outR, payload, nil, 0, cp)
	_ = inW.Close()
	_ = outR.Close()
	if err != nil {
		t.Fatalf("exchangeChildPipe failed: %v", err)
	}
	if ce := <-childErr; ce != nil {
		t.Fatalf("runSnapshotChild failed: %v", ce)
	}
	if resp.Result == nil || len(saved) != 1 || saved[0][0]["id"] != "p5" {
		t.Fatalf("unexpected checkpoint pipe result: resp=%+v saved=%+v", resp, saved)
	}
	if m, ok := cur.(map[string]any); !ok || m["next_page"] != float64(6) {
		t.Fatalf("expected updated cursor next_page=6, got %#v", cur)
	}
}

type ipcStubRequester struct {
	fn func(ctx context.Context, opts acquisition.RequestOptions) (acquisition.Response, error)
}

func (s *ipcStubRequester) FetchRequest(ctx context.Context, opts acquisition.RequestOptions) (acquisition.Response, error) {
	return s.fn(ctx, opts)
}
func (s *ipcStubRequester) Close() {}

type ipcStubRenderer struct {
	fn func(ctx context.Context, req fetchgov.RenderRequest) (fetchgov.RenderResult, error)
}

func (r *ipcStubRenderer) RenderAndSniff(ctx context.Context, req fetchgov.RenderRequest) (fetchgov.RenderResult, error) {
	return r.fn(ctx, req)
}

func runPipeRoundTrip(t *testing.T, req programrunner.Request, gov *fetchgov.SafeClient) programrunner.Response {
	t.Helper()
	childReq := req
	childReq.AuthProfile = ""
	payload, err := json.Marshal(childReq)
	if err != nil {
		t.Fatal(err)
	}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	childErr := make(chan error, 1)
	go func() {
		defer outW.Close()
		defer inR.Close()
		childErr <- runSnapshotChild(inR, outW)
	}()
	resp, err := exchangeChildPipeWithHost(context.Background(), inW, outR, req, payload, nil, 0, nil, gov)
	_ = inW.Close()
	_ = outR.Close()
	if err != nil {
		t.Fatalf("exchangeChildPipeWithHost failed: %v", err)
	}
	if ce := <-childErr; ce != nil {
		t.Fatalf("runSnapshotChild failed: %v", ce)
	}
	return resp
}

func TestChildIPCSharesParentSafeClientCircuitBreakerAndAuthProfile(t *testing.T) {
	var calls int
	var seenAuth string
	return429 := false

	cfg := fetchgov.DefaultConfig()
	cfg.RespectRobots = false
	cfg.RequestsPerSec = 100
	cfg.Burst = 100
	cfg.CircuitThreshold = 2
	cfg.CircuitCooldown = 30 * time.Second
	cfg.AuthProfiles = map[string]fetchgov.AuthProfile{
		"corp_api": {
			Name:           "corp_api",
			AllowedDomains: []string{"api.example.com"},
			HeaderName:     "Authorization",
			HeaderPrefix:   "Bearer ",
			Secret:         "parent-only-secret",
		},
	}
	cfg.RequesterFactory = func(acquisition.Policy) (fetchgov.Requester, error) {
		return &ipcStubRequester{fn: func(_ context.Context, opts acquisition.RequestOptions) (acquisition.Response, error) {
			calls++
			seenAuth = opts.Headers["Authorization"]
			if return429 {
				return acquisition.Response{
					Body:    "rate limited",
					Receipt: acquisition.Receipt{URL: opts.URL, Status: 429, Headers: map[string]string{"retry-after": "30"}},
				}, nil
			}
			return acquisition.Response{
				Body:    `{"items":[{"id":"1","title":"Verified"}]}`,
				Receipt: acquisition.Receipt{URL: opts.URL, Status: 200, Headers: map[string]string{"content-type": "application/json"}},
			}, nil
		}}, nil
	}
	sharedClient, err := fetchgov.NewSafeClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer sharedClient.Close()

	req := programrunner.Request{
		AuthProfile:    "corp_api",
		AllowedDomains: []string{"api.example.com"},
		Program: engine.ProgramRequest{
			TargetURL: "https://api.example.com/v1/items",
			Code: `
local body, err = http_get(TARGET_URL)
if err then return false, err end
local data = json_decode(body)
return true, {items = data.items}
`,
		},
	}

	// Run 1: succeeds and parent injects scoped AuthProfile header without exposing it to child.
	r1 := runPipeRoundTrip(t, req, sharedClient)
	if r1.Error != nil || r1.Result == nil || seenAuth != "Bearer parent-only-secret" || calls != 1 {
		t.Fatalf("run 1 failed: err=%+v seenAuth=%q calls=%d", r1.Error, seenAuth, calls)
	}

	// Run 2: upstream returns HTTP 429 with Retry-After -> trips parent SafeClient circuit breaker.
	return429 = true
	r2 := runPipeRoundTrip(t, req, sharedClient)
	if r2.Error == nil || r2.Error.Code != "rate_limited" {
		t.Fatalf("expected rate_limited on run 2, got %+v", r2.Error)
	}
	if calls != 2 {
		t.Fatalf("expected 2 upstream calls before circuit opens, got %d", calls)
	}

	// Run 3: brand new child execution hits shared parent CIRCUIT_OPEN with ZERO upstream call!
	r3 := runPipeRoundTrip(t, req, sharedClient)
	if r3.Error == nil || r3.Error.Code != "circuit_open" {
		t.Fatalf("expected shared parent circuit_open on run 3, got %+v", r3.Error)
	}
	if calls != 2 {
		t.Fatalf("circuit breaker did not short-circuit across child executions: calls=%d want 2", calls)
	}
}

func TestChildIPCHTTPRenderProxiesToSidecarRenderer(t *testing.T) {
	var capturedSelector string
	cfg := fetchgov.DefaultConfig()
	cfg.RespectRobots = false
	cfg.RequestsPerSec = 100
	cfg.Burst = 100
	cfg.Renderer = &ipcStubRenderer{fn: func(_ context.Context, req fetchgov.RenderRequest) (fetchgov.RenderResult, error) {
		capturedSelector = req.WaitSelector
		return fetchgov.RenderResult{
			FinalURL: req.URL,
			HTML:     `<html><body><div class="job-title">Go Systems Intern</div></body></html>`,
		}, nil
	}}
	sharedClient, err := fetchgov.NewSafeClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer sharedClient.Close()

	req := programrunner.Request{
		AllowedDomains: []string{"spa.example.com"},
		Program: engine.ProgramRequest{
			TargetURL: "https://spa.example.com/careers",
			Code: `
local html, err = http_render(TARGET_URL, ".job-title")
if err then return false, err end
local title, ferr = html_find(html, ".job-title")
if ferr then return false, ferr end
return true, {items = {{title = title, url = TARGET_URL}}}
`,
		},
	}

	resp := runPipeRoundTrip(t, req, sharedClient)
	if resp.Error != nil || resp.Result == nil {
		t.Fatalf("http_render child IPC failed: err=%+v", resp.Error)
	}
	if capturedSelector != ".job-title" {
		t.Fatalf("expected wait_selector '.job-title', got %q", capturedSelector)
	}
	items := resp.Result.Collection["items"].([]any)
	first := items[0].(map[string]any)
	if first["title"] != "Go Systems Intern" {
		t.Fatalf("unexpected rendered item: %+v", first)
	}
	if len(resp.Result.SourceEvidence) != 1 || resp.Result.SourceEvidence[0].Receipt.Method != "RENDER" {
		t.Fatalf("expected RENDER SourceEvidence recorded, got %+v", resp.Result.SourceEvidence)
	}
}
