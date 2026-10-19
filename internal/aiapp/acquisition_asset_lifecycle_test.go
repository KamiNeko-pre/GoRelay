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
	"go-lua-crawler/internal/testdb"
	"gorm.io/gorm"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPartialKnowledgeProjectionPreservesOtherFieldsAndSources(t *testing.T) {
	for _, label := range []string{"release", "opportunity"} {
		t.Run(label, func(t *testing.T) {
			s := NewService(testdb.Open(t))
			if err := s.Migrate(); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			r, err := s.Create(ctx, CreateRunRequest{RequestKey: label, Goal: "collect"})
			if err != nil {
				t.Fatal(err)
			}
			base := time.Now().UTC().Truncate(time.Second)
			put := func(fields map[string]any, url string, at time.Time) {
				spec := TaskSpec{SchemaVersion: "v1", JoinKeys: []string{"title"}, SourceNamespace: label}
				for key := range fields {
					spec.RequiredFields = append(spec.RequiredFields, FieldRequirement{Name: key, Type: "string"})
				}
				params, _ := json.Marshal(childParameters{Spec: spec})
				child := task.CrawlTask{RunID: r.ID, RequestKey: agentdata.Token(), ProgramParams: string(params)}
				if err := s.Data.DB.Create(&child).Error; err != nil {
					t.Fatal(err)
				}
				record := Record{Key: "entity", SchemaVersion: "v1", Fields: fields, Provenance: Provenance{SourceID: "composition", URL: url, FetchedAt: at, TaskID: child.ID}}
				if at.Equal(base.Add(2 * time.Minute)) {
					record.Provenance.SourceID = "new-api-capability"
				}
				if err := s.Data.DB.Transaction(s.persistAcquisition(child, AcquisitionResult{Records: []Record{record}})); err != nil {
					t.Fatal(err)
				}
			}
			url := "https://one.example/" + label
			put(map[string]any{"title": label, "body": "original complete facts"}, url, base)
			put(map[string]any{"title": label}, url, base.Add(time.Minute))
			// Same join value at an independent source cannot overwrite either fragment.
			put(map[string]any{"title": label, "body": "other source facts"}, "https://two.example/"+label, base.Add(time.Minute))
			heads, err := s.Knowledge.Heads(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(heads) != 3 {
				t.Fatalf("field fragments/source identities collapsed: %d", len(heads))
			}
			var original knowledge.Head
			for _, head := range heads {
				doc, err := s.Knowledge.Document(ctx, head.ID, head.Revision)
				if err != nil {
					t.Fatal(err)
				}
				if doc.Body == "original complete facts" {
					original = head
				}
			}
			if original.ID == "" || original.FetchedAt == nil || !original.FetchedAt.Equal(base) {
				t.Fatal("old full text was lost or falsely refreshed")
			}
			// A full refresh with the same field profile must update, not create another identity.
			put(map[string]any{"body": "updated complete facts", "title": label}, url, base.Add(2*time.Minute))
			heads, _ = s.Knowledge.Heads(ctx)
			current, _ := s.Knowledge.Head(ctx, original.ID)
			doc, _ := s.Knowledge.Document(ctx, current.ID, current.Revision)
			if len(heads) != 3 || doc.Body != "updated complete facts" {
				t.Fatal("normal full refresh stopped updating its fragment")
			}
		})
	}
}

func TestWorkerExecutesItsBoundVersionAcrossPublication(t *testing.T) {
	for _, when := range []string{"before-execute", "during-execute"} {
		t.Run(when, func(t *testing.T) {
			s := NewService(testdb.Open(t))
			if err := s.Migrate(); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			spec := TaskSpec{Goal: "collect", SchemaVersion: "v1", RequiredFields: []FieldRequirement{{Name: "name", Type: "string"}}, RequirePublishedCapability: true}
			r, err := s.Create(ctx, CreateRunRequest{RequestKey: when, Goal: "collect", Spec: &spec})
			if err != nil {
				t.Fatal(err)
			}
			schema, _ := json.Marshal(spec)
			a, _ := s.Data.SaveArtifact(ctx, agentdata.ProgramArtifact{Code: "first code", Kind: "adapter", OutputSchema: string(schema), ScopeJSON: `["example.com"]`})
			b, _ := s.Data.SaveArtifact(ctx, agentdata.ProgramArtifact{Code: "next code", Kind: "adapter", OutputSchema: string(schema), ScopeJSON: `["example.com"]`})
			if _, err := s.Data.Publish(ctx, agentdata.SourceVersion{SourceID: "source", ArtifactHash: a.Hash}, "", 0); err != nil {
				t.Fatal(err)
			}
			publish := func() {
				if _, err := s.Data.Publish(ctx, agentdata.SourceVersion{SourceID: "source", ArtifactHash: b.Hash}, a.Hash, 1); err != nil {
					t.Fatal(err)
				}
			}
			if when == "before-execute" {
				publish()
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				var input RunnerRequest
				if err := json.NewDecoder(req.Body).Decode(&input); err != nil {
					t.Error(err)
				}
				if input.Program.Code != a.Code {
					t.Error("in-flight task silently switched program versions")
				}
				if when == "during-execute" {
					publish()
				}
				json.NewEncoder(w).Encode(RunnerResponse{Result: &engine.ProgramResult{Collection: map[string]any{"items": []any{map[string]any{"name": "A", "source_url": "https://example.com/item"}}}, Requests: []acquisition.Receipt{{URL: "https://example.com/item", Method: http.MethodGet, Status: 200, FetchedAt: time.Now()}}}})
			}))
			defer server.Close()
			s.Runner, _ = NewRunnerClient(server.URL, "fixture")
			params, _ := json.Marshal(childParameters{Spec: spec, SourceID: "source"})
			child := task.CrawlTask{RunID: r.ID, RequestKey: "bound", URL: "https://example.com/item", ProgramHash: a.Hash, ProgramParams: string(params)}
			if err := s.Data.DB.Create(&child).Error; err != nil {
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
			if err := s.Data.DB.Create(&agentdata.RunTask{RunID: r.ID, TaskID: child.ID, OperationID: 1, SourceID: "source", ArtifactHash: a.Hash}).Error; err != nil {
				t.Fatal(err)
			}
			result, err := s.Evaluate(ctx, r)
			if err != nil || !taskDeliverySatisfied(&result) {
				t.Fatalf("valid committed old-version result lost at final delivery: %+v %v", result, err)
			}
			if err := s.Data.DB.Model(&agentdata.SourceVersion{}).Where("source_id = ? AND artifact_hash = ?", "source", a.Hash).Update("status", "quarantined").Error; err != nil {
				t.Fatal(err)
			}
			result, err = s.Evaluate(ctx, r)
			if err != nil || result.Coverage.Satisfied {
				t.Fatalf("revoked version still contributes to completion: %+v %v", result, err)
			}
		})
	}
}

