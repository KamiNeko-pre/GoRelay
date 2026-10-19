package information

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/aiapp"
)

func TestRealBrowserAssetRendersPublishesAndReuses(t *testing.T) {
	binary := os.Getenv("GORELAY_TEST_BROWSER_BINARY")
	if binary == "" {
		t.Skip("explicit real browser executable required")
	}
	s, _ := retrievalService(t)
	script, err := filepath.Abs("../../scripts/information-browser.cjs")
	if err != nil {
		t.Fatal(err)
	}
	s.Browser = &BrowserRunner{NodeBinary: os.Getenv("GORELAY_TEST_BROWSER_NODE"), ScriptPath: script, ModulePath: os.Getenv("GORELAY_TEST_PLAYWRIGHT_MODULE"), ExecutablePath: binary}
	ctx := context.Background()
	var generation atomic.Int32
	generation.Store(1)
	var browserGets atomic.Int32
	publicSource(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("User-Agent"), "Chrome") {
			http.Error(w, "browser rendering required", 403)
			return
		}
		browserGets.Add(1)
		if r.URL.Path == "/data" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"name": "dynamic-" + r.URL.Query().Get("page"), "version": generation.Load()})
			return
		}
		w.Header().Set("Content-Type", "text/html")
		page := r.URL.Query().Get("page")
		if page == "" {
			page = "1"
		}
		io.WriteString(w, `<!doctype html><main id="results"></main><script>fetch('/data?page=`+page+`').then(r=>r.json()).then(v=>{const item=document.createElement('article');item.className='entry';const name=document.createElement('h2');name.textContent=v.name;const version=document.createElement('code');version.textContent=v.version;const link=document.createElement('a');link.setAttribute('href','../export.csv?edition='+v.version+'&lang=zh');link.textContent='CSV';item.append(name,version,link);document.querySelector('main').append(item);});</script>`)
		if page == "1" {
			io.WriteString(w, `<a rel="next" href="/dynamic?page=2">Next</a>`)
		}
	}))
	spec := aiapp.TaskSpec{Goal: "collect dynamically rendered versions", ContractVersion: "v2", SchemaVersion: "v1", RequiredFields: []aiapp.FieldRequirement{{Name: "name", Type: "string"}, {Name: "version", Type: "string"}, {Name: "resource", Type: "string"}, {Name: "source_url", Type: "string"}}, JoinKeys: []string{"name"}, MinRecords: 2, RequireFieldSupport: true}
	in := Input{RequestKey: "browser-original", Goal: spec.Goal, Spec: &spec, SourceScope: []string{"8.8.8.8"}, Refresh: true}
	r, err := s.Create(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Preflight(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TrialAPI(ctx, r.ID, "ordinary HTTP", APIPlan{URL: "http://8.8.8.8/dynamic", Fields: map[string]string{"name": "/name"}}); err == nil {
		t.Fatal("fixture must fail without a real browser")
	}
	plan := BrowserPlan{URL: "http://8.8.8.8/dynamic", RowsSelector: "article.entry", WaitSelector: "article.entry", NextSelector: `a[rel="next"]`, Fields: map[string]BrowserField{"name": {Selector: "h2"}, "version": {Selector: "code"}, "resource": {Selector: "a", Attribute: "href"}}}
	// Component fixture: exercise the real browser/asset boundary after host
	// approval. Full Lua correction and model approval are separate acceptance.
	if err := s.storeBrowserGrant(ctx, r, plan.URL, "host-approved browser capability component fixture", "", ""); err != nil {
		t.Fatal(err)
	}
	trial, err := s.TrialBrowser(ctx, r.ID, "Dynamic public version feed", plan)
	if err != nil || !trial.Coverage.Satisfied {
		t.Fatal("real browser trial failed", trial.Coverage, err)
	}
	asset, err := s.Publish(ctx, r.ID, trial.CandidateID)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.ExecuteAsset(ctx, r.ID, asset.ID)
	if err != nil || len(rows) != 2 {
		t.Fatal("browser publication did not execute", err)
	}
	if rows[0].Fields["resource"] != "../export.csv?edition=1&lang=zh" || rows[0].Provenance.FieldSupport["resource"].Status != "supported" {
		t.Fatal("rendered original attribute/evidence was lost", rows[0])
	}
	generation.Store(2)
	in.RequestKey = "browser-independent-refresh"
	r2, err := s.Create(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	preflight, err := s.Preflight(ctx, r2.ID)
	if err != nil || len(preflight.Assets) != 1 || preflight.Assets[0].ID != asset.ID {
		t.Fatal("browser asset not discoverable across requests", preflight, err)
	}
	restarted := NewService(s.Crawler)
	restarted.Browser = s.Browser
	newRows, err := restarted.ExecuteAsset(ctx, r2.ID, preflight.Assets[0].ID)
	if err != nil || len(newRows) != 2 || newRows[0].Fields["version"] != "2" || newRows[0].Provenance.DataVersion != asset.Version {
		t.Fatal("browser did not reuse the same asset version for new data", newRows, err)
	}
	var runs int64
	s.DB.Model(&agentdata.Run{}).Count(&runs)
	if runs != 0 || browserGets.Load() < 12 {
		t.Fatal("browser success did not use the isolated real path", runs, browserGets.Load())
	}
}
