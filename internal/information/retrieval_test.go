package information

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/knowledge"
	"go-lua-crawler/internal/repository"
	"go-lua-crawler/internal/testdb"
)

// These are transport substitutes for the retrieval component, not an oracle
// for model actions. The real indexer, SQL publication and retriever still run.
type testVectors struct {
	points  []knowledge.VectorPoint
	queries int
	fail    bool
}

func (v *testVectors) Upsert(_ context.Context, p []knowledge.VectorPoint) error {
	v.points = append(v.points, p...)
	return nil
}
func (v *testVectors) Search(_ context.Context, _ []float32, n int) ([]knowledge.VectorHit, error) {
	v.queries++
	if v.fail {
		return nil, fmt.Errorf("vector service unavailable")
	}
	out := []knowledge.VectorHit{}
	for _, p := range v.points {
		out = append(out, knowledge.VectorHit{ID: p.ID, Score: 1})
		if len(out) == n {
			break
		}
	}
	return out, nil
}

type testEmbed struct{}

func (testEmbed) EmbedTexts(_ context.Context, texts []string) ([][]float32, error) {
	rows := make([][]float32, len(texts))
	for i := range rows {
		rows[i] = []float32{1, 0}
	}
	return rows, nil
}

func retrievalService(t *testing.T) (*Service, *testVectors) {
	t.Helper()
	crawler := aiapp.NewService(testdb.Open(t))
	if err := crawler.Migrate(); err != nil {
		t.Fatal(err)
	}
	s := NewService(crawler)
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	v := &testVectors{}
	s.Retriever = &knowledge.Retriever{Store: crawler.Knowledge, Embed: testEmbed{}, Vectors: v}
	return s, v
}

func TestPreflightRetrievesBeforeAcquisitionAndPreservesSnapshotTime(t *testing.T) {
	for _, tc := range []struct {
		name    string
		age     time.Duration
		refresh bool
		want    string
	}{
		{"fresh", -time.Minute, false, "reuse_records"}, {"stale", -2 * time.Hour, false, "refresh_asset"}, {"explicit-refresh", -time.Minute, true, "refresh_asset"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, v := retrievalService(t)
			ctx := context.Background()
			at := time.Now().UTC().Add(tc.age)
			spec := aiapp.TaskSpec{Goal: "collect current observed version", ContractVersion: "v2", SchemaVersion: "v1", RequiredFields: []aiapp.FieldRequirement{{Name: "name", Type: "string"}, {Name: "version", Type: "string"}}, JoinKeys: []string{"name"}, MaxAgeSeconds: 3600}
			artifact, err := s.Crawler.Data.SaveArtifact(ctx, agentdata.ProgramArtifact{Code: "actual source program", Kind: "adapter", OutputSchema: string(mustJSON(spec)), ScopeJSON: `["source.example"]`})
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.Crawler.Data.Publish(ctx, agentdata.SourceVersion{SourceID: "version-api", ArtifactHash: artifact.Hash, Description: "Observed version of a public service"}, "", 0)
			if err != nil {
				t.Fatal(err)
			}
			row := aiapp.Record{Key: "record-a", SchemaVersion: "v1", Fields: map[string]any{"name": "observed", "version": "2026.10"}, Provenance: aiapp.Provenance{SourceID: "version-api", DataVersion: artifact.Hash, URL: "https://source.example/version", FetchedAt: at, TaskID: 77}}
			payload := string(mustJSON(aiapp.AcquisitionResult{Records: []aiapp.Record{row}}))
			if err := s.DB.Create(&repository.CrawlResult{TaskID: 77, Payload: payload}).Error; err != nil {
				t.Fatal(err)
			}
			// MySQL canonicalizes JSON storage on insertion; compare the saved
			// value before/after, not its original serialization whitespace.
			var canonical repository.CrawlResult
			if err := s.DB.First(&canonical, "task_id = ?", 77).Error; err != nil {
				t.Fatal(err)
			}
			doc, err := knowledge.NewDocument(knowledge.Input{Source: row.Provenance.SourceID, SourceKey: "observed-version", URL: row.Provenance.URL, Title: "observed version", Body: `name: observed; version: 2026.10`, Format: "text", FetchedAt: at, DataVersion: artifact.Hash, TaskID: 77, RecordKey: row.Key})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.Crawler.Knowledge.Put(ctx, doc, "test-fingerprint"); err != nil {
				t.Fatal(err)
			}
			idx := knowledge.Indexer{Store: s.Crawler.Knowledge, Embed: testEmbed{}, Vectors: v, Fingerprint: "test-fingerprint"}
			if err = idx.Once(ctx); err != nil {
				t.Fatal(err)
			}
			req, err := s.Create(ctx, Input{RequestKey: tc.name, Goal: spec.Goal, Spec: &spec, SourceScope: []string{"source.example"}, Refresh: tc.refresh})
			if err != nil {
				t.Fatal(err)
			}
			out, err := s.Preflight(ctx, req.ID)
			if err != nil {
				t.Fatal(err)
			}
			if out.Next != tc.want || v.queries != 1 || len(out.Assets) != 1 {
				t.Fatalf("incorrect local-first route: %+v queries=%d", out, v.queries)
			}
			if len(out.Records) != 1 || !out.Records[0].Provenance.FetchedAt.Equal(at) {
				t.Fatal("reuse lost or refreshed the original snapshot", out.Records)
			}
			if out.Next == "reuse_records" && !out.Coverage.Satisfied {
				t.Fatal("fresh record did not cover contract")
			}
			again, err := s.Preflight(ctx, req.ID)
			if err != nil || again.Next != out.Next || v.queries != 1 {
				t.Fatal("same preflight repeated physical embedding/vector work", err, v.queries)
			}
			var saved repository.CrawlResult
			s.DB.First(&saved, "task_id = ?", 77)
			if saved.Payload != canonical.Payload {
				t.Fatal("preflight mutated canonical record")
			}
			var childCount int64
			s.DB.Model(&agentdata.Run{}).Count(&childCount)
			if childCount != 0 {
				t.Fatal("local preflight entered crawler")
			}
		})
	}
}

