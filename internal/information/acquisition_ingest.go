package information

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/knowledge"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// The physical result and its admitted receipt become visible together.
func (h *informationHost) commitAcquisition(ctx context.Context, step Step, rows []aiapp.Record, captures []acquisition.Response, out map[string]any, cause error, saved map[int]knowledge.Document, sourceEntries ...map[int]map[string]any) error {
	return h.owned(ctx, func(tx *gorm.DB) error {
		scoped := *h.service
		scoped.DB = tx
		if len(rows) > 0 || len(captures) > 0 {
			observations, err := scoped.ingestAcquiredRecords(ctx, step.RequestID, step.ID, rows, captures, saved)
			if err != nil {
				return err
			}
			var previewCount int
			if len(rows) > 0 || len(observations) > 0 {
				out["observation_count"] = len(observations)
				previews := compactObservationPreviewsWithLimits(observations, 8192, 0)
				if sharedURL, ok := sharedObservationSourceURL(observations); ok {
					out["shared_source_url"] = sharedURL
				}
				out["observations"] = previews
				previewCount = len(previews)
				if omitted := len(observations) - len(previews); omitted > 0 {
					out["observations_omitted"] = omitted
					out["observations_preview_hint"] = fmt.Sprintf("Preview truncated: showing %d of %d saved records (%d omitted). Call read_materials(kind='observations', document_ids=[...], limit=50) with next_cursor to inspect omitted records, or tighten Lua filtering.", len(previews), len(observations), omitted)
				}
			}
			var documents []taskDocument
			if err := tx.Where("request_id = ? AND receipt_id = ?", step.RequestID, step.ID).Order("response_key").Find(&documents).Error; err != nil {
				return err
			}
			refs := make([]StoredReference, 0, len(documents))
			byResponse := make(map[string]StoredReference, len(documents))
			for _, doc := range documents {
				if isValidDocumentForCompleted(doc) {
					ref := StoredReference{DocID: doc.DocID, Revision: doc.Revision}
					refs = append(refs, ref)
					byResponse[doc.ResponseKey] = ref
				}
			}
			out["references"] = refs
			for _, entries := range sourceEntries {
				for i, entry := range entries {
					if ref, ok := byResponse[acquisitionResponseKey(i)]; ok {
						entry["source_ref"] = ref
					}
				}
			}
			if evidence := sampleEvidenceFor(rows, byResponse, previewCount); len(evidence) > 0 {
				out["sample_evidence"] = evidence
			}
		}
		return scoped.finishStep(ctx, step, out, cause)
	})
}

const reusedResponseStatus = "reused_response"

// AcquiredResponse is constructed by the acquisition adapter from a persisted
// response, not accepted as model-authored source evidence.
type AcquiredResponse struct {
	Key             string
	Document        knowledge.Document
	HTTPStatus      int
	BodyValid       *bool
	Blocked         bool
	RequestProfile  string
	SnapshotKey     string
	SnapshotHeaders string
	reused          bool
}

