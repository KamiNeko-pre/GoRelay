package aiapp

import (
	"context"
	"encoding/json"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/knowledge"
	"go-lua-crawler/internal/repository"
	"go-lua-crawler/internal/testdb"
	"reflect"
	"testing"
	"time"
)

func TestKnowledgeReuseUsesAcquiredFieldsAndRejectsLaterInvalidation(t *testing.T) {
	db := testdb.Open(t)
	s := NewService(db)
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now()
	spec := TaskSpec{Goal: "release", SchemaVersion: "v1", RequiredFields: []FieldRequirement{{"name", "string"}, {"release_tag", "string"}, {"source_link", "string"}, {"snapshot_time", "string"}}, JoinKeys: []string{"name"}, MaxAgeSeconds: 3600}
	row := Record{Key: "old-row", SchemaVersion: "v1", Fields: map[string]any{"name": "A", "version": "1"}, Provenance: Provenance{SourceID: "s", URL: "https://example.com/a", FetchedAt: now, TaskID: 77, DataVersion: "code-v1"}}
	payload, _ := json.Marshal(AcquisitionResult{Records: []Record{row}})
	db.Create(&repository.CrawlResult{TaskID: 77, Payload: string(payload)})
	var canonicalBefore repository.CrawlResult
	if err := db.First(&canonicalBefore, "task_id = ?", row.Provenance.TaskID).Error; err != nil {
		t.Fatal(err)
	}
	doc, _ := knowledge.NewDocument(knowledge.Input{Source: "s", SourceKey: "a", Title: "A", URL: row.Provenance.URL, Body: `{"name":"A","version":"1"}`, Format: "text", FetchedAt: now, TaskID: 77, DataVersion: "code-v1", RecordKey: row.Key})
	s.Knowledge.Put(ctx, doc, "fp")
	work, _ := s.Knowledge.Claim(ctx, time.Minute)
	chunk := knowledge.SplitDocument(doc, 600, 80, work.Generation)[0]
	s.Knowledge.SaveChunk(ctx, work, chunk, []float32{1, 0})
	s.Knowledge.Publish(ctx, work, 1)
	head, _ := s.Knowledge.Head(ctx, doc.ID)
	set := knowledge.NewEvidenceSet([]knowledge.Chunk{chunk}, map[string]knowledge.Head{head.ID: head})
	r, err := s.Create(ctx, CreateRunRequest{RequestKey: "reuse", Goal: spec.Goal, Spec: &spec})
	if err != nil {
		t.Fatal(err)
	}
	r, _ = s.Data.Claim(ctx, r.ID, time.Minute)
	observation, _ := toolData(ToolCall{ID: "search", Function: FunctionCall{Name: "search_knowledge"}}, set)
	raw, _ := json.Marshal(observation)
	db.Create(&agentdata.Operation{RunID: r.ID, Key: "tool:search", InputHash: "search", Kind: "search_knowledge", Stage: "succeeded", ResultJSON: string(raw)})
	s.ReadOnly = func(ctx context.Context, c ToolCall, reader EvidenceReader) (ToolResult, error) {
		var a struct {
			EvidenceID string `json:"evidence_id"`
		}
		json.Unmarshal([]byte(c.Function.Arguments), &a)
		ev, err := reader.ReadEvidence(ctx, a.EvidenceID)
		if err != nil {
			return ToolResult{}, err
		}
		return toolData(c, ev)
	}
	ids, _ := json.Marshal(map[string]any{"evidence_ids": []string{set.Entries[0].ID}, "field_bindings": map[string]string{"release_tag": "/fields/version", "source_link": "/provenance/url", "snapshot_time": "/provenance/fetched_at"}})
	_, err = s.executeDurableTool(ctx, r, ToolCall{ID: "reuse", Type: "function", Function: FunctionCall{Name: "reuse_knowledge", Arguments: string(ids)}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.Evaluate(ctx, r)
	if err != nil || !result.Coverage.Satisfied || len(result.Records) != 1 {
		t.Fatalf("knowledge not delivered: %+v %v", result, err)
	}
	children, _ := s.Data.Children(ctx, r.ID)
	if len(children) != 0 {
		t.Fatal("reuse created acquisition child")
	}
	if result.Records[0].Fields["release_tag"] != "1" || result.Records[0].Fields["source_link"] != row.Provenance.URL || result.Records[0].Fields["snapshot_time"] != now.UTC().Format(time.RFC3339Nano) {
		t.Fatalf("projection lost original fields or acquisition time: %+v", result.Records[0])
	}
	var original repository.CrawlResult
	if err := db.First(&original, "task_id = ?", row.Provenance.TaskID).Error; err != nil || original.Payload != canonicalBefore.Payload {
		t.Fatal("projection rewrote canonical acquisition snapshot:", err)
	}
	for _, binding := range []map[string]string{
		{"not_requested": "/fields/version"},
		{"release_tag": "/provenance/task_id"},
		{"release_tag": "/fields/missing"},
		{"release_tag": "invented value"},
	} {
		args, _ := json.Marshal(map[string]any{"evidence_ids": []string{set.Entries[0].ID}, "field_bindings": binding})
		if _, err := s.executeDurableTool(ctx, r, ToolCall{ID: "bad-binding-" + agentdata.Hash(binding), Function: FunctionCall{Name: "reuse_knowledge", Arguments: string(args)}}); err == nil {
			t.Fatal("invalid field binding accepted:", binding)
		}
	}
	// Two legitimate views of the same original snapshot can fill different
	// requested columns; origin deduplication must not erase the second view.
	aliases := spec
	aliases.RequiredFields = append(append([]FieldRequirement{}, spec.RequiredFields...), FieldRequirement{"secondary_tag", "string"})
	r2, err := s.Create(ctx, CreateRunRequest{RequestKey: "reuse-views", Goal: spec.Goal, Spec: &aliases})
	if err != nil {
		t.Fatal(err)
	}
	r2, err = s.Data.Claim(ctx, r2.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&agentdata.Operation{RunID: r2.ID, Key: "tool:search", InputHash: "search", Kind: "search_knowledge", Stage: "succeeded", ResultJSON: string(raw)}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := s.executeDurableTool(ctx, r2, ToolCall{ID: "view-a", Type: "function", Function: FunctionCall{Name: "reuse_knowledge", Arguments: string(ids)}}); err != nil {
		t.Fatal(err)
	}
	secondView, _ := json.Marshal(map[string]any{"evidence_ids": []string{set.Entries[0].ID}, "field_bindings": map[string]string{"secondary_tag": "/fields/version"}})
	if _, err := s.executeDurableTool(ctx, r2, ToolCall{ID: "view-b", Type: "function", Function: FunctionCall{Name: "reuse_knowledge", Arguments: string(secondView)}}); err != nil {
		t.Fatal(err)
	}
	views, err := s.Evaluate(ctx, r2)
	if err != nil || !views.Coverage.Satisfied {
		t.Fatalf("second field projection silently erased by origin-only deduplication: %+v %v", views, err)
	}
	s.Knowledge.Invalidate(ctx, doc.ID)
	result, err = s.Evaluate(ctx, r)
	if err != nil || result.Coverage.Satisfied || len(result.ReuseIssues) == 0 {
		t.Fatal("invalidated evidence stayed in current goal coverage")
	}
}
func TestPartialCoverageReportsUsefulFreshFields(t *testing.T) {
	spec := TaskSpec{Goal: "release", SchemaVersion: "v1", RequiredFields: []FieldRequirement{{"name", "string"}, {"version", "string"}, {"license", "string"}}}
	row := Record{Key: "a", SchemaVersion: "v1", Fields: map[string]any{"name": "A", "version": "1"}, Provenance: Provenance{SourceID: "s", URL: "https://example.com", FetchedAt: time.Now(), TaskID: 1, DataVersion: "v"}}
	merged, _ := MergeRecords([]Record{row}, nil)
	facts, _ := EvaluateMergedRecords(spec, merged, time.Now())
	if facts.Satisfied || facts.PartialValidRecords != 1 || len(facts.Fields[1].Records) != 1 || len(facts.Fields[2].Records) != 0 {
		t.Fatalf("partial evidence invisible: %+v", facts)
	}
}

func TestReuseProjectionPreservesFreshnessAndCanonicalFields(t *testing.T) {
	now := time.Now().UTC()
	spec := TaskSpec{Goal: "measurement", SchemaVersion: "v1", RequiredFields: []FieldRequirement{{"name", "string"}, {"count", "integer"}, {"origin", "string"}, {"captured", "string"}}, MaxAgeSeconds: 60}
	row := Record{Key: "measurement", SchemaVersion: "v1", Fields: map[string]any{"label": "B", "value": 12}, Provenance: Provenance{SourceID: "meter", URL: "https://measure.example/b", FetchedAt: now.Add(-time.Hour), TaskID: 88, DataVersion: "original"}}
	bindings := map[string]string{"name": "/fields/label", "count": "/fields/value", "origin": "/provenance/url", "captured": "/provenance/fetched_at"}
	projected, err := projectReusedRecord(spec, row, bindings)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(projected.Provenance, row.Provenance) || projected.Key != row.Key || projected.SchemaVersion != row.SchemaVersion {
		t.Fatal("projection changed identity, acquisition time or data version")
	}
	if _, exists := row.Fields["captured"]; exists {
		t.Fatal("projection mutated original fields map")
	}
	coverage, err := EvaluateRecords(spec, []Record{projected}, now)
	if err != nil || coverage.Satisfied || len(coverage.Issues) != 1 || coverage.Issues[0].Reason != "stale acquisition" {
		t.Fatalf("projection refreshed stale data: %+v %v", coverage, err)
	}
	row.Provenance.FetchedAt = now
	projected, err = projectReusedRecord(spec, row, bindings)
	if err != nil {
		t.Fatal(err)
	}
	coverage, err = EvaluateRecords(spec, []Record{projected}, now)
	if err != nil || !coverage.Satisfied {
		t.Fatalf("fresh sibling projection rejected: %+v %v", coverage, err)
	}
}
