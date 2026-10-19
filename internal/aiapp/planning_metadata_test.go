package aiapp

import (
	"context"
	"encoding/json"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/testdb"
	"testing"
	"time"
)

func TestPlanUsesHostMetadataWithoutModelEcho(t *testing.T) {
	for _, goal := range []string{"查询版本及发布时间，出处属于来源元数据", "compare two vendors' price and currency; preserve requested delivery columns"} {
		t.Run(goal, func(t *testing.T) {
			s := NewService(testdb.Open(t))
			if err := s.Migrate(); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			r, err := s.Create(ctx, CreateRunRequest{RequestKey: "host-metadata", Goal: goal, SourceScope: []string{"example.com"}})
			if err != nil {
				t.Fatal(err)
			}
			r, err = s.Data.Claim(ctx, r.ID, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			args := `{"required_fields":[{"name":"version","type":"string"}],"join_keys":["version"]}`
			call := ToolCall{ID: "plan", Type: "function", Function: FunctionCall{Name: "plan_task", Arguments: args}}
			if _, err = s.executeDurableTool(ctx, r, call); err != nil {
				t.Fatal(err)
			}
			r, _ = s.Data.Run(ctx, r.ID)
			var spec TaskSpec
			if err = json.Unmarshal([]byte(r.ContractProposalJSON), &spec); err != nil || spec.Goal != goal || spec.SchemaVersion != "v1" || len(spec.RequiredFields) != 1 || spec.SourceScope[0] != "example.com" {
				t.Fatal("host goal/version/scope or requested fields changed", spec, err)
			}
			if err = s.Data.SaveRun(ctx, r.ID, r.Token, map[string]any{"status": "needs_input"}); err != nil {
				t.Fatal(err)
			}
			if _, err = s.ApproveContract(ctx, r.ID, agentdata.Hash(json.RawMessage(r.ContractProposalJSON))); err != nil {
				t.Fatal(err)
			}
			r, err = s.Data.Claim(ctx, r.ID, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			// Model representation echoes cannot rewrite host identity. The
			// independently frozen business columns remain protected.
			for i, bad := range []string{
				`{"goal":"different goal","required_fields":[{"name":"version","type":"string"}],"join_keys":["version"]}`,
				`{"schema_version":"invented-version","required_fields":[{"name":"version","type":"string"}],"join_keys":["version"]}`,
				`{"required_fields":[{"name":"price","type":"number"}]}`,
			} {
				call.ID = "bad-" + string(rune('a'+i))
				call.Function.Arguments = bad
				_, err = s.executeDurableTool(ctx, r, call)
				if i < 2 && err != nil {
					t.Fatalf("metadata echo changed immutable business plan: %v", err)
				}
				if i == 2 && err == nil {
					t.Fatalf("accepted business-column rewrite: %s", bad)
				}
				after, _ := s.Data.Run(ctx, r.ID)
				if after.Goal != r.Goal || after.SpecJSON != r.SpecJSON || after.InputJSON != r.InputJSON {
					t.Fatal("model changed host identity or frozen plan")
				}
			}
		})
	}
}

func TestPlanPreservesCallerPinnedVersionAndRequestedEntityColumn(t *testing.T) {
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	spec := TaskSpec{Goal: "deliver repository and version", SchemaVersion: "v2", RequiredFields: []FieldRequirement{{"repository", "string"}, {"version", "string"}}, Entities: []string{"Alpha"}, EntityField: "repository", JoinKeys: []string{"repository"}, SourceScope: []string{"example.com"}}
	r, err := s.Create(ctx, CreateRunRequest{RequestKey: "caller-contract", Goal: spec.Goal, Spec: &spec, SourceScope: spec.SourceScope})
	if err != nil {
		t.Fatal(err)
	}
	r, err = s.Data.Claim(ctx, r.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	spec.Goal, spec.SchemaVersion = "", ""
	args, _ := json.Marshal(spec)
	if _, err = s.executeDurableTool(ctx, r, ToolCall{ID: "plan", Type: "function", Function: FunctionCall{Name: "plan_task", Arguments: string(args)}}); err != nil {
		t.Fatal("omitted metadata rewrote caller contract", err)
	}
	saved, _ := s.Data.Run(ctx, r.ID)
	if saved.SpecJSON != r.SpecJSON {
		t.Fatal("caller version or legitimate requested entity column changed")
	}
}
