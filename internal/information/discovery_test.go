package information

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/engine"
	"go-lua-crawler/internal/knowledge"
	"go-lua-crawler/internal/programrunner"
)

func TestDiscoverySourceCatalogKeepsVerificationAndCanonicalIdentity(t *testing.T) {
	s, _ := retrievalService(t)
	ctx := context.Background()
	for _, raw := range []string{
		"https://SOURCE.example/releases#latest",
		"//duckduckgo.com/l/?uddg=https%3A%2F%2Fother.example%2Fnews%23today",
	} {
		rows := []aiapp.SearchCandidate{{URL: raw, Title: "observed source"}}
		if err := s.rememberSources(ctx, "first", "query A", rows, false); err != nil {
			t.Fatal(err)
		}
		known, err := s.knownSources(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if known[len(known)-1].State != "candidate" {
			t.Fatal("search hit was promoted to a verified fact")
		}
		if err := s.rememberSources(ctx, "fetch", "", rows, true); err != nil {
			t.Fatal(err)
		}
		if err := s.rememberSources(ctx, "later", "query B", rows, false); err != nil {
			t.Fatal(err)
		}
	}
	known, err := s.knownSources(ctx, []string{"source.example"})
	if err != nil {
		t.Fatal(err)
	}
	if len(known) != 1 || known[0].URL != "https://source.example/releases" || known[0].State != "verified" || known[0].VerifiedAt == nil || known[0].Query != "query B" {
		t.Fatalf("identity, verification or scope lost: %+v", known)
	}
	allKnown, _ := s.knownSources(ctx, nil)
	foundDDG := false
	for _, src := range allKnown {
		if src.URL == "https://other.example/news" {
			foundDDG = true
		}
	}
	if !foundDDG {
		t.Fatalf("expected protocol-relative DuckDuckGo uddg redirect to unwrap cleanly: %+v", allKnown)
	}
	if err := s.rememberSources(ctx, "invalid", "q", []aiapp.SearchCandidate{{URL: "javascript:alert(1)", Title: "invalid"}}, false); err != nil {
		t.Fatal(err)
	}
	all, _ := s.knownSources(ctx, nil)
	if len(all) != 2 {
		t.Fatal("invalid URL admitted or query minted duplicate source")
	}
}

func TestDiscoveryHandsOffOnlySelectedPublicSnapshots(t *testing.T) {
	s, _ := retrievalService(t)
	ctx := context.Background()
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	input := Input{Goal: "Compare supported release trains", SourceScope: []string{"releases.example"}}
	parent, err := s.Create(ctx, input)
	check(err)
	check(s.DB.Model(&parent).Updates(map[string]any{"status": "running", "lease_token": "discovery-parent", "lease_until": time.Now().Add(time.Minute)}).Error)
	parent, err = s.Request(ctx, parent.ID)
	check(err)
	child, err := s.create(ctx, Input{RequestKey: "find-release-feeds", Goal: "Verify the release feeds"}, "source_discovery")
	check(err)
	step := Step{RequestID: child.ID, Kind: "acquire_sources", Key: "release-feeds", Stage: "succeeded"}
	check(s.DB.Create(&step).Error)
	var refs []StoredReference
	var urls []string
	for _, fixture := range []struct{ path, snapshotKey string }{{"/selected", "selected-stateless"}, {"/session", ""}, {"/unselected", "unselected-stateless"}} {
		u := "https://releases.example" + fixture.path
		doc, err := knowledge.NewDocument(knowledge.Input{Source: "information-response", SourceKey: fixture.path, URL: u, Title: "Release train", Body: "Release train A", Format: "text", FetchedAt: time.Now().UTC()})
		check(err)
		_, err = s.IngestAcquisition(ctx, child.ID, step.ID, []AcquiredResponse{{Key: fixture.path, Document: doc, HTTPStatus: 200, SnapshotKey: fixture.snapshotKey}}, nil)
		check(err)
		refs = append(refs, StoredReference{DocID: doc.ID, Revision: doc.Revision})
		urls = append(urls, u)
	}
	attach := Step{RequestID: parent.ID, Kind: "discover_sources", Key: "discovery-handoff", Stage: "reserved"}
	check(s.DB.Create(&attach).Error)
	h := &informationHost{service: s, request: parent, input: input, owner: requestExecutionOwner{RequestID: parent.ID, Token: parent.LeaseToken}}
	for range 2 {
		verified, err := h.attachDiscoverySources(ctx, attach, child.ID, DeliveryDraft{References: refs, DiscoveredSources: urls[:2]})
		check(err)
		if len(verified) != 2 || verified[0].SourceRef == nil || verified[0].SourceRef.DocID != refs[0].DocID || verified[1].SourceRef != nil {
			t.Fatalf("wrong snapshot reuse: %+v", verified)
		}
	}
	snapshots, err := s.taskAcquisitionSnapshots(ctx, parent.ID, input.SourceScope)
	check(err)
	if len(snapshots.responses) != 1 || snapshots.responses["selected-stateless"].Body != "Release train A" {
		t.Fatal("parent did not receive exactly one verified public capture")
	}
	s.LuaRunner = func(ctx context.Context, request programrunner.Request) (engine.ProgramResult, error) {
		out, err := programrunner.Run(ctx, request)
		if err != nil {
			return engine.ProgramResult{}, err
		}
		return *out.Result, nil
	}
	args := luaRunArgs{Mode: "probe", SourceRef: &luaSourceRef{DocID: refs[0].DocID, Revision: refs[0].Revision}, Code: `return true, { verified = SOURCE_RESPONSE.body }`}
	result, err := h.invoke(ctx, "read-verified-reused", "run_lua", string(mustMarshal(args)))
	check(err)
	var receipt informationReceipt
	check(json.Unmarshal([]byte(result), &receipt))
	if receipt.Status != "succeeded" || !strings.Contains(result, "Release train A") {
		t.Fatalf("Lua could not read the source_ref returned by discovery: %s", result)
	}
}

func TestDiscoveryTerminalDependenciesCannotWaitForAnotherTransition(t *testing.T) {
	s, _ := retrievalService(t)
	ctx := context.Background()
	for _, status := range []string{"failed", "completed", "running"} {
		child, err := s.Create(ctx, Input{RequestKey: "terminal-dependency-" + status, Goal: "Read source facts"})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.DB.Model(&Request{}).Where("id = ?", child.ID).Update("status", status).Error; err != nil {
			t.Fatal(err)
		}
		dep, err := s.informationDependency(ctx, "information", child.ID)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(informationWait{Dependencies: []informationDependency{dep}})
		ready, err := s.informationReady(ctx, Request{Status: "waiting_dependency", WaitJSON: string(raw)})
		if err != nil || ready != (status != "running") {
			t.Fatalf("terminal dependency %s ready=%v error=%v", status, ready, err)
		}
	}
}
