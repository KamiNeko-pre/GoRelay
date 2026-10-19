package aiapp

import (
	"context"
	"encoding/json"
	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/engine"
	"go-lua-crawler/internal/repository"
	task "go-lua-crawler/internal/task"
	"go-lua-crawler/internal/testdb"
	"strings"
	"testing"
)

func TestProgramContinuationMetadataSurvivesWorkerFeedback(t *testing.T) {
	for _, key := range []string{"next_cursor", "next_page"} {
		t.Run(key, func(t *testing.T) {
			s := NewService(testdb.Open(t))
			if err := s.Migrate(); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			r, err := s.Create(ctx, CreateRunRequest{RequestKey: key, Goal: "collect"})
			if err != nil {
				t.Fatal(err)
			}
			child := task.CrawlTask{RequestKey: "child", RunID: r.ID, Status: task.StatusSucceeded}
			if err := s.Data.DB.Create(&child).Error; err != nil {
				t.Fatal(err)
			}
			link := agentdata.RunTask{RunID: r.ID, TaskID: child.ID, OperationID: 1}
			if err := s.Data.DB.Create(&link).Error; err != nil {
				t.Fatal(err)
			}
			payload, _ := json.Marshal(AcquisitionResult{Records: []Record{{Key: "fact", Fields: map[string]any{"title": "A"}}}, Runtime: engine.ProgramResult{Collection: map[string]any{"items": []any{map[string]any{"title": "A"}}, key: "opaque-source-continuation", "has_more": true}, SourceEvidence: []acquisition.Response{{Body: "raw-html-must-stay-on-host"}}}})
			if err := s.Data.DB.Create(&repository.CrawlResult{TaskID: child.ID, Payload: string(payload)}).Error; err != nil {
				t.Fatal(err)
			}
			out, err := s.dispatchTool(ctx, r, ToolCall{ID: "results", Type: "function", Function: FunctionCall{Name: "get_task_results", Arguments: `{}`}})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(out.Data), "opaque-source-continuation") {
				t.Fatal("program continuation discarded before the next Agent decision", string(out.Data))
			}
			if strings.Contains(string(out.Data), "raw-html-must-stay-on-host") {
				t.Fatal("worker feedback exposed raw source evidence")
			}
			var got struct {
				Records []Record `json:"records"`
			}
			json.Unmarshal(out.Data, &got)
			if len(got.Records) != 1 || got.Records[0].Fields[key] != nil {
				t.Fatal("operational metadata was promoted to acquired facts")
			}
		})
	}
}
