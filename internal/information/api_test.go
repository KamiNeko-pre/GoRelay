package information

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/knowledge"
)

// A host-controlled CONNECT proxy maps a verified public fixture address to
// local test HTTP. Production still runs PublicFetcher and every scope check.
func publicSource(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	source := httptest.NewServer(handler)
	t.Cleanup(source.Close)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != "8.8.8.8:80" {
			http.Error(w, "unexpected proxy peer", 403)
			return
		}
		destination, err := net.Dial("tcp", strings.TrimPrefix(source.URL, "http://"))
		if err != nil {
			http.Error(w, "source unavailable", 502)
			return
		}
		client, buffer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			destination.Close()
			return
		}
		buffer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		buffer.Flush()
		go func() { io.Copy(destination, buffer); destination.Close() }()
		io.Copy(client, destination)
		client.Close()
	}))
	t.Cleanup(proxy.Close)
	t.Setenv("GORELAY_ACQUISITION_PROXY", proxy.URL)
	return source
}

func TestAPIAssetTrialPublicationIndependentRefreshAndReuse(t *testing.T) {
	runAPIAssetLifecycle(t, false)
}

func TestRealAPIAssetLifecycleWithBGEAndQdrant(t *testing.T) {
	runAPIAssetLifecycle(t, true)
}

