package information

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/knowledge"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type MaterialLinks struct {
	ObservationIDs []string          `json:"observation_ids,omitempty"`
	Documents      []StoredReference `json:"documents,omitempty"`
}

// LinkMaterials adopts saved evidence explicitly. It retains the original
// receipt and observation identity; it does not pretend a new fetch occurred.
func (s *Service) LinkMaterials(ctx context.Context, id string, links MaterialLinks) error {
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var request Request
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&request, "id = ?", id).Error; err != nil {
			return err
		}
		var input Input
		if err := json.Unmarshal([]byte(request.InputJSON), &input); err != nil {
			return err
		}
		var changed int64
		linkDocument := func(ref StoredReference) error {
			doc, err := knowledge.NewStore(tx).ResolveReference(ctx, ref, knowledge.RetrievalScope{URLHosts: input.SourceScope})
			if err != nil {
				return businessError("REFERENCE_UNAVAILABLE", "reference_resolver", err.Error(), "选择资料搜索中可访问且符合当前来源范围的确切版本")
			}
			var original taskDocument
			err = tx.Where("doc_id = ? AND revision = ?", ref.DocID, ref.Revision).Order("receipt_id").First(&original).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				// An imported document already has a real saved version. Associate
				// it using this host link operation, explicitly marked saved material.
				receiptID, ok := ctx.Value(acquisitionReceiptKey{}).(uint64)
				if !ok {
					return businessError("LINK_RECEIPT_REQUIRED", "facts", "关联操作缺少宿主回执", "通过 read_materials(link_references/link_observation_ids) 关联保存资料")
				}
				original = taskDocument{ReceiptID: receiptID, ResponseKey: agentdata.Hash([]string{ref.DocID, ref.Revision}), DocID: ref.DocID, Revision: ref.Revision, Status: "saved_material", BodyValid: strings.TrimSpace(doc.Body) != "", EvidenceDigest: docEvidenceDigest(doc)}
			} else if err != nil {
				return err
			} else {
				if original.EvidenceDigest == "" {
					original.EvidenceDigest = docEvidenceDigest(doc)
				}
				if !original.BodyValid && original.HTTPStatus == 0 && strings.TrimSpace(doc.Body) != "" {
					original.BodyValid = true
				}
			}
			original.RequestID = id
			q := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&original)
			changed += q.RowsAffected
			return q.Error
		}
		for _, observationID := range links.ObservationIDs {
			var row observationRow
			if err := tx.First(&row, "id = ?", observationID).Error; err != nil {
				return businessError("OBSERVATION_NOT_FOUND", "facts", err.Error(), "从已保存资料查询复制真实观测 ID")
			}
			o, err := decodeObservation(row)
			if err != nil {
				return err
			}
			for _, ref := range o.References {
				if err := linkDocument(ref); err != nil {
					return err
				}
			}
			q := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&taskObservation{RequestID: id, ObservationID: observationID})
			if q.Error != nil {
				return q.Error
			}
			changed += q.RowsAffected
		}
		for _, ref := range links.Documents {
			if err := linkDocument(ref); err != nil {
				return err
			}
		}
		if changed > 0 {
			return tx.Model(&Request{}).Where("id = ?", id).UpdateColumn("facts_revision", gorm.Expr("facts_revision + 1")).Error
		}
		return nil
	})
}
func (s *Service) LinkObservations(ctx context.Context, id string, ids []string) error {
	return s.LinkMaterials(ctx, id, MaterialLinks{ObservationIDs: ids})
}
func (s *Service) LinkDocuments(ctx context.Context, id string, refs []StoredReference) error {
	return s.LinkMaterials(ctx, id, MaterialLinks{Documents: refs})
}
