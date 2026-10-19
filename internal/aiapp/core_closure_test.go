package aiapp

import (
	"context"
	"encoding/json"
	"fmt"
	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/engine"
	"go-lua-crawler/internal/knowledge"
	"go-lua-crawler/internal/repository"
	task "go-lua-crawler/internal/task"
	"go-lua-crawler/internal/testdb"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Only the external model/HTTP world are fixtures. All four core decisions,
// SQL registration, child execution, publication and evidence reads are real.
type closureChat struct {
	goal      string
	calls     int
	candidate string
	spec      *TaskSpec
	code      string
}

func (p *closureChat) Chat(_ context.Context, r ChatRequest) (ChatResponse, error) {
	for _, m := range r.Messages {
		if m.Role == "tool" {
			var out ToolResult
			json.Unmarshal([]byte(m.Content), &out)
			if out.Name == "validate_program" {
				var receipt map[string]any
				json.Unmarshal(out.Data, &receipt)
				// An old consumed validation is a recoverable asset reference, not
				// a fresh trial. The current trial must still expose typed checks.
				if compacted, _ := receipt["compacted"].(bool); compacted {
					candidate, _ := receipt["candidate_id"].(string)
					if candidate == "" {
						return ChatResponse{}, fmt.Errorf("compacted validation lost candidate identity")
					}
					p.candidate = candidate
					continue
				}
				if _, ok := receipt["checks"].(map[string]any); !ok {
					return ChatResponse{}, fmt.Errorf("validation checks are not a typed object: %s", m.Content)
				}
				if _, ok := receipt["result"].(map[string]any); !ok {
					return ChatResponse{}, fmt.Errorf("trial result is not a typed object")
				}
				var c Candidate
				json.Unmarshal(out.Data, &c)
				p.candidate = c.ID
			}
		}
	}
	p.calls++
	name := ""
	var args any
	switch p.calls {
	case 1:
		name = "plan_task"
		args = TaskSpec{Goal: p.goal, SchemaVersion: "v1", RequiredFields: []FieldRequirement{{Name: "name", Type: "string"}, {Name: "version", Type: "string"}}, JoinKeys: []string{"name"}}
		if p.spec != nil {
			args = *p.spec
		}
	case 2:
		name = "validate_program"
		code := p.code
		if code == "" {
			code = `local body=assert(http_get(TARGET_URL));local data=assert(json_decode(body));local items={};for _,row in ipairs(data.rows) do table.insert(items,{name=row.name,version=row.version,source_url=TARGET_URL}) end;return true,{items=items}`
		}
		args = map[string]any{"code": code, "kind": "adapter", "source_id": "release-source", "purpose": "public facts", "url": "https://fixture.example/releases"}
	case 3:
		name = "register_source"
		args = map[string]string{"candidate_id": p.candidate, "description": "parameterized public release adapter"}
	case 4:
		name = "submit_acquisition"
		args = map[string]string{"source_id": "release-source", "url": "https://fixture.example/releases"}
	case 5:
		name = "evaluate_result"
		args = map[string]any{}
	case 6:
		name = "finish_task"
		args = map[string]string{"status": "completed", "reason": "observed facts satisfy frozen requirements"}
	default:
		return ChatResponse{}, fmt.Errorf("unexpected model call %d", p.calls)
	}
	raw, _ := json.Marshal(args)
	calls := []ToolCall{{ID: fmt.Sprintf("action-%d", p.calls), Type: "function", Function: FunctionCall{Name: name, Arguments: string(raw)}}}
	// V3 uses host-owned progress; update_task_progress is no longer exposed.
	return ChatResponse{Message: Message{Role: "assistant", ToolCalls: calls}, Usage: &Usage{TotalTokens: 7}}, nil
}

func TestProductionCoreLoopRegistersExecutesAndCompletes(t *testing.T) {
	db := testdb.Open(t)
	s := NewService(db)
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	s.Fingerprint = "closure-fixture"
	oldDB := repository.DB
	repository.DB = db
	t.Cleanup(func() { repository.DB = oldDB })
	runner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in RunnerRequest
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Error(err)
			return
		}
		in.Limits.Timeout = 20 * time.Second // Runner applies its own timeout; JSON excludes it.
		replay := acquisition.NewReplay([]acquisition.ReplayEntry{{Method: "GET", URL: in.Program.TargetURL, Status: 200, Body: `{"rows":[{"name":"Alpha","version":"1.0"}]}`, FetchedAt: time.Now()}})
		result, err := engine.RunProgram(r.Context(), in.Program, in.Limits, replay.Fetch)
		if err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		json.NewEncoder(w).Encode(RunnerResponse{Result: &result})
	}))
	defer runner.Close()
	s.Runner, _ = NewRunnerClient(runner.URL, "fixture-token")
	ctx := context.Background()
	callerSpec := TaskSpec{Goal: "collect release facts", SchemaVersion: "v1", RequiredFields: []FieldRequirement{{Name: "name", Type: "string"}, {Name: "version", Type: "string"}}, JoinKeys: []string{"name"}, SourceScope: []string{"fixture.example"}}
	r, err := s.Create(ctx, CreateRunRequest{RequestKey: "production-closure", Goal: callerSpec.Goal, SourceScope: callerSpec.SourceScope, Spec: &callerSpec})
	if err != nil {
		t.Fatal(err)
	}
	model := &closureChat{goal: r.Goal, spec: &callerSpec}
	s.Model = model
	for n := 0; n < 9; n++ {
		if err = s.Advance(ctx, r.ID); err != nil {
			t.Fatal(err)
		}
	}
	stored, _ := s.Data.Run(ctx, r.ID)
	if stored.Status != "waiting" || model.calls != 4 {
		t.Fatalf("not waiting after submit: %s %d", stored.Status, model.calls)
	}
	rows, _ := s.ChildResults(ctx, r.ID)
	if len(rows) != 1 {
		t.Fatalf("children %d", len(rows))
	}
	// Recreate the service at the durable wait boundary; no core substitutions.
	s = NewService(db)
	s.Fingerprint = "closure-fixture"
	s.Model = model
	s.Runner, _ = NewRunnerClient(runner.URL, "fixture-token")
	if err = s.Advance(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	if model.calls != 4 {
		t.Fatal("waiting spent model budget")
	}
	child := rows[0].Task
	token, err := repository.ClaimCrawlTask(ctx, child, time.Minute, false)
	if err != nil {
		t.Fatal(err)
	}
	db.First(&child, child.ID)
	persist, err := s.ExecuteTask(ctx, child)
	if err != nil {
		t.Fatal(err)
	}
	if err = repository.CompleteCrawlTask(ctx, child.ID, task.StatusSucceeded, "", "", time.Now(), token, persist); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 5; n++ {
		if err = s.Advance(ctx, r.ID); err != nil {
			t.Fatal(err)
		}
	}
	stored, _ = s.Data.Run(ctx, r.ID)
	if stored.Status != "completed" || model.calls != 6 {
		t.Fatalf("not completed: %s %d", stored.Status, model.calls)
	}
	links, _ := s.Data.Children(ctx, r.ID)
	if len(links) != 1 || links[0].TaskID != child.ID {
		t.Fatal("restart duplicated task")
	}
	embed := &projectionEmbedFixture{}
	index := knowledge.Indexer{Store: s.Knowledge, Fingerprint: s.Fingerprint, Embed: embed, Vectors: projectionVectorsFixture{}}
	if err = index.Once(ctx); err != nil {
		t.Fatal(err)
	}
	heads, _ := s.Knowledge.Heads(ctx)
	chunks, _ := s.Knowledge.GenerationChunks(ctx, heads[0].Generation)
	set := knowledge.NewEvidenceSet(chunks, map[string]knowledge.Head{heads[0].ID: heads[0]})
	args, _ := json.Marshal(map[string]string{"evidence_id": set.Entries[0].ID})
	if _, err = ExecuteReadOnlyCall(ctx, ToolCall{ID: "read-final", Type: "function", Function: FunctionCall{Name: "read_evidence", Arguments: string(args)}}, knowledge.SetReader{Store: s.Knowledge, Set: set}); err != nil {
		t.Fatal(err)
	}
	traces, _ := s.Data.Traces(ctx, r.ID)
	ops, _ := s.Data.Operations(ctx, r.ID)
	result, err := s.Evaluate(ctx, stored)
	if err != nil || !result.Coverage.Satisfied || result.Records[0].Fields["version"] != "1.0" {
		t.Fatalf("wrong delivery: %+v %v", result, err)
	}
	if evidenceDir := os.Getenv("GORELAY_TEST_EVIDENCE_DIR"); evidenceDir != "" {
		// Tokens belong to a scripted provider, explicitly excluded from resume claims.
		raw, _ := json.MarshalIndent(map[string]any{"mode": "scripted_model_frozen_http_real_mysql", "live_model": false, "run": stored, "result": result, "loop": BuildLoopReport(traces), "operations": ops, "child_count": len(links), "embedding_fixture_calls": embed.Calls}, "", "  ")
		if err = os.WriteFile(filepath.Join(evidenceDir, "production-core-closure.json"), append(raw, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
}
