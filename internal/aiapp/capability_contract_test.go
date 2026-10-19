package aiapp

import (
	"context"
	"encoding/json"
	"fmt"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/testdb"
	"testing"
	"time"
)

func TestCatalogAndSubmissionShareCapabilityContract(t *testing.T) {
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	spec := TaskSpec{Goal: "collect release fields", SchemaVersion: "v1", RequiredFields: []FieldRequirement{{"name", "string"}, {"version", "string"}, {"license", "string"}}, JoinKeys: []string{"name"}}
	r, err := s.Create(ctx, CreateRunRequest{RequestKey: "capability-contract", Goal: spec.Goal, Spec: &spec, SourceScope: []string{"example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	r, err = s.Data.Claim(ctx, r.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		fields []FieldRequirement
		scope  string
		want   bool
	}{
		{"release", []FieldRequirement{{"name", "string"}, {"version", "string"}}, `["example.com"]`, true},
		{"license", []FieldRequirement{{"name", "string"}, {"license", "string"}}, `["docs.example.com"]`, true},
		{"unrelated", []FieldRequirement{{"product", "string"}, {"price", "number"}}, `["example.com"]`, false},
		{"wrong-type", []FieldRequirement{{"name", "string"}, {"version", "integer"}}, `["example.com"]`, false},
		{"no-join-key", []FieldRequirement{{"license", "string"}}, `["example.com"]`, false},
		{"foreign-scope", []FieldRequirement{{"name", "string"}, {"version", "string"}}, `["other.example"]`, false},
		{"unrestricted", []FieldRequirement{{"name", "string"}, {"version", "string"}}, `null`, false},
	}
	for _, tc := range cases {
		output := spec
		output.RequiredFields = tc.fields
		output.JoinKeys = nil
		schema, _ := json.Marshal(output)
		a, err := s.Data.SaveArtifact(ctx, agentdata.ProgramArtifact{Code: "return true,{items={}} -- " + tc.name, Kind: "adapter", PolicyVersion: "public-read-v1", OutputSchema: string(schema), ScopeJSON: tc.scope})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.Data.Publish(ctx, agentdata.SourceVersion{SourceID: tc.name, ArtifactHash: a.Hash}, "", 0); err != nil {
			t.Fatal(err)
		}
	}
	catalogCall := ToolCall{ID: "catalog", Type: "function", Function: FunctionCall{Name: "list_sources", Arguments: `{}`}}
	out, err := s.executeDurableTool(ctx, r, catalogCall)
	if err != nil {
		t.Fatal(err)
	}
	var catalog struct {
		Active []SourceCapability `json:"active"`
	}
	if err = json.Unmarshal(out.Data, &catalog); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, item := range catalog.Active {
		seen[item.Source.SourceID] = true
	}
	for _, tc := range cases {
		if seen[tc.name] != tc.want {
			t.Errorf("catalog %s visible=%v want=%v", tc.name, seen[tc.name], tc.want)
		}
		target := "https://example.com/releases"
		if tc.name == "license" {
			target = "https://docs.example.com/releases"
		}
		call := ToolCall{ID: "submit-" + tc.name, Type: "function", Function: FunctionCall{Name: "submit_acquisition", Arguments: fmt.Sprintf(`{"source_id":%q,"url":%q}`, tc.name, target)}}
		_, err := s.executeDurableTool(ctx, r, call)
		if (err == nil) != tc.want {
			t.Errorf("submit %s err=%v want allowed=%v", tc.name, err, tc.want)
		}
	}
	children, err := s.ChildResults(ctx, r.ID)
	if err != nil || len(children) != 2 {
		t.Fatalf("incompatible capabilities queued: %d %v", len(children), err)
	}
	var outboxes int64
	if err := s.Data.DB.Table("task_outboxes").Count(&outboxes).Error; err != nil {
		t.Fatal(err)
	}
	if outboxes != 2 {
		t.Fatalf("rejected proposals created Outbox rows: %d", outboxes)
	}
}

func TestCapabilityCompatibilityPreservesProjectionAndNumericWidening(t *testing.T) {
	spec := TaskSpec{Goal: "current measurements", SchemaVersion: "v1", RequiredFields: []FieldRequirement{{"name", "string"}, {"count", "number"}, {"license", "string"}}, JoinKeys: []string{"name"}}
	raw, _ := json.Marshal(spec)
	r := agentdata.Run{SpecJSON: string(raw), InputJSON: `{"source_scope":["example.com","other.example"]}`}
	output := TaskSpec{Goal: "adapter measurements", SchemaVersion: "v1", RequiredFields: []FieldRequirement{{"name", "string"}, {"count", "integer"}, {"extra", "boolean"}}}
	schema, _ := json.Marshal(output)
	a := agentdata.ProgramArtifact{OutputSchema: string(schema), ScopeJSON: `["other.example","docs.example.com"]`}
	if err := checkCapabilityForRun(r, a); err != nil {
		t.Fatal("compatible subset and integer-to-number projection rejected:", err)
	}
	// A public task can use a narrower adapter without expanding its permissions.
	r.InputJSON = `{}`
	if err := checkCapabilityForRun(r, a); err != nil {
		t.Fatal("narrow capability rejected for public task:", err)
	}
	output.SchemaVersion = "v2"
	schema, _ = json.Marshal(output)
	a.OutputSchema = string(schema)
	if err := checkCapabilityForRun(r, a); err == nil {
		t.Fatal("record schema version mismatch accepted")
	}
	// Reverse conversion is not safe: a number promise may contain decimals.
	output.SchemaVersion = "v1"
	output.RequiredFields[1].Type = "number"
	spec.RequiredFields[1].Type = "integer"
	raw, _ = json.Marshal(spec)
	r.SpecJSON = string(raw)
	schema, _ = json.Marshal(output)
	a.OutputSchema = string(schema)
	if err := checkCapabilityForRun(r, a); err == nil {
		t.Fatal("number-to-integer promise accepted")
	}
}
