package information

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/knowledge"
	"go-lua-crawler/internal/repository"
	task "go-lua-crawler/internal/task"
)

type routingOnlyModel struct{ calls int }

func (m *routingOnlyModel) Chat(_ context.Context, in aiapp.ChatRequest) (aiapp.ChatResponse, error) {
	m.calls++
	if len(in.Tools) != 1 {
		return aiapp.ChatResponse{}, fmt.Errorf("stage must advertise its real decision tool")
	}
	name := in.Tools[0].Function.Name
	// Tests only the negative routing branches. No source URL, locator, code or
	// business result is supplied to a simulated model.
	args := map[string]any{"action": "no_known_api", "reason": "no known API from the visible context"}
	if name == "choose_asset" {
		args["action"] = "no_matching_asset"
	}
	return aiapp.ChatResponse{Message: aiapp.Message{Role: "assistant", ToolCalls: []aiapp.ToolCall{{ID: fmt.Sprint(m.calls), Type: "function", Function: aiapp.FunctionCall{Name: name, Arguments: string(mustMarshal(args))}}}}, FinishReason: "tool_calls"}, nil
}

func TestLuaFailureDoesNotImplyBrowserAndInFlightCorrectionKeepsOwnership(t *testing.T) {
	for _, tc := range []struct {
		name, parentStatus, want string
		status                   int
	}{
		{"nonterminal-with-failed-child", "reviewing", "wait_crawler", 0},
		{"nonterminal-with-blocked-source", "reviewing", "wait_crawler", 403},
		{"terminal-without-http-status-needs-cause-report", "failed", "review_browser", 0},
		{"terminal-with-http-barrier-needs-assessment", "failed", "review_browser", 403},
		{"terminal-with-rate-limit-needs-assessment", "failed", "review_browser", 429},
		{"cancelled-cannot-start-browser", "cancelled", "cancelled", 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := retrievalService(t)
			ctx := context.Background()
			spec := aiapp.TaskSpec{Goal: "collect public observations", SchemaVersion: "v1", RequiredFields: []aiapp.FieldRequirement{{Name: "name", Type: "string"}}}
			r, err := s.Create(ctx, Input{RequestKey: tc.name, Goal: spec.Goal, Spec: &spec, SourceScope: []string{"source.example"}})
			if err != nil {
				t.Fatal(err)
			}
			child, err := s.Crawler.Create(ctx, aiapp.CreateRunRequest{RequestKey: "child", Goal: spec.Goal, Spec: &spec, SourceScope: []string{"source.example"}})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.DB.Model(&agentdata.Run{}).Where("id = ?", child.ID).Updates(map[string]any{"status": tc.parentStatus, "last_error": "could not acquire the requested source"}).Error; err != nil {
				t.Fatal(err)
			}
			work := task.CrawlTask{RequestKey: "failed-attempt", RunID: child.ID, Target: "agent_program", URL: "https://source.example/items", Status: task.StatusFailed}
			if err := s.DB.Create(&work).Error; err != nil {
				t.Fatal(err)
			}
			if err := s.DB.Create(&agentdata.RunTask{RunID: child.ID, TaskID: work.ID, OperationID: 1}).Error; err != nil {
				t.Fatal(err)
			}
			if tc.status > 0 {
				out := aiapp.ToolResult{Name: "inspect_source", Failure: &aiapp.FailureInfo{Code: "SOURCE_HTTP_ERROR", Details: map[string]any{"receipt": map[string]any{"url": work.URL, "status": tc.status}}}}
				if err := s.DB.Create(&agentdata.Operation{RunID: child.ID, Key: "blocked-source", Kind: "inspect_source", Stage: "failed", ResultJSON: string(mustMarshal(out))}).Error; err != nil {
					t.Fatal(err)
				}
			}
			if err := s.DB.Model(&Request{}).Where("id = ?", r.ID).Updates(map[string]any{"stage": "wait_crawler", "child_run_id": child.ID, "context_json": string(mustMarshal(routeContext{URL: "https://source.example/items"}))}).Error; err != nil {
				t.Fatal(err)
			}
			if err := s.Advance(ctx, r.ID); err != nil && tc.want != "failed" {
				t.Fatal(err)
			}
			current, _ := s.Request(ctx, r.ID)
			if current.Stage != tc.want {
				t.Fatalf("Lua/Browser ownership boundary: got %s want %s", current.Stage, tc.want)
			}
		})
	}
}

