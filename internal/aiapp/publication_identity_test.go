package aiapp

import (
	"context"
	"encoding/json"
	"fmt"
	"go-lua-crawler/internal/testdb"
	"testing"
	"time"
)

func TestPublicationUsesToolAndParameters(t *testing.T) {
	input := func(candidate string, description any) string {
		raw, _ := json.Marshal(map[string]any{"candidate_id": candidate, "description": description})
		return businessToolInput(FunctionCall{Name: "register_source", Arguments: string(raw)}, nil)
	}
	if input("candidate-a", "first description") == input("candidate-a", "rewritten description") {
		t.Error("different description parameters were collapsed")
	}
	if input("candidate-a", "valid") == input("candidate-b", "valid") {
		t.Error("different validated candidate collapsed")
	}
	for _, invalid := range []any{"", nil, true, 42} {
		if input("candidate-a", invalid) == input("candidate-a", "valid") {
			t.Errorf("invalid metadata poisons a later corrected proposal: %v", invalid)
		}
	}
}

func TestPublicationIdempotencyRejectsOnlySameParameters(t *testing.T) {
	for _, kind := range []string{"adapter", "composition"} {
		t.Run(kind, func(t *testing.T) {
			s := NewService(testdb.Open(t))
			if err := s.Migrate(); err != nil {
				t.Fatal(err)
			}
			in := admissionFixture(t)
			r, err := s.Create(context.Background(), CreateRunRequest{RequestKey: "publication-intent", Goal: in.Spec.Goal, Spec: in.Spec, SourceScope: in.Scope})
			if err != nil {
				t.Fatal(err)
			}
			r, err = s.Data.Claim(context.Background(), r.ID, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				a := in.Artifact
				a.Kind = kind
				a.Hash = fmt.Sprintf("artifact-%d", i)
				if err := s.Data.DB.Create(&a).Error; err != nil {
					t.Fatal(err)
				}
				c := in.Candidate
				c.ID = fmt.Sprintf("candidate-%d", i)
				c.SourceID = fmt.Sprintf("source-%d", i)
				c.RunID = r.ID
				c.ArtifactHash = a.Hash
				if err := s.Data.DB.Create(&c).Error; err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			s.Admission = func(ctx context.Context, in AdmissionContext) (AdmissionDecision, error) {
				calls++
				return RegisterValidatedSource(ctx, in)
			}
			call := func(id, candidate, description string) (ToolResult, error) {
				raw, _ := json.Marshal(map[string]any{"candidate_id": candidate, "description": description})
				return s.executeDurableTool(context.Background(), r, ToolCall{ID: id, Type: "function", Function: FunctionCall{Name: "register_source", Arguments: string(raw)}})
			}
			first, err := call("first", "candidate-0", "first description")
			if kind == "adapter" && err != nil || kind == "composition" && (err == nil || first.Failure == nil) {
				t.Fatal("first result", first, err)
			}
			again, err := call("renamed", "candidate-0", "first description")
			if err == nil || again.Failure == nil || again.Failure.Code != "DUPLICATE_TOOL_CALL" || calls != 1 {
				t.Fatalf("registration ran again instead of refusing: calls=%d result=%+v error=%v", calls, again, err)
			}
			_, _ = call("changed-description", "candidate-0", "rewritten description")
			if calls != 2 {
				t.Fatal("different parameters were incorrectly blocked", calls)
			}
			_, _ = call("different-candidate", "candidate-1", "first description")
			if calls != 3 {
				t.Fatal("new candidate could not execute", calls)
			}
		})
	}
}
