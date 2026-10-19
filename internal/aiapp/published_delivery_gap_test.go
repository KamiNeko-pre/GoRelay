package aiapp

import (
	"context"
	"errors"
	"go-lua-crawler/internal/agentdata"
	task "go-lua-crawler/internal/task"
	"go-lua-crawler/internal/testdb"
	"testing"
	"time"
)

func TestPublishedSettledIncompleteDeliveryReturnsToCorrection(t *testing.T) {
	for _, tc := range []struct{ name, code string }{{"empty", `return true,{items={}}`}, {"insufficient_count", `local b=assert(http_get(TARGET_URL));return true,{items={{title="Only title",source_url=TARGET_URL}}}`}} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewService(testdb.Open(t))
			if err := s.Migrate(); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			probeFeedbackProxy(t, `<main><h2>Only title</h2></main>`)
			s.Runner = actualRunnerForChain(t)
			spec := TaskSpec{Goal: "collect title and source URL", ContractVersion: "v2", SchemaVersion: "v1", RequiredFields: []FieldRequirement{{Name: "title", Type: "string"}, {Name: "source_url", Type: "string"}}, JoinKeys: []string{"title"}, MinRecords: 2}
			artifact, err := s.Data.SaveArtifact(ctx, agentdata.ProgramArtifact{Code: tc.code, Kind: "adapter", OutputSchema: string(mustJSON(spec)), InputSchema: `{"type":"object","properties":{},"additionalProperties":false}`, ScopeJSON: `["8.8.8.8"]`})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.Data.Publish(ctx, agentdata.SourceVersion{SourceID: "published-incomplete", ArtifactHash: artifact.Hash}, "", 0); err != nil {
				t.Fatal(err)
			}
			r, err := s.StartPublishedAcquisition(ctx, CreateRunRequest{RequestKey: tc.name, Goal: spec.Goal, Spec: &spec, SourceScope: []string{"8.8.8.8"}}, "published-incomplete", artifact.Hash, "http://8.8.8.8/detail", nil, time.Now().Add(time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.CompletePublishedAcquisition(ctx, r.ID); !errors.Is(err, ErrPublishedPending) {
				t.Fatal("queued work must remain pending", err)
			}
			links, _ := s.Data.Children(ctx, r.ID)
			var child task.CrawlTask
			if err = s.Data.DB.First(&child, "id = ?", links[0].TaskID).Error; err != nil {
				t.Fatal(err)
			}
			persist, err := s.ExecuteTask(ctx, child)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.Data.DB.Transaction(persist); err != nil {
				t.Fatal(err)
			}
			s.Data.DB.Model(&child).Update("status", task.StatusSucceeded)
			if _, err = s.CompletePublishedAcquisition(ctx, r.ID); err == nil || errors.Is(err, ErrPublishedPending) {
				t.Fatal("settled incomplete task incorrectly treated as in-flight", err)
			}
			resumed, err := s.ResumePublishedCorrection(ctx, r.ID)
			if err != nil || resumed.ID != r.ID || resumed.Status != "reviewing" || resumed.SpecJSON != r.SpecJSON {
				t.Fatal("incomplete delivery cannot return original Run to correction", resumed.Status, err)
			}
		})
	}
}