func TestCompletedCrawlerCollectsCorrectedResultsAfterFailedAttempt(t *testing.T) {
	for _, name := range []string{"first-observation", "different-observation"} {
		t.Run(name, func(t *testing.T) {
			s, _ := retrievalService(t)
			ctx := context.Background()
			spec := aiapp.TaskSpec{Goal: "collect observations", SchemaVersion: "v1", RequiredFields: []aiapp.FieldRequirement{{Name: "name", Type: "string"}}, JoinKeys: []string{"name"}}
			r, err := s.Create(ctx, Input{RequestKey: name, Goal: spec.Goal, Spec: &spec})
			if err != nil {
				t.Fatal(err)
			}
			child, err := s.Crawler.Create(ctx, aiapp.CreateRunRequest{RequestKey: name, Goal: spec.Goal, Spec: &spec})
			if err != nil {
				t.Fatal(err)
			}
			for n, status := range []task.Status{task.StatusFailed, task.StatusSucceeded} {
				work := task.CrawlTask{RequestKey: fmt.Sprintf("%s-%d", name, n), RunID: child.ID, Target: "agent_program", URL: "https://source.example/items", Status: status}
				if err := s.DB.Create(&work).Error; err != nil {
					t.Fatal(err)
				}
				if err := s.DB.Create(&agentdata.RunTask{RunID: child.ID, TaskID: work.ID, OperationID: uint64(n + 1)}).Error; err != nil {
					t.Fatal(err)
				}
				if status == task.StatusSucceeded {
					rows := []aiapp.Record{{Key: name, SchemaVersion: "v1", Fields: map[string]any{"name": name}, Provenance: aiapp.Provenance{SourceID: "source", URL: work.URL, FetchedAt: time.Now(), TaskID: work.ID, DataVersion: "version-1"}}}
					if err := s.DB.Create(&repository.CrawlResult{TaskID: work.ID, Payload: string(mustMarshal(aiapp.AcquisitionResult{Records: rows}))}).Error; err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := s.DB.Model(&agentdata.Run{}).Where("id = ?", child.ID).Update("status", "completed").Error; err != nil {
				t.Fatal(err)
			}
			if err := s.DB.Model(&Request{}).Where("id = ?", r.ID).Updates(map[string]any{"stage": "wait_crawler", "child_run_id": child.ID}).Error; err != nil {
				t.Fatal(err)
			}
			if err := s.Advance(ctx, r.ID); err != nil {
				t.Fatal("failed attempt hid successful correction", err)
			}
			current, _ := s.Request(ctx, r.ID)
			var out Result
			json.Unmarshal([]byte(current.ResultJSON), &out)
			if current.Stage != "completed" || len(out.Records) != 1 || out.Records[0].Fields["name"] != name {
				t.Fatal("corrected result not delivered", current.Stage, out)
			}
		})
	}
}

type routingSearch struct{ calls int }

func (p *routingSearch) Search(context.Context, string, int) (aiapp.SearchResponse, error) {
	p.calls++
	return aiapp.SearchResponse{Candidates: []aiapp.SearchCandidate{}}, nil
}

func TestWorkflowChecksLocalThenKnownAPIThenSearchBeforeCrawler(t *testing.T) {
	s, _ := retrievalService(t)
	model := &routingOnlyModel{}
	search := &routingSearch{}
	s.Model = model
	s.Search = search
	spec := aiapp.TaskSpec{Goal: "collect public observations", SchemaVersion: "v1", RequiredFields: []aiapp.FieldRequirement{{Name: "name", Type: "string"}}}
	r, err := s.Create(context.Background(), Input{RequestKey: "ordered-routing", Goal: spec.Goal, Spec: &spec, SourceScope: []string{"source.example"}})
	if err != nil {
		t.Fatal(err)
	}
	for expected, stage := range []string{"known_api", "search_api", "start_crawler"} {
		if err := s.Advance(context.Background(), r.ID); err != nil {
			t.Fatal(err)
		}
		r, _ = s.Request(context.Background(), r.ID)
		if r.Stage != stage {
			t.Fatalf("step %d: got %s want %s", expected, r.Stage, stage)
		}
		var count int64
		s.DB.Model(&agentdata.Run{}).Count(&count)
		if count != 0 {
			t.Fatal("crawler started before API discovery completed")
		}
	}
	if model.calls != 1 || search.calls != 1 {
		t.Fatal("host order did not use the actual configured model/search adapters", model.calls, search.calls)
	}
}

func TestWorkflowFreshRAGResultCompletesWithoutCallingCrawlerOrModel(t *testing.T) {
	s, v := retrievalService(t)
	ctx := context.Background()
	model := &routingOnlyModel{}
	s.Model = model
	spec := aiapp.TaskSpec{Goal: "get local observed version", SchemaVersion: "v1", RequiredFields: []aiapp.FieldRequirement{{Name: "version", Type: "string"}}, MaxAgeSeconds: 3600}
	row := aiapp.Record{Key: "current", SchemaVersion: "v1", Fields: map[string]any{"version": "observed-v1"}, Provenance: aiapp.Provenance{SourceID: "legacy-source", URL: "https://source.example/version", FetchedAt: time.Now().Add(-time.Minute), TaskID: 601, DataVersion: "published-v1"}}
	s.DB.Create(&repository.CrawlResult{TaskID: 601, Payload: string(mustMarshal(aiapp.AcquisitionResult{Records: []aiapp.Record{row}}))})
	doc, err := knowledge.NewDocument(knowledge.Input{Source: row.Provenance.SourceID, SourceKey: "version", URL: row.Provenance.URL, Title: "version", Body: "version: observed-v1", Format: "text", FetchedAt: row.Provenance.FetchedAt, DataVersion: row.Provenance.DataVersion, TaskID: 601, RecordKey: row.Key})
	if err != nil {
		t.Fatal(err)
	}
	s.Crawler.Knowledge.Put(ctx, doc, "workflow-test")
	idx := knowledge.Indexer{Store: s.Crawler.Knowledge, Embed: testEmbed{}, Vectors: v, Fingerprint: "workflow-test"}
	if err := idx.Once(ctx); err != nil {
		t.Fatal(err)
	}
	r, err := s.Create(ctx, Input{RequestKey: "workflow-fresh", Goal: spec.Goal, Spec: &spec, SourceScope: []string{"source.example"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Advance(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	r, _ = s.Request(ctx, r.ID)
	if r.Stage != "completed" || model.calls != 0 {
		t.Fatal("fresh RAG result entered acquisition", r.Stage, model.calls)
	}
	var out Result
	if err := json.Unmarshal([]byte(r.ResultJSON), &out); err != nil || len(out.Records) != 1 || len(out.Evidence.Entries) == 0 || !out.Coverage.Satisfied {
		t.Fatal("fresh result lost records/evidence", out, err)
	}
	if err := s.Advance(ctx, r.ID); err != nil || v.queries != 1 {
		t.Fatal("completed request repeated retrieval", err, v.queries)
	}
}
