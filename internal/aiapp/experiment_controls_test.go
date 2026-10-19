package aiapp

import (
	"context"
	"encoding/json"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/testdb"
	"testing"
	"time"
)

func TestExperimentalReuseControlsAreFrozenAndHostEnforced(t *testing.T) {
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	a, err := s.Data.SaveArtifact(ctx, agentdata.ProgramArtifact{Code: "return true,{items={}}", Kind: "adapter", PolicyVersion: "public-read-v1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Data.Publish(ctx, agentdata.SourceVersion{SourceID: "fixture", ArtifactHash: a.Hash}, "", 0); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GORELAY_DISABLE_CAPABILITY_REUSE", "true")
	t.Setenv("GORELAY_DISABLE_KNOWLEDGE_REUSE", "true")
	r, err := s.Create(ctx, CreateRunRequest{RequestKey: "off", Goal: "goal"})
	if err != nil {
		t.Fatal(err)
	}
	r, err = s.Data.Claim(ctx, r.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, def := range runToolDefinitions(r) {
		if def.Function.Name == "search_knowledge" || def.Function.Name == "reuse_knowledge" {
			t.Fatal("knowledge baseline still has cache access")
		}
	}
	call := ToolCall{ID: "catalog", Type: "function", Function: FunctionCall{Name: "list_sources", Arguments: `{}`}}
	out, err := s.executeDurableTool(ctx, r, call)
	if err != nil {
		t.Fatal(err)
	}
	var catalog struct {
		Active []json.RawMessage `json:"active"`
	}
	json.Unmarshal(out.Data, &catalog)
	if len(catalog.Active) != 0 {
		t.Fatal("disabled capability leaked through catalog")
	}
	call.ID = "inspect"
	call.Function = FunctionCall{Name: "inspect_source", Arguments: `{"source_id":"fixture"}`}
	if _, err = s.executeDurableTool(ctx, r, call); err == nil {
		t.Fatal("old capability still reusable through direct tool call")
	}
	t.Setenv("GORELAY_DISABLE_CAPABILITY_REUSE", "false")
	t.Setenv("GORELAY_DISABLE_KNOWLEDGE_REUSE", "false")
	if !experimentControls(r).DisableKnowledgeReuse {
		t.Fatal("configuration reset the frozen comparison")
	}
	normal, err := s.Create(ctx, CreateRunRequest{RequestKey: "normal", Goal: "goal"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, def := range runToolDefinitions(normal) {
		if def.Function.Name == "reuse_knowledge" {
			found = true
		}
	}
	if !found {
		t.Fatal("normal mainline lost knowledge reuse")
	}
}
