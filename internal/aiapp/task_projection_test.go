package aiapp

import (
	"context"
	"encoding/json"
	"fmt"
	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/knowledge"
	"go-lua-crawler/internal/testdb"
	"testing"
	"time"
)

type projectionEmbedFixture struct{ Calls int }

func (e *projectionEmbedFixture) EmbedTexts(_ context.Context, texts []string) ([][]float32, error) {
	e.Calls++
	out := make([][]float32, len(texts))
	for n := range out {
		out[n] = []float32{1, 0}
	}
	return out, nil
}

type projectionVectorsFixture struct{}

func (projectionVectorsFixture) Upsert(context.Context, []knowledge.VectorPoint) error { return nil }
func (projectionVectorsFixture) Search(context.Context, []float32, int) ([]knowledge.VectorHit, error) {
	return nil, nil
}

func TestTaskFactChangeAndMetadataRefreshUseSameKnowledgeProjection(t *testing.T) {
	t.Setenv("GORELAY_ACTION_MODE", "per_tool")
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	s.Fingerprint = "fixture"
	ctx := context.Background()
	spec := TaskSpec{Goal: "compare Alpha and Beta releases", SchemaVersion: "v1", RequiredFields: []FieldRequirement{{Name: "name", Type: "string"}, {Name: "version", Type: "string"}}, JoinKeys: []string{"name"}, Entities: []string{"Alpha", "Beta"}, EntityField: "name"}
	var r agentdata.Run
	var err error
	embed := &projectionEmbedFixture{}
	indexer := knowledge.Indexer{Store: s.Knowledge, Embed: embed, Vectors: projectionVectorsFixture{}, Fingerprint: s.Fingerprint}
	for stage, version := range []string{"1.0", "2.0", "2.0"} {
		r, err = s.Create(ctx, CreateRunRequest{RequestKey: fmt.Sprintf("task-projection-%d", stage), Goal: spec.Goal, Spec: &spec})
		if err != nil {
			t.Fatal(err)
		}
		r, err = s.Data.Claim(ctx, r.ID, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		page := fmt.Sprintf("page%d", stage)
		url := "https://fixture.example/releases"
		if stage == 2 {
			url += "?edition=new"
		}
		observation, _ := toolData(ToolCall{ID: page, Function: FunctionCall{Name: "inspect_source"}}, sourceInspection{Source: acquisition.Response{Receipt: acquisition.Receipt{Method: "GET", URL: url, Status: 200, FetchedAt: time.Now()}, Body: "controlled task response"}})
		raw, _ := json.Marshal(observation)
		if err = s.Data.DB.Create(&agentdata.Operation{RunID: r.ID, Key: "tool:" + page, Kind: "inspect_source", Stage: "succeeded", ResultJSON: string(raw)}).Error; err != nil {
			t.Fatal(err)
		}
		args, _ := json.Marshal(map[string]any{"inspection_call_id": page, "items": []any{map[string]any{"name": "Alpha", "version": version, "source_url": url}, map[string]any{"name": "Beta", "version": "1.0", "source_url": url}}})
		if _, err = s.executeDurableTool(ctx, r, ToolCall{ID: fmt.Sprintf("extract%d", stage), Type: "function", Function: FunctionCall{Name: "submit_extraction", Arguments: string(args)}}); err != nil {
			t.Fatal(err)
		}
		for n := 0; n < 2; n++ {
			if err = indexer.Once(ctx); err != nil {
				t.Fatal(err)
			}
		}
		want := 2
		if stage > 0 {
			want = 3
		}
		if embed.Calls != want {
			t.Fatalf("stage %d re-embedded unaffected task facts: calls=%d want=%d", stage, embed.Calls, want)
		}
	}
	heads, err := s.Knowledge.Heads(ctx)
	if err != nil || len(heads) != 2 {
		t.Fatalf("business identities multiplied across tasks: %+v %v", heads, err)
	}
	for _, head := range heads {
		if head.Generation == "" || head.URL != "https://fixture.example/releases?edition=new" {
			t.Fatal("metadata refresh failed to publish current evidence")
		}
	}
	result, err := s.Evaluate(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Coverage.Satisfied || len(result.Conflicts) != 0 {
		t.Fatal("later goal did not receive its current complete facts")
	}
}
