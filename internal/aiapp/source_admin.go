package aiapp

import (
	"context"
	"fmt"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/knowledge"
	"go-lua-crawler/internal/repository"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"os"
)

type SourceCapability struct {
	Source           agentdata.SourceVersion `json:"source"`
	Kind             string                  `json:"kind"`
	PolicyVersion    string                  `json:"policy_version"`
	InputSchema      string                  `json:"input_schema"`
	InputExampleJSON string                  `json:"input_example_json"`
	OutputSchema     string                  `json:"output_schema"`
	ScopeJSON        string                  `json:"scope_json"`
	DirectUseIssue   string                  `json:"direct_use_issue,omitempty"`
}
type LegacyCapability struct {
	SourceID    string `json:"source_id"`
	Description string `json:"description"`
	URL         string `json:"url"`
	OutputKey   string `json:"output_key"`
	State       string `json:"state"`
}

func legacyCatalog() ([]LegacyCapability, error) {
	rules, err := repository.ListRuleDefinitions()
	if err != nil {
		return nil, err
	}
	out := []LegacyCapability{}
	for _, r := range rules {
		out = append(out, LegacyCapability{SourceID: r.ID, Description: r.Name, URL: r.URL, OutputKey: r.ItemKey, State: "trusted_legacy_requires_public_typed_trial"})
	}
	return out, nil
}
func (s *Service) capabilities(ctx context.Context) ([]SourceCapability, error) {
	rows, err := s.Data.ListSources(ctx)
	if err != nil {
		return nil, err
	}
	out := []SourceCapability{}
	for _, row := range rows {
		a, err := s.Data.Artifact(ctx, row.ArtifactHash)
		if err != nil {
			return nil, err
		}
		out = append(out, SourceCapability{Source: row, Kind: a.Kind, PolicyVersion: a.PolicyVersion, InputSchema: a.InputSchema, InputExampleJSON: a.InputExampleJSON, OutputSchema: a.OutputSchema, ScopeJSON: a.ScopeJSON})
	}
	return out, nil
}
func (s *Service) sourceCode(ctx context.Context, id string) (any, error) {
	h, err := s.Data.SourceHead(ctx, id)
	if err == nil {
		a, err := s.Data.Artifact(ctx, h.ArtifactHash)
		return map[string]any{"head": h, "artifact": a}, err
	}
	if err != gorm.ErrRecordNotFound {
		return nil, err
	}
	rules, err := repository.ListRuleDefinitions()
	if err != nil {
		return nil, err
	}
	for _, r := range rules {
		if r.ID == id {
			code, err := os.ReadFile(r.Script)
			if err != nil {
				return nil, err
			}
			if len(code) > 64<<10 {
				return nil, fmt.Errorf("legacy code exceeds inspection limit")
			}
			return map[string]any{"legacy_source": r, "code": string(code), "needs_public_typed_trial": true}, nil
		}
	}
	return nil, gorm.ErrRecordNotFound
}
func (s *Service) QuarantineSource(ctx context.Context, id, version, reason string) error {
	return s.Data.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := agentdata.NewStore(tx).Quarantine(ctx, id, version, reason); err != nil {
			return err
		}
		var heads []knowledge.Head
		if err := tx.Where("source = ? AND active = ?", id, true).Find(&heads).Error; err != nil {
			return err
		}
		for _, h := range heads {
			if err := knowledge.NewStore(tx).Invalidate(ctx, h.ID); err != nil {
				return err
			}
		}
		return nil
	})
}

// Explicit rollback reuses the admission gate, never silently changes queued
// program hashes or resurrects a quarantined historical version.
func (s *Service) RollbackSource(ctx context.Context, id, target string) (agentdata.SourceVersion, error) {
	var out agentdata.SourceVersion
	err := s.Data.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var head agentdata.SourceHead
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&head, "source_id = ?", id).Error; err != nil {
			return err
		}
		var version agentdata.SourceVersion
		if err := tx.First(&version, "source_id = ? AND artifact_hash = ?", id, target).Error; err != nil {
			return err
		}
		if version.Status != "active" {
			return agentdata.ErrConflict
		}
		var candidate Candidate
		if err := tx.Order("created_at DESC").First(&candidate, "source_id = ? AND artifact_hash = ?", id, target).Error; err != nil {
			return err
		}
		candidate.ParentHash = head.ArtifactHash
		candidate.ParentEpoch = head.Epoch
		artifact, err := agentdata.NewStore(tx).Artifact(ctx, target)
		if err != nil {
			return err
		}
		decision, err := s.Admission(ctx, AdmissionContext{Candidate: candidate, Current: head, PolicyVersion: "public-read-v1", ValidatorVersion: "typed-receipt-v1", Artifact: artifact, Mode: "rollback"})
		if err != nil {
			return err
		}
		if !decision.Allowed {
			return fmt.Errorf("rollback admission rejected: %s", decision.Reason)
		}
		out, err = agentdata.NewStore(tx).Publish(ctx, version, head.ArtifactHash, head.Epoch)
		return err
	})
	return out, err
}
