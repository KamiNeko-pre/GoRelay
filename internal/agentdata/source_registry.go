package agentdata

import (
	"context"
	"errors"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Publish persists an admission already approved by RegisterValidatedSource.
// It rechecks current SQL epoch/version; it is not the admission decision.
func (s *Store) Publish(ctx context.Context, version SourceVersion, parent string, epoch uint64) (SourceVersion, error) {
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var artifact ProgramArtifact
		if err := tx.First(&artifact, "hash = ?", version.ArtifactHash).Error; err != nil {
			return err
		}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&SourceHead{SourceID: version.SourceID, Status: "candidate"}).Error; err != nil {
			return err
		}
		var head SourceHead
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&head, "source_id = ?", version.SourceID).Error; err != nil {
			return err
		}
		if head.ArtifactHash == version.ArtifactHash && head.Status == "active" {
			var prior SourceVersion
			if err := tx.Where("source_id = ? AND artifact_hash = ?", version.SourceID, version.ArtifactHash).First(&prior).Error; err != nil {
				return err
			}
			if prior.ProofJSON != version.ProofJSON {
				return ErrConflict
			}
			version = prior
			return nil
		}
		if head.Epoch != epoch || head.ArtifactHash != parent {
			return ErrConflict
		}
		var previous SourceVersion
		err := tx.Where("source_id = ? AND artifact_hash = ?", version.SourceID, version.ArtifactHash).First(&previous).Error
		if err == nil && previous.Status == "quarantined" {
			return ErrConflict
		}
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		version.Status = "active"
		if err = tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&version).Error; err != nil {
			return err
		}
		return tx.Model(&SourceHead{}).Where("source_id = ? AND epoch = ?", head.SourceID, epoch).Updates(map[string]any{"artifact_hash": version.ArtifactHash, "epoch": epoch + 1, "status": "active", "reason": ""}).Error
	})
	return version, err
}
func (s *Store) Quarantine(ctx context.Context, source, version, reason string) error {
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		q := tx.Model(&SourceHead{}).Where("source_id = ? AND artifact_hash = ? AND status = ?", source, version, "active").Updates(map[string]any{"status": "quarantined", "epoch": gorm.Expr("epoch + 1"), "reason": reason})
		if q.Error != nil {
			return q.Error
		}
		if q.RowsAffected != 1 {
			return ErrConflict
		}
		return tx.Model(&SourceVersion{}).Where("source_id = ? AND artifact_hash = ?", source, version).Update("status", "quarantined").Error
	})
}
func (s *Store) ListSources(ctx context.Context) ([]SourceVersion, error) {
	var rows []SourceVersion
	err := s.DB.WithContext(ctx).Table("source_versions v").Select("v.*").Joins("JOIN source_heads h ON h.source_id = v.source_id AND h.artifact_hash = v.artifact_hash").Where("h.status = ? AND v.status = ?", "active", "active").Find(&rows).Error
	return rows, err
}
func (s *Store) SourceHead(ctx context.Context, id string) (SourceHead, error) {
	var head SourceHead
	err := s.DB.WithContext(ctx).First(&head, "source_id = ?", id).Error
	return head, err
}
