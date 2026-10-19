package aiapp

import (
	"context"
	"encoding/json"
	"fmt"
	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/knowledge"
	"go-lua-crawler/internal/testdb"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Controlled business changes, real SQL/BGE/Qdrant, no model substitute and no
// paid model calls. This isolates projection cost/correctness from Agent quality.
func TestRealTaskProjectionCostAndCurrentEvidence(t *testing.T) {
	if os.Getenv("GORELAY_REAL_PROJECTION") != "1" {
		t.Skip("explicit local embedding/Qdrant experiment required")
	}
	for _, force := range []bool{true, false} {
		variant := "reuse"
		if force {
			variant = "force_rebuild"
		}
		t.Run(variant, func(t *testing.T) {
			t.Setenv("GORELAY_ACTION_MODE", "per_tool")
			collection := "gorelay_projection_" + agentdata.Token()
			t.Setenv("GORELAY_QDRANT_COLLECTION", collection)
			s := NewService(testdb.Open(t))
			if err := s.Migrate(); err != nil {
				t.Fatal(err)
			}
			s.Configure()
			if s.Retriever == nil {
				t.Fatal("real embedding/Qdrant configuration required")
			}
			ctx := context.Background()
			q := s.Retriever.Vectors.(*knowledge.Qdrant)
			if err := q.EnsureCollection(ctx, 512); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				req, _ := http.NewRequest("DELETE", os.Getenv("GORELAY_QDRANT_URL")+"/collections/"+collection, nil)
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Error(err)
				} else {
					resp.Body.Close()
					if resp.StatusCode != 200 {
						t.Error(resp.Status)
					}
				}
			})
			spec := TaskSpec{Goal: "compare dependency releases", SchemaVersion: "v1", RequiredFields: []FieldRequirement{{"name", "string"}, {"version", "string"}}, JoinKeys: []string{"name"}, Entities: []string{"Alpha", "Beta"}, EntityField: "name"}
			chunkSize, chunkOverlap, err := ChunkingSettingsFromEnv()
			if err != nil {
				t.Fatal(err)
			}
			indexer := knowledge.Indexer{Store: s.Knowledge, Embed: s.Retriever.Embed, Vectors: q, Fingerprint: s.Fingerprint, Planner: knowledge.ExperimentalPlanner(knowledge.BuildIndexPlan, force), ChunkSize: chunkSize, ChunkOverlap: chunkOverlap}
			states := []any{}
			var lastSet knowledge.EvidenceSet
			for stage, version := range []string{"1.0", "1.0", "2.0", "2.0"} {
				r, err := s.Create(ctx, CreateRunRequest{RequestKey: fmt.Sprintf("projection-%d", stage), Goal: spec.Goal, Spec: &spec})
				if err != nil {
					t.Fatal(err)
				}
				r, err = s.Data.Claim(ctx, r.ID, time.Minute)
				if err != nil {
					t.Fatal(err)
				}
				url := "https://controlled.example/releases"
				if stage == 3 {
					url += "?edition=new"
				}
				page := fmt.Sprintf("page-%d", stage)
				observation, _ := toolData(ToolCall{ID: page, Function: FunctionCall{Name: "inspect_source"}}, sourceInspection{Source: acquisition.Response{Receipt: acquisition.Receipt{Method: "GET", URL: url, Status: 200, FetchedAt: time.Now()}, Body: fmt.Sprintf(`{"rows":[{"name":"Alpha","version":%q},{"name":"Beta","version":"1.0"}]}`, version)}})
				raw, _ := json.Marshal(observation)
				if err = s.Data.DB.Create(&agentdata.Operation{RunID: r.ID, Key: "tool:" + page, Kind: "inspect_source", Stage: "succeeded", ResultJSON: string(raw)}).Error; err != nil {
					t.Fatal(err)
				}
				args, _ := json.Marshal(map[string]any{"inspection_call_id": page, "items": []any{map[string]any{"name": "Alpha", "version": version, "source_url": url}, map[string]any{"name": "Beta", "version": "1.0", "source_url": url}}})
				if _, err = s.executeDurableTool(ctx, r, ToolCall{ID: fmt.Sprintf("extract-%d", stage), Type: "function", Function: FunctionCall{Name: "submit_extraction", Arguments: string(args)}}); err != nil {
					t.Fatal(err)
				}
				for n := 0; n < 2; n++ {
					if err = indexer.Once(ctx); err != nil {
						t.Fatal(err)
					}
				}
				result, err := s.Evaluate(ctx, r)
				if err != nil || !result.Coverage.Satisfied || len(result.Records) != 2 {
					t.Fatalf("incorrect current goal: %+v %v", result, err)
				}
				lastSet, err = s.Retriever.Retrieve(ctx, "Alpha Beta dependency release version", 8)
				if err != nil || len(lastSet.Entries) != 2 {
					t.Fatalf("current evidence unavailable: %+v %v", lastSet, err)
				}
				for _, ev := range lastSet.Entries {
					var fields map[string]any
					if err = json.Unmarshal([]byte(ev.Text), &fields); err != nil || ev.URL != url {
						t.Fatal("wrong current provenance/body", err)
					}
					want := "1.0"
					if fields["name"] == "Alpha" {
						want = version
					}
					if fields["version"] != want {
						t.Fatal("old field version escaped current-state gate")
					}
				}
				states = append(states, map[string]any{"stage": stage, "result": result, "retrieval": lastSet})
			}
			attempts, err := s.Knowledge.IndexAttempts(ctx, "")
			if err != nil || len(attempts) != 8 {
				t.Fatal("index attempt count", len(attempts), err)
			}
			calls, tokens := 0, int64(0)
			for _, attempt := range attempts {
				var metrics knowledge.IndexMetrics
				if err = json.Unmarshal([]byte(attempt.MetricsJSON), &metrics); err != nil || attempt.Status != "succeeded" {
					t.Fatal("failed/unknown index attempt", attempt, err)
				}
				calls += metrics.EmbeddingCalls
				if metrics.Usage != nil {
					tokens += metrics.Usage.TotalTokens
				}
			}
			wantCalls := 3 // initial 2 + changed Alpha 1; refresh only republishes provenance.
			if force {
				wantCalls = 8
			}
			if calls != wantCalls || tokens <= 0 {
				t.Fatalf("embedding calls=%d want=%d tokens=%d", calls, wantCalls, tokens)
			}
			// Old vectors remain physically present; invalidation is enforced by SQL.
			heads, _ := s.Knowledge.Heads(ctx)
			for _, head := range heads {
				if err = s.Knowledge.Invalidate(ctx, head.ID); err != nil {
					t.Fatal(err)
				}
			}
			for _, ev := range lastSet.Entries {
				if _, err = (knowledge.SetReader{Store: s.Knowledge, Set: lastSet}).ReadEvidence(ctx, ev.ID); err == nil {
					t.Fatal("old evidence remained readable")
				}
			}
			inactive, err := s.Retriever.Retrieve(ctx, "Alpha Beta dependency release version", 8)
			if err != nil || len(inactive.Entries) != 0 {
				t.Fatal("inactive evidence escaped SQL shield", err)
			}
			query, err := s.Retriever.Embed.EmbedTexts(ctx, []string{"Alpha Beta dependency release version"})
			if err != nil {
				t.Fatal(err)
			}
			hits, err := q.Search(ctx, query[0], 24)
			if err != nil || len(hits) < 2 {
				t.Fatal("no actual residual vectors to challenge shield", err)
			}
			if dir := os.Getenv("GORELAY_TEST_EVIDENCE_DIR"); dir != "" {
				raw, _ := json.MarshalIndent(map[string]any{"mode": "controlled_business_changes_real_mysql_bge_qdrant_no_model", "variant": variant, "index_embedding_calls": calls, "index_embedding_tokens": tokens, "attempts": attempts, "states": states, "invalidated_retrieval": inactive, "residual_vector_hits": len(hits)}, "", "  ")
				if err = os.WriteFile(filepath.Join(dir, "projection-"+variant+".json"), raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			t.Logf("%s: 4 correct states, index calls=%d tokens=%d; invalidated evidence=0, residual vectors=%d", variant, calls, tokens, len(hits))
		})
	}
}
