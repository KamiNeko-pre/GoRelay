package aiapp

import (
	"context"
	"encoding/json"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/testdb"
	"testing"
)

func TestCapabilityCatalogShowsAdaptableAssetsWithoutWeakeningExecution(t *testing.T) {
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	spec := TaskSpec{Goal: "collect titles", SchemaVersion: "v1", RequiredFields: []FieldRequirement{{Name: "title", Type: "string"}}}
	r, err := s.Create(ctx, CreateRunRequest{RequestKey: "catalog", Goal: spec.Goal, Spec: &spec, SourceScope: []string{"example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"title", "name"} {
		output := spec
		output.RequiredFields = []FieldRequirement{{Name: field, Type: "string"}}
		raw, _ := json.Marshal(output)
		a, err := s.Data.SaveArtifact(ctx, agentdata.ProgramArtifact{Code: field + " program", Kind: "adapter", OutputSchema: string(raw), ScopeJSON: `["example.com"]`})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Data.Publish(ctx, agentdata.SourceVersion{SourceID: field, ArtifactHash: a.Hash}, "", 0); err != nil {
			t.Fatal(err)
		}
		if field == "name" && checkCapabilityForRun(r, a) == nil {
			t.Fatal("incompatible output must remain rejected for direct execution")
		}
	}
	out, err := s.dispatchTool(ctx, r, ToolCall{ID: "catalog", Function: FunctionCall{Name: "list_sources", Arguments: `{}`}})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Active []SourceCapability `json:"active"`
	}
	if err := json.Unmarshal(out.Data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Active) != 2 {
		t.Fatal("useful existing code hidden merely because current output names differ", string(out.Data))
	}
}
