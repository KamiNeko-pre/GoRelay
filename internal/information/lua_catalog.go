package information

import (
	"context"
	"encoding/json"

	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/knowledge"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// SyncLuaCatalog projects existing published programs into the information
// catalog and semantic index. It never writes source_heads, source_versions,
// program_artifacts or the crawler's state. Revocation flows one way.
func (s *Service) SyncLuaCatalog(ctx context.Context, requestIDs ...string) error {
	sources, err := s.Crawler.Data.ListSources(ctx)
	if err != nil {
		return err
	}
	live := map[string]bool{}
	for _, source := range sources {
		program, err := s.Crawler.Data.Artifact(ctx, source.ArtifactHash)
		if err != nil {
			return err
		}
		a := Asset{ID: agentdata.Hash([]string{"lua", source.SourceID, source.ArtifactHash}), Kind: "lua", SourceID: source.SourceID, Version: source.ArtifactHash, Description: source.Description, SpecJSON: program.OutputSchema, ScopeJSON: program.ScopeJSON, ManifestJSON: string(mustMarshal(map[string]any{"source_id": source.SourceID, "artifact_hash": source.ArtifactHash, "input_schema": program.InputSchema, "input_example": program.InputExampleJSON})), ProofJSON: source.ProofJSON, Active: true, VerifiedAt: source.CreatedAt}
		var example struct {
			URL       string `json:"url"`
			TargetURL string `json:"target_url"`
		}
		json.Unmarshal([]byte(program.InputExampleJSON), &example)
		a.EntryURL = example.URL
		if a.EntryURL == "" {
			a.EntryURL = example.TargetURL
		}
		live[a.ID] = true
		if err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&a).Error; err != nil {
				return err
			}
			return s.indexAsset(tx, a, requestIDs...)
		}); err != nil {
			return err
		}
	}
	var derived []Asset
	if err := s.DB.WithContext(ctx).Where("kind = ? AND active = ?", "lua", true).Find(&derived).Error; err != nil {
		return err
	}
	for _, a := range derived {
		if live[a.ID] {
			continue
		}
		if err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if err := tx.Model(&Asset{}).Where("id = ?", a.ID).Update("active", false).Error; err != nil {
				return err
			}
			var heads []knowledge.Head
			if err := tx.Where("source = ? AND record_key = ?", "information-capability", a.ID).Find(&heads).Error; err != nil {
				return err
			}
			for _, h := range heads {
				if err := knowledge.NewStore(tx).Invalidate(ctx, h.ID); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}
