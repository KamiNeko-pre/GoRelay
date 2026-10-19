package aiapp

import (
	"context"
	"encoding/json"
	"errors"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/knowledge"
	"go-lua-crawler/internal/testdb"
	"strings"
	"testing"
)

func TestHostEvidenceCatalogMatchesCurrentCitationValidity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change map[string]any
	}{
		{"new_revision", map[string]any{"revision": "revision-2"}},
		{"new_generation", map[string]any{"generation": "generation-2"}},
		{"deactivated", map[string]any{"active": false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s := NewService(testdb.Open(t))
			if err := s.Migrate(); err != nil {
				t.Fatal(err)
			}
			r, err := s.Create(ctx, CreateRunRequest{RequestKey: tc.name, Goal: "summarize current public evidence"})
			if err != nil {
				t.Fatal(err)
			}
			set := knowledge.EvidenceSet{RequestID: r.ID, Entries: []knowledge.Evidence{
				{ID: "ev_superseded", DocID: "changed", Revision: "revision-1", Generation: "generation-1", ChunkID: "chunk-changed"},
				{ID: "ev_unchanged", DocID: "stable", Revision: "stable-revision", Generation: "stable-generation", ChunkID: "chunk-stable"},
			}}
			for _, e := range set.Entries {
				if err := s.Knowledge.DB.Create(&knowledge.Head{ID: e.DocID, Revision: e.Revision, Generation: e.Generation, Active: true}).Error; err != nil {
					t.Fatal(err)
				}
			}
			// A foreign document being present in SQL is not enough to advertise it.
			if err := s.Knowledge.DB.Create(&knowledge.Head{ID: "foreign", Revision: "f", Generation: "f", Active: true}).Error; err != nil {
				t.Fatal(err)
			}
			out, _ := toolData(ToolCall{ID: "retrieved", Function: FunctionCall{Name: "search_knowledge"}}, set)
			raw, _ := json.Marshal(out)
			op := agentdata.Operation{RunID: r.ID, Key: "tool:retrieved", Kind: "search_knowledge", Stage: "succeeded", ResultJSON: string(raw)}
			if err := s.Data.DB.Create(&op).Error; err != nil {
				t.Fatal(err)
			}
			before, err := s.state(ctx, r)
			if err != nil || len(before.EvidenceCatalog) != 2 {
				t.Fatalf("valid initial catalog lost: %+v %v", before.EvidenceCatalog, err)
			}
			if err := s.Knowledge.DB.Model(&knowledge.Head{}).Where("id = ?", "changed").Updates(tc.change).Error; err != nil {
				t.Fatal(err)
			}
			reader := knowledge.SetReader{Store: s.Knowledge, Set: set}
			if _, err := reader.ReadEvidence(ctx, "ev_superseded"); !errors.Is(err, knowledge.ErrInvalidReference) {
				t.Fatalf("stale citation accepted: %v", err)
			}
			if _, err := reader.ReadEvidence(ctx, "ev_unchanged"); err != nil {
				t.Fatal(err)
			}
			model := &capabilityRequestChat{}
			s.Model = model
			if err := s.Advance(ctx, r.ID); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, m := range model.request.Messages {
				if m.Role != "system" || !strings.HasPrefix(m.Content, "Persisted host facts") {
					continue
				}
				found = true
				var facts struct {
					EvidenceCatalog []map[string]any `json:"evidence_catalog"`
				}
				if err := json.Unmarshal([]byte(m.Content[strings.Index(m.Content, "{"):]), &facts); err != nil {
					t.Fatal(err)
				}
				if len(facts.EvidenceCatalog) != 1 || facts.EvidenceCatalog[0]["id"] != "ev_unchanged" {
					t.Fatalf("model catalog contradicts citation validator: %+v", facts.EvidenceCatalog)
				}
			}
			if !found {
				t.Fatal("model received no host snapshot")
			}
			var saved agentdata.Operation
			if err := s.Data.DB.First(&saved, op.ID).Error; err != nil || saved.ResultJSON != string(raw) {
				t.Fatal("historical retrieval receipt rewritten", err)
			}
		})
	}
}
