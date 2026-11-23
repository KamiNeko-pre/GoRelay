package information

import (
	"context"
	"encoding/json"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/knowledge"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type DocumentImport struct {
	knowledge.Input
	RequestID string `json:"request_id,omitempty"`
}

// ImportDocument saves the host operation and original together. Imports can
// live in the library without manufacturing a task or a network capture.
func (s *Service) ImportDocument(ctx context.Context, input DocumentImport) (knowledge.Head, error) {
	var head knowledge.Head
	doc, err := knowledge.NewDocument(input.Input)
	if err != nil {
		return head, err
	}
	if s.Crawler == nil || s.Crawler.Fingerprint == "" {
		return head, businessError("DOCUMENT_STORE_UNAVAILABLE", "import", "资料存储尚未配置", "恢复知识库配置后重试原导入，无需重新创建任务")
	}
	err = s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if input.RequestID != "" {
			var request Request
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&request, "id = ?", input.RequestID).Error; err != nil {
				return err
			}
		}
		key := "import:" + agentdata.Hash([]string{doc.ID, doc.Revision})
		receipt := Step{RequestID: input.RequestID, Key: key, Kind: "import", Stage: "reserved", InputJSON: string(mustMarshal(input))}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&receipt).Error; err != nil {
			return err
		}
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&receipt, "request_id = ? AND `key` = ?", input.RequestID, key).Error; err != nil {
			return err
		}
		if receipt.Stage == "succeeded" {
			return json.Unmarshal([]byte(receipt.ResultJSON), &head)
		}
		var err error
		head, err = knowledge.PutDocument(tx, doc, s.Crawler.Fingerprint, input.RequestID)
		if err != nil {
			return err
		}
		link := taskDocument{RequestID: input.RequestID, ReceiptID: receipt.ID, ResponseKey: "import", DocID: doc.ID, Revision: doc.Revision, Status: "imported", BodyValid: doc.Body != "", EvidenceDigest: docEvidenceDigest(doc)}
		if err := tx.Create(&link).Error; err != nil {
			return err
		}
		if input.RequestID != "" {
			if err := tx.Model(&Request{}).Where("id = ?", input.RequestID).UpdateColumn("facts_revision", gorm.Expr("facts_revision + 1")).Error; err != nil {
				return err
			}
		}
		return tx.Model(&receipt).Updates(map[string]any{"stage": "succeeded", "result_json": string(mustMarshal(head))}).Error
	})
	return head, err
}
