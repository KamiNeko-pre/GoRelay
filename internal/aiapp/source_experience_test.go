package aiapp

import (
	"context"
	"encoding/json"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/task"
	"go-lua-crawler/internal/testdb"
	"testing"
)

func TestSourceExperienceRequiresExecutedVersionAndScope(t *testing.T) {
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := s.Data.DB.AutoMigrate(&task.CrawlTask{}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	spec := TaskSpec{Goal: "release", SchemaVersion: "v1", RequiredFields: []FieldRequirement{{Name: "name", Type: "string"}}}
	schema, _ := json.Marshal(spec)
	for _, id := range []string{"alpha", "beta"} {
		a, err := s.Data.SaveArtifact(ctx, agentdata.ProgramArtifact{Code: "return true,{items={}} --" + id, Kind: "adapter", OutputSchema: string(schema), ScopeJSON: `["example.com"]`})
		if err != nil {
			t.Fatal(err)
		}
		prior, err := s.Create(ctx, CreateRunRequest{RequestKey: id, Goal: spec.Goal, Spec: &spec, SourceScope: []string{"example.com"}})
		if err != nil {
			t.Fatal(err)
		}
		if err = s.Data.DB.Create(&agentdata.SourceHead{SourceID: id, ArtifactHash: a.Hash, Status: "active"}).Error; err != nil {
			t.Fatal(err)
		}
		args, _ := json.Marshal(map[string]any{"source_id": id, "url": "https://example.com/list", "purpose": "release"})
		response, _ := json.Marshal(ChatResponse{Message: Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "bad", Function: FunctionCall{Name: "validate_program", Arguments: string(args)}}}}})
		failed, _ := json.Marshal(ToolResult{CallID: "bad", Name: "validate_program", Error: "Lua syntax error"})
		s.Data.DB.Create(&agentdata.Operation{RunID: prior.ID, Key: "model:first", Kind: "model", Stage: "succeeded", ResultJSON: string(response)})
		s.Data.DB.Create(&agentdata.Operation{RunID: prior.ID, Key: "tool:bad", Kind: "validate_program", Stage: "failed", ResultJSON: string(failed)})
		s.Data.DB.Create(&Candidate{ID: id, RunID: prior.ID, SourceID: id, ArtifactHash: a.Hash})
		current, err := s.Create(ctx, CreateRunRequest{RequestKey: id + "-next", Goal: spec.Goal, Spec: &spec, SourceScope: []string{"example.com"}})
		if err != nil {
			t.Fatal(err)
		}
		entries, err := s.sourceExperience(ctx, current, id)
		if err != nil || len(entries) != 0 {
			t.Fatal("trial alone promoted to executed experience", entries, err)
		}
		child := task.CrawlTask{RequestKey: id + "-child", Status: task.StatusSucceeded}
		if err = s.Data.DB.Create(&child).Error; err != nil {
			t.Fatal(err)
		}
		if err := s.Data.DB.Create(&agentdata.RunTask{RunID: prior.ID, TaskID: child.ID, OperationID: child.ID, SourceID: id, ArtifactHash: a.Hash}).Error; err != nil {
			t.Fatal(err)
		}
		entries, err = s.sourceExperience(ctx, current, id)
		if err != nil || len(entries) != 1 || len(entries[0].Failures) != 1 {
			t.Fatal("cross-run correction unavailable", entries, err)
		}
		denied := current
		input, _ := json.Marshal(CreateRunRequest{SourceScope: []string{"other.example"}})
		denied.InputJSON = string(input)
		if _, err = s.sourceExperience(ctx, denied, id); err == nil {
			t.Fatal("foreign scope exposed experience")
		}
		s.Data.DB.Model(&agentdata.SourceHead{}).Where("source_id = ?", id).Update("status", "quarantined")
		if _, err = s.sourceExperience(ctx, current, id); err == nil {
			t.Fatal("quarantined capability reused")
		}
	}
}