// IngestAcquisition keeps response documents and record links in one transaction.
// A zero-record response remains usable evidence for a no_match delivery.
func (s *Service) IngestAcquisition(ctx context.Context, requestID string, receiptID uint64, responses []AcquiredResponse, records []AcquiredRecord) ([]Observation, error) {
	if s.Crawler == nil || s.Crawler.Fingerprint == "" {
		return nil, errors.New("acquisition document store fingerprint is not configured")
	}
	var observations []Observation
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var request Request
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&request, "id = ?", requestID).Error; err != nil {
			return err
		}
		var receipt Step
		if err := tx.First(&receipt, "id = ? AND request_id = ?", receiptID, requestID).Error; err != nil {
			return err
		}
		byKey := map[string]knowledge.Document{}
		for _, response := range responses {
			if response.Key == "" || response.Document.ID == "" || response.Document.Revision == "" {
				return businessError("INVALID_RESPONSE_IDENTITY", "acquisition", "获取响应缺少稳定身份或原文版本", "使用持久回执中的响应身份和版本化原文")
			}
			if _, exists := byKey[response.Key]; exists {
				return businessError("DUPLICATE_RESPONSE_KEY", "acquisition", "回执内响应身份重复", "保留每个真实响应的独立稳定身份")
			}
			digest := docEvidenceDigest(response.Document)
			persistedDocument := response.reused
			if !response.reused && !response.Blocked && response.HTTPStatus >= 200 && response.HTTPStatus < 300 && response.Document.Source == "information-response" {
				var prior taskDocument
				err := tx.Where("request_id = ? AND source_url = ? AND evidence_digest = ? AND status = ? AND body_valid = ?", requestID, response.Document.URL, digest, "succeeded", true).Order("receipt_id, response_key").Take(&prior).Error
				if err == nil {
					original := response.Document
					response.Document.ID, response.Document.Revision = prior.DocID, prior.Revision
					persistedDocument = true
					for i := range records {
						for j := range records[i].References {
							ref := &records[i].References[j]
							if ref.DocID == original.ID && ref.Revision == original.Revision {
								ref.DocID, ref.Revision = prior.DocID, prior.Revision
							}
						}
					}
				} else if !errors.Is(err, gorm.ErrRecordNotFound) {
					return err
				}
			}
			byKey[response.Key] = response.Document
			var saved taskDocument
			err := tx.First(&saved, "request_id = ? AND receipt_id = ? AND response_key = ?", requestID, receiptID, response.Key).Error
			if err == nil {
				if saved.DocID != response.Document.ID || saved.Revision != response.Document.Revision {
					return businessError("RECEIPT_CONTENT_CONFLICT", "acquisition", "同一响应指向不同原文版本", "从原持久回执恢复，不重写响应")
				}
				continue
			}
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			count := 0
			for _, record := range records {
				linked := record.ResponseKey == response.Key
				for _, ref := range record.References {
					linked = linked || (ref.DocID == response.Document.ID && ref.Revision == response.Document.Revision)
				}
				if linked {
					count++
				}
			}
			docStatus := "succeeded"
			if response.HTTPStatus >= 400 || response.Blocked {
				docStatus = "failed"
			} else if response.reused {
				docStatus = reusedResponseStatus
			}
			bodyValid := strings.TrimSpace(response.Document.Body) != "" || count > 0
			if response.BodyValid != nil {
				bodyValid = *response.BodyValid || count > 0
			} else if response.HTTPStatus >= 400 {
				bodyValid = count > 0
			}
			if docStatus == "succeeded" && bodyValid && !persistedDocument {
				if _, err := knowledge.PutDocument(tx, response.Document, s.Crawler.Fingerprint, requestID); err != nil {
					return err
				}
			}
			link := taskDocument{
				RequestID:       requestID,
				ReceiptID:       receiptID,
				ResponseKey:     response.Key,
				DocID:           response.Document.ID,
				Revision:        response.Document.Revision,
				Status:          docStatus,
				RecordCount:     count,
				HTTPStatus:      response.HTTPStatus,
				SourceURL:       response.Document.URL,
				RequestProfile:  response.RequestProfile,
				SnapshotKey:     response.SnapshotKey,
				SnapshotHeaders: response.SnapshotHeaders,
				BodyValid:       bodyValid,
				EvidenceDigest:  digest,
			}
			if err := tx.Create(&link).Error; err != nil {
				return err
			}
			if err := tx.Model(&Request{}).Where("id = ?", requestID).UpdateColumn("facts_revision", gorm.Expr("facts_revision + 1")).Error; err != nil {
				return err
			}
		}
		for i := range records {
			doc, ok := byKey[records[i].ResponseKey]
			if !ok {
				return businessError("RESPONSE_NOT_FOUND", "acquisition", "记录没有对应的持久响应", "提供该记录的实际响应及稳定响应身份")
			}
			if len(records[i].References) == 0 {
				records[i].References = []StoredReference{{DocID: doc.ID, Revision: doc.Revision}}
			}
			for _, ref := range records[i].References {
				found := false
				for _, captured := range byKey {
					if ref.DocID == captured.ID && ref.Revision == captured.Revision {
						found = true
						break
					}
				}
				if !found {
					return businessError("RECORD_RESPONSE_MISMATCH", "acquisition", "记录引用不属于这次实际获取的响应", "从该回执真实响应生成字段定位")
				}
			}
		}
		var err error
		observations, err = (&Service{DB: tx}).IngestObservations(ctx, requestID, receiptID, records)
		return err
	})
	if err != nil {
		return nil, err
	}
	return observations, nil
}
