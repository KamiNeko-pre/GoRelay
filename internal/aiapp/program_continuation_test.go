package aiapp

import (
	"context"
	"encoding/json"
	"fmt"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/testdb"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestProbeContinuationKeepsThePublicationAndSubmissionRoute(t *testing.T) {
	for _, kind := range []string{"adapter", "composition"} {
		for _, required := range []bool{false, true} {
			for _, used := range []int{0, probeAttemptLimit - 1} {
				t.Run(fmt.Sprintf("%s/published=%v/used=%d", kind, required, used), func(t *testing.T) {
					s, run, _ := storedHTMLReader(t, `<h2>Observed title</h2>`)
					spec := TaskSpec{Goal: "Collect observed titles", ContractVersion: "v2", SchemaVersion: "v1", MinRecords: 1, JoinKeys: []string{"title"}, RequiredFields: []FieldRequirement{{Name: "title", Type: "string"}}, RequirePublishedCapability: required}
					if err := spec.Validate(); err != nil {
						t.Fatal(err)
					}
					raw, _ := json.Marshal(spec)
					run.SpecJSON = string(raw)
					for i := 0; i < used; i++ {
						receipt, _ := toolData(ToolCall{ID: fmt.Sprintf("probe-%d", i), Function: FunctionCall{Name: "validate_program"}}, map[string]any{"mode": "probe"})
						data, _ := json.Marshal(receipt)
						if err := s.Data.DB.Create(&agentdata.Operation{RunID: run.ID, Key: fmt.Sprintf("tool:probe-%d", i), Kind: "validate_program", Stage: "succeeded", ResultJSON: string(data)}).Error; err != nil {
							t.Fatal(err)
						}
					}
					runner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"collection": map[string]any{"items": []any{map[string]any{"title": "Observed title"}}}}})
					}))
					defer runner.Close()
					s.Runner, _ = NewRunnerClient(runner.URL, "test")
					out, err := s.trialProbe(context.Background(), run, ToolCall{ID: "current-probe", Function: FunctionCall{Name: "validate_program"}}, ToolResult{CallID: "current-probe", Name: "validate_program"}, trialArgs{Kind: kind, Code: "return true,{items={}}", URL: "https://fixture.example", ProducedFields: []string{"title"}}, spec)
					if err != nil {
						t.Fatal(err)
					}
					var got struct {
						Next struct {
							Tool    string `json:"tool"`
							Changes struct {
								Kind   string   `json:"kind"`
								Mode   string   `json:"mode"`
								Fields []string `json:"produced_fields"`
							} `json:"argument_changes"`
							Instruction string `json:"instruction"`
						} `json:"next_action"`
						Step string `json:"next_step"`
					}
					json.Unmarshal(out.Data, &got)
					wantKind := kind
					if required {
						wantKind = "adapter"
					}
					if got.Next.Tool != "validate_program" || got.Next.Changes.Kind != wantKind || got.Next.Changes.Mode != "acquisition" || len(got.Next.Changes.Fields) != 1 || got.Next.Changes.Fields[0] != "title" {
						t.Fatalf("probe omits the exact delivery transition: %s", out.Data)
					}
					instructions := got.Step + " " + got.Next.Instruction
					if wantKind == "adapter" {
						if strings.Index(instructions, "register_source") < 0 || strings.Index(instructions, "register_source") > strings.Index(instructions, "submit_acquisition") || !strings.Contains(instructions, "source_id") || strings.Contains(instructions, "then submit_acquisition with the candidate_id") {
							t.Fatalf("probe skips or contradicts publication: %s", out.Data)
						}
					} else if strings.Contains(instructions, "register_source") || !strings.Contains(instructions, "candidate_id") {
						t.Fatalf("temporary composition lost its valid submission route: %s", out.Data)
					}
				})
			}
		}
	}
}

func TestProgramContinuationMatchesRequiredDelivery(t *testing.T) {
	for _, kind := range []string{"adapter", "composition"} {
		for _, required := range []bool{false, true} {
			args := trialArgs{Kind: kind, SourceID: "source", URL: "https://example.test/list", Purpose: "collect", Params: map[string]any{"limit": 3}}
			next := programDeliveryContinuation(TaskSpec{RequirePublishedCapability: required}, args, Candidate{ID: "candidate"}, "validation")
			raw, _ := json.Marshal(next)
			want := "register_source"
			if kind == "composition" {
				want = "submit_acquisition"
				if required {
					want = "validate_program"
				}
			}
			if next["tool"] != want {
				t.Fatal("host continuation contradicts task/kind", kind, required, string(raw))
			}
			if required && kind == "composition" && (next["argument_changes"] == nil || !strings.Contains(string(raw), "adapter")) {
				t.Fatal("conversion omits executable changes", string(raw))
			}
		}
	}
}

func TestRegistrationOffersTheExactValidatedSubmission(t *testing.T) {
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	in := admissionFixture(t)
	r, err := s.Create(context.Background(), CreateRunRequest{RequestKey: "registration-continuation", Goal: in.Spec.Goal, Spec: in.Spec, SourceScope: in.Scope})
	if err != nil {
		t.Fatal(err)
	}
	r, err = s.Data.Claim(context.Background(), r.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	in.Candidate.RunID = r.ID
	if err = s.Data.DB.Create(&in.Artifact).Error; err != nil {
		t.Fatal(err)
	}
	if err = s.Data.DB.Create(&in.Candidate).Error; err != nil {
		t.Fatal(err)
	}
	submission := map[string]any{"source_id": in.Candidate.SourceID, "url": "https://example.com/observed-entry", "params": map[string]any{"page": 2}}
	validation, _ := toolData(ToolCall{ID: "original-validation", Function: FunctionCall{Name: "validate_program"}}, map[string]any{"candidate_id": in.Candidate.ID, "artifact_hash": in.Candidate.ArtifactHash, "next_action": map[string]any{"tool": "register_source", "then_submit": submission}})
	raw, _ := json.Marshal(validation)
	if err = s.Data.DB.Create(&agentdata.Operation{RunID: r.ID, Key: "tool:original-validation", Kind: "validate_program", Stage: "succeeded", ResultJSON: string(raw)}).Error; err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]any{"candidate_id": in.Candidate.ID, "description": "collect supported facts"})
	out, err := s.executeDurableTool(context.Background(), r, ToolCall{ID: "publish", Type: "function", Function: FunctionCall{Name: "register_source", Arguments: string(args)}})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Next struct {
			Tool      string         `json:"tool"`
			Arguments map[string]any `json:"arguments"`
		} `json:"next_action"`
	}
	json.Unmarshal(out.Data, &got)
	if got.Next.Tool != "submit_acquisition" || agentdata.Hash(got.Next.Arguments) != agentdata.Hash(submission) {
		t.Fatalf("published result lost executable submission parameters: %s", out.Data)
	}
}
