package aiapp

import (
	"context"
	"encoding/json"
	"errors"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/knowledge"
	"go-lua-crawler/internal/repository"
	"go-lua-crawler/internal/testdb"
	"testing"
	"time"
)

// Model text is a fixture; SQL, evidence validity and the terminal loop are real.
func TestTaskSummaryPersistsThroughLoopAndRejectsInvalidReferences(t *testing.T) {
	for _, topic := range []string{"release comparison", "employment research"} {
		t.Run(topic, func(t *testing.T) {
			ctx := context.Background()
			s := NewService(testdb.Open(t))
			if err := s.Migrate(); err != nil {
				t.Fatal(err)
			}
			spec := TaskSpec{Goal: topic, SchemaVersion: "v1", RequiredFields: []FieldRequirement{{"name", "string"}}}
			r, err := s.Create(ctx, CreateRunRequest{RequestKey: "summary", Goal: topic, Spec: &spec, RequireSummary: true})
			if err != nil {
				t.Fatal(err)
			}
			r, err = s.Data.Claim(ctx, r.ID, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			row := Record{Key: "source-row", SchemaVersion: "v1", Fields: map[string]any{"name": "A"}, Provenance: Provenance{SourceID: "public", URL: "https://example.com/report", FetchedAt: now, TaskID: 77, DataVersion: "v1"}}
			payload, _ := json.Marshal(AcquisitionResult{Records: []Record{row}})
			if err := s.Data.DB.Create(&repository.CrawlResult{TaskID: 77, Payload: string(payload)}).Error; err != nil {
				t.Fatal(err)
			}
			doc, err := knowledge.NewDocument(knowledge.Input{Source: "public", SourceKey: "report", Title: topic, URL: row.Provenance.URL, Body: "A has an observed result; earlier years are missing.", Format: "text", FetchedAt: now, TaskID: 77, DataVersion: "v1", RecordKey: row.Key})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Knowledge.Put(ctx, doc, "fp"); err != nil {
				t.Fatal(err)
			}
			work, err := s.Knowledge.Claim(ctx, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			chunk := knowledge.SplitDocument(doc, 600, 80, work.Generation)[0]
			if err := s.Knowledge.SaveChunk(ctx, work, chunk, []float32{1, 0}); err != nil {
				t.Fatal(err)
			}
			if err := s.Knowledge.Publish(ctx, work, 1); err != nil {
				t.Fatal(err)
			}
			head, _ := s.Knowledge.Head(ctx, doc.ID)
			set := knowledge.NewEvidenceSet([]knowledge.Chunk{chunk}, map[string]knowledge.Head{head.ID: head})
			observed, _ := toolData(ToolCall{ID: "search", Function: FunctionCall{Name: "search_knowledge"}}, set)
			raw, _ := json.Marshal(observed)
			if err := s.Data.DB.Create(&agentdata.Operation{RunID: r.ID, Key: "tool:search", InputHash: "search", Kind: "search_knowledge", Stage: "succeeded", ResultJSON: string(raw)}).Error; err != nil {
				t.Fatal(err)
			}
			call := func(id string, args any) ToolCall {
				raw, _ := json.Marshal(args)
				return ToolCall{ID: id, Type: "function", Function: FunctionCall{Name: "finish_task", Arguments: string(raw)}}
			}
			ids := []string{set.Entries[0].ID}
			other, err := s.Create(ctx, CreateRunRequest{RequestKey: "other-run", Goal: topic, Spec: &spec, RequireSummary: true})
			if err != nil {
				t.Fatal(err)
			}
			other, err = s.Data.Claim(ctx, other.ID, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.executeDurableTool(ctx, other, call("cross-run", map[string]any{"status": "completed", "reason": "done", "summary": "A has an observed result", "citations": ids})); !errors.Is(err, knowledge.ErrInvalidReference) {
				t.Fatalf("real evidence from another Run was accepted: %v", err)
			}
			progressRaw, _ := json.Marshal(TaskProgress{Version: 1, Requirements: []RequirementResult{{ID: "scope", Status: "satisfied", Explanation: "Supported by the current evidence", Citations: ids}}})
			r.ProgressJSON = string(progressRaw)
			progress, err := s.currentProgress(ctx, r)
			if err != nil || progress.Requirements[0].Status != "satisfied" {
				t.Fatalf("current evidence was downgraded: %+v %v", progress, err)
			}
			if _, err := s.executeDurableTool(ctx, r, call("missing-summary", map[string]string{"status": "completed", "reason": "fields present"})); err == nil {
				t.Fatal("structural fields alone completed a required-summary goal")
			}
			if _, err := s.executeDurableTool(ctx, r, call("declared-gap", map[string]any{"status": "completed", "reason": "done", "summary": "A has an observed result", "citations": ids, "gaps": []string{"Earlier years missing"}})); err == nil {
				t.Fatal("declared gaps accepted as completed")
			}
			limited, err := s.executeDurableTool(ctx, r, call("disclosed-limit", map[string]any{"status": "completed", "reason": "requested comparison delivered within stated evidence scope", "summary": "A has an observed result; sample and time coverage are explicitly limited.", "citations": ids, "limitations": []string{"No continuous annual series was available; no values were interpolated."}}))
			if err != nil {
				t.Fatalf("permitted disclosed limitation blocked delivery: %v", err)
			}
			var limitationReceipt struct {
				Limitations []string `json:"limitations"`
			}
			if err = json.Unmarshal(limited.Data, &limitationReceipt); err != nil || len(limitationReceipt.Limitations) != 1 {
				t.Fatal("limitation discarded", err)
			}
			reuse, _ := json.Marshal(map[string]any{"evidence_ids": ids})
			if _, err := s.executeDurableTool(ctx, r, ToolCall{ID: "reuse", Type: "function", Function: FunctionCall{Name: "reuse_knowledge", Arguments: string(reuse)}}); err != nil {
				t.Fatal(err)
			}
			valid := map[string]any{"status": "partial", "reason": "historical coverage missing", "summary": "A has an observed result.", "citations": ids, "gaps": []string{"Earlier years missing"}}
			bad := map[string]any{"status": "completed", "reason": "done", "summary": "Unsupported", "citations": []string{"ev_foreign"}}
			if _, err := s.executeDurableTool(ctx, r, call("foreign", bad)); !errors.Is(err, knowledge.ErrInvalidReference) {
				t.Fatalf("foreign citation not rejected: %v", err)
			}
			out, err := s.executeDurableTool(ctx, r, call("finish", valid))
			if err != nil {
				t.Fatal(err)
			}
			raw, _ = json.Marshal(out)
			plan, _ := json.Marshal([]Message{{Role: "tool", ToolCallID: "finish", Content: string(raw)}})
			if err := s.Data.SaveRun(ctx, r.ID, r.Token, map[string]any{"status": "reviewing", "plan_json": string(plan)}); err != nil {
				t.Fatal(err)
			}
			// Recreate the service: summary must come from the durable receipt.
			s = NewService(s.Data.DB)
			if err := s.Advance(ctx, r.ID); err != nil {
				t.Fatal(err)
			}
			stored, err := s.Data.Run(ctx, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			var delivered struct {
				Summary string
				Gaps    []string
				Answer  *RAGAnswer
			}
			if err := json.Unmarshal([]byte(stored.ResultJSON), &delivered); err != nil {
				t.Fatal(err)
			}
			if stored.Status != "partial" || delivered.Summary != valid["summary"] || len(delivered.Gaps) != 1 || delivered.Answer == nil || len(delivered.Answer.Evidence.Entries) != 1 || delivered.Answer.SemanticSupportChecked {
				t.Fatalf("lost summary or falsely claimed semantics: %s %+v", stored.Status, delivered)
			}
			if err := s.Knowledge.Invalidate(ctx, doc.ID); err != nil {
				t.Fatal(err)
			}
			progress, err = s.currentProgress(ctx, r)
			if err != nil || progress.Requirements[0].Status != "missing" || len(progress.Requirements[0].Citations) != 0 || r.ProgressJSON != string(progressRaw) {
				t.Fatalf("invalid evidence remained satisfied or historical checkpoint changed: %+v %v", progress, err)
			}
			current, err := s.Evaluate(ctx, stored)
			if err != nil {
				t.Fatal(err)
			}
			if current.Summary != "" || len(current.Gaps) == 0 {
				t.Fatal("stale summary exposed as current")
			}
			// Immutable historical delivery remains readable after invalidation.
			history, _ := s.Data.Run(ctx, r.ID)
			if history.ResultJSON != stored.ResultJSON {
				t.Fatal("historical answer was rewritten")
			}
		})
	}
}

func TestSummaryCompletionGatePreservesStructuredGoals(t *testing.T) {
	state := decisionState()
	state.Spec = &TaskSpec{Goal: state.Run.Goal}
	state.Result = &TaskResult{Coverage: CoverageReport{Satisfied: true, ValidRecords: 1}, SummaryRequired: true}
	out, _ := toolData(ToolCall{ID: "finish", Function: FunctionCall{Name: "finish_task"}}, map[string]string{"status": "completed", "reason": "done"})
	raw, _ := json.Marshal(out)
	state.Messages = []Message{{Role: "tool", ToolCallID: "finish", Content: string(raw)}}
	if next, err := RunTaskLoop(context.Background(), state); err != nil || next.Kind == "finish" {
		t.Fatalf("required summary ignored: %+v %v", next, err)
	}
	state.Result.SummaryRequired = false
	if next, err := RunTaskLoop(context.Background(), state); err != nil || next.Kind != "finish" {
		t.Fatalf("structured-only goal regressed: %+v %v", next, err)
	}
}
