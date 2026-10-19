package aiapp

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"go-lua-crawler/internal/agentdata"
	task "go-lua-crawler/internal/task"
	"go-lua-crawler/internal/testdb"
)

func TestPublishedFailureReturnsSameRunToNormalLuaCorrection(t *testing.T) {
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	spec := TaskSpec{Goal: "collect supported fields", SchemaVersion: "v1", RequiredFields: []FieldRequirement{{Name: "name", Type: "string"}}}
	r, err := s.Create(ctx, CreateRunRequest{RequestKey: "resume-published-repair", Goal: spec.Goal, Spec: &spec})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Data.DB.Model(&agentdata.Run{}).Where("id = ?", r.ID).Updates(map[string]any{"status": "running", "next_wake_at": time.Now().Add(time.Minute)}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResumePublishedCorrection(ctx, r.ID); err == nil {
		t.Fatal("a pending execution cannot trigger repair")
	}
	work := task.CrawlTask{RequestKey: "failed-published", RunID: r.ID, Target: "agent_program", URL: "https://example.test/items", Status: task.StatusFailed}
	if err := s.Data.DB.Create(&work).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.Data.DB.Create(&agentdata.RunTask{RunID: r.ID, TaskID: work.ID, OperationID: 21}).Error; err != nil {
		t.Fatal(err)
	}
	resumed, err := s.ResumePublishedCorrection(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.ID != r.ID || resumed.Status != "reviewing" || resumed.SpecJSON != r.SpecJSON {
		t.Fatal("published failure lost original task/contract", resumed)
	}
	pending, err := s.Data.Pending(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range pending {
		found = found || p.ID == r.ID
	}
	if !found {
		t.Fatal("normal Lua scheduler cannot continue original correction")
	}
	links, _ := s.Data.Children(ctx, r.ID)
	if len(links) != 1 || links[0].TaskID != work.ID {
		t.Fatal("repair created a replacement task or erased prior evidence")
	}
}

func TestPublishedEntryKeepsCrawlerExecutionAndImmutableReuse(t *testing.T) {
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	probeFeedbackProxy(t, `<main><h2>Observed asset value</h2></main>`)
	s.Runner = actualRunnerForChain(t)
	spec := TaskSpec{Goal: "collect published asset value", ContractVersion: "v2", SchemaVersion: "v1", RequiredFields: []FieldRequirement{{Name: "title", Type: "string"}, {Name: "source_url", Type: "string"}}, JoinKeys: []string{"title"}, RequirePublishedCapability: true, RequireFieldSupport: true}
	code := `local b=assert(http_get(TARGET_URL));return true,{items={{title=assert(html_find(b,"h2")),source_url=TARGET_URL,_evidence={title={selector="h2",index=0}}}}}`
	artifact, err := s.Data.SaveArtifact(ctx, agentdata.ProgramArtifact{Code: code, Kind: "adapter", OutputSchema: string(mustJSON(spec)), InputSchema: `{"type":"object","properties":{},"additionalProperties":false}`, ScopeJSON: `["8.8.8.8"]`})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Data.Publish(ctx, agentdata.SourceVersion{SourceID: "published-title", ArtifactHash: artifact.Hash}, "", 0); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"independent-a", "independent-b"} {
		r, err := s.StartPublishedAcquisition(ctx, CreateRunRequest{RequestKey: key, Goal: spec.Goal, Spec: &spec, SourceScope: []string{"8.8.8.8"}}, "published-title", artifact.Hash, "http://8.8.8.8/detail", nil, time.Now().Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		again, err := s.StartPublishedAcquisition(ctx, CreateRunRequest{RequestKey: key, Goal: spec.Goal, Spec: &spec, SourceScope: []string{"8.8.8.8"}}, "published-title", artifact.Hash, "http://8.8.8.8/detail", nil, r.Deadline)
		if err != nil || again.ID != r.ID {
			t.Fatal("resume created a different execution", err)
		}
		links, err := s.Data.Children(ctx, r.ID)
		if err != nil || len(links) != 1 {
			t.Fatal("published asset did not submit exactly one normal child", links, err)
		}
		pending, err := s.Data.Pending(ctx, 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range pending {
			if p.ID == r.ID {
				t.Fatal("independently managed asset run reached the model scheduler")
			}
		}
		var child task.CrawlTask
		if err := s.Data.DB.First(&child, "id = ?", links[0].TaskID).Error; err != nil {
			t.Fatal(err)
		}
		persist, err := s.ExecuteTask(ctx, child)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Data.DB.Transaction(persist); err != nil {
			t.Fatal(err)
		}
		if err := s.Data.DB.Model(&child).Update("status", task.StatusSucceeded).Error; err != nil {
			t.Fatal(err)
		}
		completed, err := s.CompletePublishedAcquisition(ctx, r.ID)
		if err != nil || completed.Status != "completed" {
			t.Fatal("actual result could not complete published reuse", completed.Status, err)
		}
		var result TaskResult
		json.Unmarshal([]byte(completed.ResultJSON), &result)
		if !taskDeliverySatisfied(&result) || result.ReusableCapabilities != 1 {
			t.Fatal("publication/evidence checks were bypassed", result)
		}
	}
	if _, err := s.StartPublishedAcquisition(ctx, CreateRunRequest{RequestKey: "wrong-version", Goal: spec.Goal, Spec: &spec, SourceScope: []string{"8.8.8.8"}}, "published-title", "wrong-version", "http://8.8.8.8/detail", nil, time.Now().Add(time.Minute)); err == nil {
		t.Fatal("wrong pinned version was accepted")
	}
}
