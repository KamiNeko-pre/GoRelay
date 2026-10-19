package aiapp

import (
	"context"
	"encoding/json"
	"errors"
	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/engine"
	"go-lua-crawler/internal/knowledge"
	task "go-lua-crawler/internal/task"
	"testing"
	"time"
)

type decisionEvidenceReader struct{ calls int }

func (r *decisionEvidenceReader) ReadEvidence(_ context.Context, id string) (knowledge.Evidence, error) {
	r.calls++
	if id != "ev_current" {
		return knowledge.Evidence{}, knowledge.ErrInvalidReference
	}
	return knowledge.Evidence{ID: id, Text: "observed fact"}, nil
}

func TestCoreReadOnlyBoundary(t *testing.T) {
	r := &decisionEvidenceReader{}
	call := ToolCall{ID: "read-1", Type: "function", Function: FunctionCall{Name: "read_evidence", Arguments: `{"evidence_id":"ev_current"}`}}
	out, err := ExecuteReadOnlyCall(context.Background(), call, r)
	if err != nil || out.CallID != call.ID || r.calls != 1 {
		t.Fatalf("read failed: %+v %v", out, err)
	}
	for _, args := range []string{`{}`, `{"evidence_id":""}`, `{"evidence_id":"ev_current","extra":1}`, `{"evidence_id":"ev_current"} {}`} {
		call.Function.Arguments = args
		if _, err = ExecuteReadOnlyCall(context.Background(), call, r); err == nil {
			t.Fatalf("accepted %s", args)
		}
	}
	call.Function.Arguments = `{"evidence_id":"ev_foreign"}`
	if _, err = ExecuteReadOnlyCall(context.Background(), call, r); !errors.Is(err, knowledge.ErrInvalidReference) {
		t.Fatal(err)
	}
	call.Function.Name = "register_source"
	before := r.calls
	if _, err = ExecuteReadOnlyCall(context.Background(), call, r); err == nil || r.calls != before {
		t.Fatal("write tool reached reader")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = ExecuteReadOnlyCall(ctx, call, r); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func decisionState() LoopState {
	return LoopState{Run: agentdata.Run{ID: "run", Goal: "collect facts", Status: "reviewing", Deadline: time.Now().Add(time.Hour)}, Budget: BudgetSnapshot{Limits: DefaultBudgetLimits(), Deadline: time.Now().Add(time.Hour)}}
}
func TestCoreLoopDecisions(t *testing.T) {
	call := ToolCall{ID: "search-1", Type: "function", Function: FunctionCall{Name: "discover_sources", Arguments: `{"query":"public facts"}`}}
	for _, tc := range []struct {
		name         string
		mutate       func(*LoopState)
		kind, status string
	}{
		{"initial", func(s *LoopState) {}, "model", ""},
		{"proposal", func(s *LoopState) { s.Messages = []Message{{Role: "assistant", ToolCalls: []ToolCall{call}}} }, "tools", ""},
		{"feedback", func(s *LoopState) {
			s.Messages = []Message{{Role: "assistant", ToolCalls: []ToolCall{call}}, {Role: "tool", ToolCallID: call.ID, Content: `{"error":"search unavailable"}`}}
		}, "model", ""},
		{"waiting", func(s *LoopState) {
			s.TaskResults = []ChildResult{{Task: task.CrawlTask{ID: 1, Status: task.StatusQueued}}}
		}, "wait", ""},
		{"failed_child", func(s *LoopState) {
			s.TaskResults = []ChildResult{{Task: task.CrawlTask{ID: 1, Status: task.StatusFailed}}}
		}, "model", ""},
		{"budget_empty", func(s *LoopState) { s.Budget.Spent.ModelCalls = s.Budget.Limits.ModelCalls }, "finish", "failed"},
		{"budget_partial", func(s *LoopState) {
			s.Budget.Spent.ModelCalls = s.Budget.Limits.ModelCalls
			s.Result = &TaskResult{Coverage: CoverageReport{PartialValidRecords: 1}}
		}, "finish", "partial"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := decisionState()
			tc.mutate(&s)
			a, err := RunTaskLoop(context.Background(), s)
			if err != nil || a.Kind != tc.kind || a.Status != tc.status {
				t.Fatalf("%+v %v", a, err)
			}
		})
	}
}
func TestCoreLoopCompletionAndBoundedRetry(t *testing.T) {
	s := decisionState()
	call := ToolCall{ID: "finish", Type: "function", Function: FunctionCall{Name: "finish_task", Arguments: `{"status":"completed","reason":"done"}`}}
	out, _ := toolData(call, map[string]string{"status": "completed", "reason": "done"})
	raw, _ := json.Marshal(out)
	s.Messages = []Message{{Role: "assistant", ToolCalls: []ToolCall{call}}, {Role: "tool", ToolCallID: call.ID, Content: string(raw)}}
	a, err := RunTaskLoop(context.Background(), s)
	if err != nil || a.Kind != "model" {
		t.Fatalf("accepted false completion: %+v %v", a, err)
	}
	s.Spec = &TaskSpec{Goal: s.Run.Goal}
	s.Result = &TaskResult{Coverage: CoverageReport{Satisfied: true, ValidRecords: 1}}
	a, err = RunTaskLoop(context.Background(), s)
	if err != nil || a.Kind != "finish" || a.Status != "completed" {
		t.Fatalf("lost valid completion: %+v %v", a, err)
	}
	s = decisionState()
	s.Operations = []agentdata.Operation{{Kind: "model", Stage: "unknown_external"}}
	a, err = RunTaskLoop(context.Background(), s)
	if err != nil || a.Kind != "model" || a.ModelAttempt != 1 {
		t.Fatalf("retry: %+v %v", a, err)
	}
	s.Operations = append(s.Operations, agentdata.Operation{Kind: "model", Stage: "unknown_external"}, agentdata.Operation{Kind: "model", Stage: "unknown_external"})
	a, err = RunTaskLoop(context.Background(), s)
	if err != nil || a.Kind != "finish" || a.Status != "failed" {
		t.Fatalf("unbounded retries: %+v %v", a, err)
	}
}

func admissionFixture(t *testing.T) AdmissionContext {
	t.Helper()
	spec := TaskSpec{Goal: "collect Alpha facts", SchemaVersion: "v1", RequiredFields: []FieldRequirement{{Name: "name", Type: "string"}}, JoinKeys: []string{"name"}}
	schema, _ := json.Marshal(spec)
	a := agentdata.ProgramArtifact{Hash: "artifact", Code: "return true,{items={}}", Kind: "adapter", PolicyVersion: "public-read-v1", ScopeJSON: `["example.com"]`, OutputSchema: string(schema)}
	r := engine.ProgramResult{Collection: map[string]any{"items": []any{map[string]any{"name": "Alpha", "source_url": "https://example.com/facts"}}}, Requests: []acquisition.Receipt{{Method: "GET", URL: "https://example.com/facts", Status: 200, FetchedAt: time.Now()}}, Stats: acquisition.Stats{Requests: 1}}
	raw, _ := json.Marshal(r)
	checks, _ := json.Marshal(map[string]any{"coverage": CoverageReport{Satisfied: true, ValidRecords: 1}, "semantic_support_checked": false})
	c := Candidate{ID: "candidate", RunID: "run", SourceID: "alpha", ArtifactHash: a.Hash, PolicyVersion: a.PolicyVersion, ValidatorVersion: "typed-receipt-v1", ResultJSON: string(raw), ResultHash: agentdata.Hash(r), ChecksJSON: string(checks)}
	return AdmissionContext{Candidate: c, Artifact: a, Spec: &spec, Scope: []string{"example.com"}, PolicyVersion: a.PolicyVersion, ValidatorVersion: c.ValidatorVersion, Mode: "register"}
}
func TestCoreSourceAdmission(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*AdmissionContext)
		allowed bool
	}{
		{"new_valid_source", func(in *AdmissionContext) {}, true},
		{"wrong_artifact", func(in *AdmissionContext) { in.Candidate.ArtifactHash = "other" }, false},
		{"stale_parent", func(in *AdmissionContext) { in.Current.Epoch = 2 }, false},
		{"wrong_policy", func(in *AdmissionContext) { in.Artifact.PolicyVersion = "shell" }, false},
		{"composition_not_source", func(in *AdmissionContext) { in.Artifact.Kind = "composition" }, false},
		{"false_trial_proof", func(in *AdmissionContext) { in.Candidate.ChecksJSON = `{"coverage":{"valid_records":0}}` }, false},
		{"foreign_scope", func(in *AdmissionContext) { in.Scope = []string{"other.example"} }, false},
		{"corrupt_result", func(in *AdmissionContext) { in.Candidate.ResultHash = "other" }, false},
		{"rollback", func(in *AdmissionContext) { in.Mode = "rollback"; in.Spec = nil; in.Scope = nil }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := admissionFixture(t)
			tc.mutate(&in)
			d, err := RegisterValidatedSource(context.Background(), in)
			if err != nil || d.Allowed != tc.allowed {
				t.Fatalf("%+v %v", d, err)
			}
		})
	}
}