func TestPreflightDistinguishesRetrievalOutageAndRevokedOrForeignAssets(t *testing.T) {
	s, v := retrievalService(t)
	ctx := context.Background()
	v.fail = true
	spec := aiapp.TaskSpec{Goal: "collect data", SchemaVersion: "v1", RequiredFields: []aiapp.FieldRequirement{{Name: "name", Type: "string"}}}
	for _, host := range []string{"source.example", "outside.example"} {
		a, _ := s.Crawler.Data.SaveArtifact(ctx, agentdata.ProgramArtifact{Code: host, Kind: "adapter", OutputSchema: string(mustJSON(spec)), ScopeJSON: string(mustJSON([]string{host}))})
		if _, err := s.Crawler.Data.Publish(ctx, agentdata.SourceVersion{SourceID: host, ArtifactHash: a.Hash}, "", 0); err != nil {
			t.Fatal(err)
		}
	}
	r, err := s.Create(ctx, Input{RequestKey: "outage", Goal: spec.Goal, Spec: &spec, SourceScope: []string{"source.example"}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.Preflight(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if out.RetrievalStatus != "unavailable" || !strings.Contains(out.RetrievalError, "vector service") || len(out.Assets) != 1 || out.Assets[0].Kind != "lua" {
		t.Fatal("outage became an empty miss or scope broadened", out)
	}
	head, _ := s.Crawler.Data.SourceHead(ctx, "source.example")
	if err := s.Crawler.QuarantineSource(ctx, "source.example", head.ArtifactHash, "withdrawn"); err != nil {
		t.Fatal(err)
	}
	r, err = s.Create(ctx, Input{RequestKey: "revoked", Goal: spec.Goal, Spec: &spec, SourceScope: []string{"source.example"}})
	if err != nil {
		t.Fatal(err)
	}
	out, err = s.Preflight(ctx, r.ID)
	if err != nil || len(out.Assets) != 0 {
		t.Fatal("revoked asset remains reusable", out, err)
	}
}

func TestInformationInputIdempotencyAndCrawlerSeparation(t *testing.T) {
	s, _ := retrievalService(t)
	ctx := context.Background()
	in := Input{RequestKey: "input-key", Goal: "get public data", SourceScope: []string{"source.example"}}
	a, err := s.Create(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Create(ctx, in)
	if err != nil || a.ID != b.ID {
		t.Fatal("same request did not restore", err)
	}
	in.Refresh = true
	if _, err = s.Create(ctx, in); err == nil {
		t.Fatal("request key changed original refresh contract")
	}
	var count int64
	s.DB.Model(&agentdata.Run{}).Count(&count)
	if count != 0 {
		t.Fatal("outer request changed crawler state")
	}
}

func mustJSON(v any) []byte {
	b, e := json.Marshal(v)
	if e != nil {
		panic(e)
	}
	return b
}