func TestBoundPublishedVersionSurvivesNewHeadButNotRevocation(t *testing.T) {
	for _, name := range []string{"new-head", "revoked-version", "quarantined-source"} {
		t.Run(name, func(t *testing.T) {
			s := NewService(testdb.Open(t))
			if err := s.Migrate(); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			r, err := s.Create(ctx, CreateRunRequest{RequestKey: name, Goal: "collect"})
			if err != nil {
				t.Fatal(err)
			}
			a, _ := s.Data.SaveArtifact(ctx, agentdata.ProgramArtifact{Code: "first", Kind: "adapter"})
			b, _ := s.Data.SaveArtifact(ctx, agentdata.ProgramArtifact{Code: "next", Kind: "adapter"})
			if _, err := s.Data.Publish(ctx, agentdata.SourceVersion{SourceID: "source", ArtifactHash: a.Hash}, "", 0); err != nil {
				t.Fatal(err)
			}
			params, _ := json.Marshal(childParameters{SourceID: "source"})
			child := task.CrawlTask{RunID: r.ID, RequestKey: "bound", ProgramHash: a.Hash, ProgramParams: string(params)}
			if err := s.Data.DB.Create(&child).Error; err != nil {
				t.Fatal(err)
			}
			if _, err := s.Data.Publish(ctx, agentdata.SourceVersion{SourceID: "source", ArtifactHash: b.Hash}, a.Hash, 1); err != nil {
				t.Fatal(err)
			}
			if name == "revoked-version" {
				s.Data.DB.Model(&agentdata.SourceVersion{}).Where("source_id = ? AND artifact_hash = ?", "source", a.Hash).Update("status", "quarantined")
			}
			if name == "quarantined-source" {
				if err := s.Data.Quarantine(ctx, "source", b.Hash, "revoked"); err != nil {
					t.Fatal(err)
				}
			}
			record := Record{Key: "entity", SchemaVersion: "v1", Fields: map[string]any{"body": "valid old-version result"}, Provenance: Provenance{SourceID: "source", URL: "https://example.com/item", FetchedAt: time.Now(), DataVersion: a.Hash, TaskID: child.ID}}
			err = s.Data.DB.Transaction(func(tx *gorm.DB) error {
				return s.persistAcquisition(child, AcquisitionResult{Records: []Record{record}})(tx)
			})
			if name == "new-head" && err != nil {
				t.Fatalf("in-flight immutable version rejected merely because head advanced: %v", err)
			}
			if name != "new-head" && !errors.Is(err, agentdata.ErrConflict) {
				t.Fatalf("revoked version/source accepted: %v", err)
			}
		})
	}
}
