package aiapp

import (
	"context"
	"encoding/json"
	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/engine"
	"go-lua-crawler/internal/testdb"
	"testing"
	"time"
)

func TestPlanningProposalMetadataCannotOwnHostIdentity(t *testing.T) {
	for _, echo := range []struct{ goal, version string }{{"equivalent paraphrase", ""}, {"", "model-schema"}, {"equivalent paraphrase", "model-schema"}} {
		t.Run(echo.goal+echo.version, func(t *testing.T) {
			s := NewService(testdb.Open(t))
			if err := s.Migrate(); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			r, err := s.Create(ctx, CreateRunRequest{RequestKey: "metadata", Goal: "original request", SourceScope: []string{"example.com"}})
			if err != nil {
				t.Fatal(err)
			}
			r, err = s.Data.Claim(ctx, r.ID, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			args, _ := json.Marshal(TaskSpec{Goal: echo.goal, SchemaVersion: echo.version, RequiredFields: []FieldRequirement{{Name: "value", Type: "string"}}, JoinKeys: []string{"value"}})
			if _, err = s.executeDurableTool(ctx, r, ToolCall{ID: "plan", Type: "function", Function: FunctionCall{Name: "plan_task", Arguments: string(args)}}); err != nil {
				t.Fatal("model representation metadata rejected otherwise valid business plan", err)
			}
			saved, _ := s.Data.Run(ctx, r.ID)
			spec, err := specOf(saved)
			if err != nil || spec.Goal != r.Goal || spec.SchemaVersion != "v1" || len(spec.SourceScope) != 1 || spec.SourceScope[0] != "example.com" {
				t.Fatal("model gained ownership of goal/version/scope", spec, err)
			}
		})
	}
}

func TestLegacyPlanningMetadataValidationIsNotRewritten(t *testing.T) {
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r, err := s.Create(ctx, CreateRunRequest{RequestKey: "legacy-identity", Goal: "original"})
	if err != nil {
		t.Fatal(err)
	}
	var budget BudgetSnapshot
	if err = json.Unmarshal([]byte(r.BudgetJSON), &budget); err != nil {
		t.Fatal(err)
	}
	budget.LoopVersion = 1
	raw, _ := json.Marshal(budget)
	if err = s.Data.DB.Model(&agentdata.Run{}).Where("id = ?", r.ID).Update("budget_json", string(raw)).Error; err != nil {
		t.Fatal(err)
	}
	r, err = s.Data.Claim(ctx, r.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	call := ToolCall{ID: "legacy-plan", Type: "function", Function: FunctionCall{Name: "plan_task", Arguments: `{"goal":"foreign","required_fields":[{"name":"value","type":"string"}]}`}}
	if _, err = s.executeDurableTool(ctx, r, call); err == nil {
		t.Fatal("legacy protocol changed")
	}
	saved, _ := s.Data.Run(ctx, r.ID)
	if saved.SpecJSON != "" || saved.Goal != r.Goal {
		t.Fatal("rejected legacy proposal mutated identity")
	}
}

func TestDirectExtractionBindsAbsentSourceURLFromObservedInspection(t *testing.T) {
	for _, url := range []string{"https://example.com/docs", "https://example.com/api/data"} {
		t.Run(url, func(t *testing.T) {
			s := NewService(testdb.Open(t))
			if err := s.Migrate(); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			spec := TaskSpec{Goal: "value", SchemaVersion: "v1", RequiredFields: []FieldRequirement{{Name: "value", Type: "string"}}, JoinKeys: []string{"value"}}
			r, err := s.Create(ctx, CreateRunRequest{RequestKey: "direct-bind", Goal: spec.Goal, Spec: &spec})
			if err != nil {
				t.Fatal(err)
			}
			r, err = s.Data.Claim(ctx, r.ID, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			receipt := acquisition.Receipt{Method: "GET", URL: url, Status: 200, FetchedAt: time.Now()}
			observed, _ := toolData(ToolCall{ID: "inspect", Function: FunctionCall{Name: "inspect_source"}}, sourceInspection{Source: acquisition.Response{Receipt: receipt, Body: "fixture observed text"}})
			raw, _ := json.Marshal(observed)
			if err = s.Data.DB.Create(&agentdata.Operation{RunID: r.ID, Key: "tool:inspect", Kind: "inspect_source", Stage: "succeeded", ResultJSON: string(raw)}).Error; err != nil {
				t.Fatal(err)
			}
			call := ToolCall{ID: "extract", Type: "function", Function: FunctionCall{Name: "submit_extraction", Arguments: `{"inspection_call_id":"inspect","items":[{"value":"A"}]}`}}
			if _, err = s.executeDurableTool(ctx, r, call); err != nil {
				t.Fatal("host-declared URL binding was delegated back to model", err)
			}
			children, err := s.ChildResults(ctx, r.ID)
			if err != nil || len(children) != 1 || len(children[0].Records) != 1 || children[0].Records[0].Provenance.URL != url || !children[0].Records[0].Provenance.FetchedAt.Equal(receipt.FetchedAt) {
				t.Fatal("observed provenance lost", children, err)
			}
			// Generated Lua still supplies a URL for each of its potentially many GETs.
			_, err = BindRecords(engine.ProgramResult{Collection: map[string]any{"items": []any{map[string]any{"value": "A"}}}, Requests: []acquisition.Receipt{receipt}}, RecordBinding{SourceID: "lua", DataVersion: "v1", TaskID: 1, SchemaVersion: "v1"})
			if err == nil {
				t.Fatal("single-inspection fix weakened generated Lua provenance")
			}
		})
	}
}