func runAPIAssetLifecycle(t *testing.T, realRetrieval bool) {
	s, vectors := retrievalService(t)
	s.Crawler.Fingerprint = "api-test-fingerprint"
	ctx := context.Background()
	var embed knowledge.Embedder = testEmbed{}
	var vectorStore knowledge.VectorStore = vectors
	if realRetrieval {
		embed, vectorStore = configureRealRetrieval(t, s)
	}
	var version atomic.Int32
	version.Store(1)
	var gets atomic.Int32
	publicSource(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gets.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.RequestURI() {
		case "/api/items":
			json.NewEncoder(w).Encode(map[string]any{"items": []any{map[string]any{"id": "A", "version": version.Load()}}, "links": map[string]any{"next": "/api/items?cursor=opaque-ending"}})
		case "/api/items?cursor=opaque-ending":
			json.NewEncoder(w).Encode(map[string]any{"items": []any{map[string]any{"id": "B", "version": version.Load()}}, "links": map[string]any{"next": nil}})
		default:
			http.NotFound(w, r)
		}
	}))
	spec := aiapp.TaskSpec{Goal: "collect current versions", ContractVersion: "v2", SchemaVersion: "v1", RequiredFields: []aiapp.FieldRequirement{{Name: "name", Type: "string"}, {Name: "version", Type: "integer"}, {Name: "source_url", Type: "string"}}, JoinKeys: []string{"name"}, MinRecords: 2, RequireFieldSupport: true, MaxAgeSeconds: 3600}
	in := Input{RequestKey: "create-api", Goal: spec.Goal, Spec: &spec, SourceScope: []string{"8.8.8.8"}, Refresh: true}
	req, err := s.Create(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	plan := APIPlan{URL: "http://8.8.8.8/api/items", ItemsPointer: "/items", Fields: map[string]string{"name": "/id", "version": "/version"}, NextPointer: "/links/next"}
	if _, err = s.TrialAPI(ctx, req.ID, "version feed", plan); err == nil || gets.Load() != 0 {
		t.Fatal("API ran before local lookup", err)
	}
	if _, err = s.Preflight(ctx, req.ID); err != nil {
		t.Fatal(err)
	}
	trial, err := s.TrialAPI(ctx, req.ID, "Public version API", plan)
	if err != nil || !trial.Coverage.Satisfied || len(trial.Records) != 2 {
		t.Fatal("real API trial failed", trial.Coverage, err)
	}
	var duplicate *ActionError
	if _, err = s.TrialAPI(ctx, req.ID, "different display label", plan); !errors.As(err, &duplicate) || duplicate.Code != "ACTION_ALREADY_CALLED" || gets.Load() != 2 {
		t.Fatal("same API parameters re-executed", err, gets.Load())
	}
	a, err := s.Publish(ctx, req.ID, trial.CandidateID)
	if err != nil {
		t.Fatal(err)
	}
	if a.ManifestJSON != string(mustJSON(plan)) || a.ProofJSON == "" {
		t.Fatal("trial serialization lost executable plan or host validation proof")
	}
	restarted := NewService(s.Crawler)
	restarted.Retriever = s.Retriever
	rows, err := restarted.ExecuteAPI(ctx, req.ID, a.ID)
	if err != nil || len(rows) != 2 || gets.Load() != 4 {
		t.Fatal("published API did not execute", err, gets.Load())
	}
	if rows[0].Provenance.FieldSupport["version"].Status != "supported" {
		t.Fatal("native API bypassed field source checking")
	}
	idx := knowledge.Indexer{Store: s.Crawler.Knowledge, Embed: embed, Vectors: vectorStore, Fingerprint: s.Crawler.Fingerprint}
	for n := 0; n < 3; n++ {
		if err := idx.Once(ctx); err != nil {
			t.Fatal(err)
		}
	}
	ledger, err := s.Accounting(ctx, req)
	if err != nil || ledger.ChildRunID != "" || len(ledger.ModelCalls) != 0 || len(ledger.NativeTaskIDs) != 1 || ledger.RunAccounting == nil || !ledger.RunAccounting.IndexSettled || len(ledger.RunAccounting.IndexWork) != 3 || len(ledger.RunAccounting.IndexAttempts) != 3 {
		t.Fatalf("native attribution omitted work or created a crawler: %+v %v", ledger, err)
	}
	if realRetrieval {
		for _, attempt := range ledger.RunAccounting.IndexAttempts {
			var metrics knowledge.IndexMetrics
			if err := json.Unmarshal([]byte(attempt.MetricsJSON), &metrics); err != nil || metrics.Usage == nil || metrics.Usage.TotalTokens == 0 {
				t.Fatalf("real native index token receipt missing: %+v %v", attempt, err)
			}
		}
	}
	t.Logf("native accounting: model calls=%d child runs=0 native tasks=%d index attempts=%d settled=%v", len(ledger.ModelCalls), len(ledger.NativeTaskIDs), len(ledger.RunAccounting.IndexAttempts), ledger.RunAccounting.IndexSettled)
	in.RequestKey = "fresh-records"
	in.Refresh = false
	fresh, err := s.Create(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	lookup, err := s.Preflight(ctx, fresh.ID)
	if err != nil || lookup.Next != "reuse_records" || gets.Load() != 4 {
		t.Fatal("fresh facts caused new acquisition", lookup.Next, err, gets.Load())
	}
	freshLedger, err := s.Accounting(ctx, fresh)
	if err != nil || len(freshLedger.RunAccounting.IndexWork) != 0 || len(freshLedger.RunAccounting.IndexAttempts) != 0 {
		t.Fatal("unchanged metadata was billed to the reusing request", err)
	}
	version.Store(2)
	in.RequestKey = "independent-refresh"
	in.Refresh = true
	next, err := s.Create(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	lookup, err = s.Preflight(ctx, next.ID)
	if err != nil || lookup.Next != "refresh_asset" || len(lookup.Assets) != 1 {
		t.Fatal("second request did not discover API asset", lookup, err)
	}
	refreshed, err := s.ExecuteAPI(ctx, next.ID, lookup.Assets[0].ID)
	if err != nil || len(refreshed) != 2 || gets.Load() != 6 {
		t.Fatal("independent API refresh failed", err, gets.Load())
	}
	if refreshed[0].Fields["version"].(json.Number).String() != "2" || refreshed[0].Provenance.DataVersion != a.Version || !refreshed[0].Provenance.FetchedAt.After(rows[0].Provenance.FetchedAt) {
		t.Fatal("refresh did not acquire new information using the same immutable version", refreshed)
	}
	if err := s.Revoke(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	in.RequestKey = "revoked-api"
	revoked, err := s.Create(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	lookup, err = s.Preflight(ctx, revoked.ID)
	if err != nil || len(lookup.Assets) != 0 {
		t.Fatal("revoked API asset still offered", lookup, err)
	}
	if _, err := s.ExecuteAPI(ctx, revoked.ID, a.ID); err == nil || gets.Load() != 6 {
		t.Fatal("revoked API executed", err, gets.Load())
	}
	var count int64
	s.DB.Model(&agentdata.Run{}).Count(&count)
	if count != 0 {
		t.Fatal("API asset execution entered crawler")
	}
}

func TestAPIPlanErrorsExposeCauseAndDoNotRepeatHTTP(t *testing.T) {
	s, _ := retrievalService(t)
	ctx := context.Background()
	var gets atomic.Int32
	publicSource(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gets.Add(1)
		io.WriteString(w, `{"rows":[{"key":"A"}],"next":"/api"}`)
	}))
	spec := aiapp.TaskSpec{Goal: "collect keys", SchemaVersion: "v1", RequiredFields: []aiapp.FieldRequirement{{Name: "name", Type: "string"}}}
	r, err := s.Create(ctx, Input{RequestKey: "bad-plan", Goal: spec.Goal, Spec: &spec, SourceScope: []string{"8.8.8.8"}})
	if err != nil {
		t.Fatal(err)
	}
	s.Preflight(ctx, r.ID)
	wrong := APIPlan{URL: "http://8.8.8.8/api", ItemsPointer: "/rows", Fields: map[string]string{"name": "/absent"}}
	_, err = s.TrialAPI(ctx, r.ID, "keys", wrong)
	var failure *ActionError
	if !errors.As(err, &failure) || !strings.Contains(failure.Message, "absent") || len(failure.Steps) == 0 {
		t.Fatal("field error lost exact cause/recovery", err)
	}
	_, err = s.TrialAPI(ctx, r.ID, "keys", wrong)
	if !errors.As(err, &failure) || failure.Code != "ACTION_ALREADY_CALLED" || gets.Load() != 1 {
		t.Fatal("repeat did not stop at business idempotency", err, gets.Load())
	}
	m := &routingReply{args: map[string]any{"action": "trial_api", "reason": "repeat the saved failed plan", "api": wrong}}
	s.Model = m
	if err := s.DB.Model(&Request{}).Where("id = ?", r.ID).Update("stage", "plan_api").Error; err != nil {
		t.Fatal(err)
	}
	if err := s.Advance(ctx, r.ID); !errors.As(err, &failure) || failure.Code != "ACTION_ALREADY_CALLED" {
		t.Fatalf("repeated trial lost duplicate cause: %v", err)
	}
	current, _ := s.Request(ctx, r.ID)
	if current.Stage != "plan_api" || !strings.Contains(current.FeedbackJSON, "absent") || gets.Load() != 1 {
		t.Fatalf("duplicate was treated as unsupported API: stage=%s gets=%d feedback=%s", current.Stage, gets.Load(), current.FeedbackJSON)
	}
	correct := wrong
	correct.Fields = map[string]string{"name": "/key"}
	out, err := s.TrialAPI(ctx, r.ID, "keys", correct)
	if err != nil || len(out.Records) != 1 || gets.Load() != 2 {
		t.Fatal("corrected real parameters were blocked", out, err)
	}
	m.args = map[string]any{"action": "trial_api", "reason": "restore a previously validated plan", "api": correct}
	if err := s.Advance(ctx, r.ID); err != nil {
		t.Fatalf("saved successful trial was not restored: %v", err)
	}
	current, _ = s.Request(ctx, r.ID)
	if current.Stage != "execute_asset" || gets.Load() != 2 {
		t.Fatalf("trial recovery repeated HTTP or lost publication: %s %d", current.Stage, gets.Load())
	}
	correct.NextPointer = "/next"
	_, err = s.TrialAPI(ctx, r.ID, "pagination cycle", correct)
	if err == nil || !strings.Contains(err.Error(), "revisited") || gets.Load() != 3 {
		t.Fatal("pagination cycle repeated physical GET", err, gets.Load())
	}
	correct.URL = "http://127.0.0.1/internal"
	_, err = s.TrialAPI(ctx, r.ID, "private", correct)
	if err == nil || gets.Load() != 3 {
		t.Fatal("scope/network permission widened", err)
	}
}
