package information

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"sort"
	"time"

	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/knowledge"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type StoredReference = knowledge.DocumentReference

type Observation struct {
	ID             string            `json:"id"`
	ReceiptID      uint64            `json:"receipt_id"`
	RecordPosition int               `json:"record_position"`
	Fields         map[string]any    `json:"fields"`
	References     []StoredReference `json:"references"`
	FetchedAt      time.Time         `json:"fetched_at"`
}

type AcquiredRecord struct {
	Position    int               `json:"position"`
	ResponseKey string            `json:"response_key"`
	Fields      map[string]any    `json:"fields"`
	References  []StoredReference `json:"references"`
	FetchedAt   time.Time         `json:"fetched_at"`
}

type observationRow struct {
	ID             string    `gorm:"primaryKey;size:32"`
	ReceiptID      uint64    `gorm:"uniqueIndex:observation_receipt_position"`
	RecordPosition int       `gorm:"uniqueIndex:observation_receipt_position"`
	ResponseKey    string    `gorm:"size:128"`
	FieldsJSON     string    `gorm:"type:longtext"`
	ReferencesJSON string    `gorm:"type:longtext"`
	FetchedAt      time.Time `gorm:"precision:6"`
}

func (observationRow) TableName() string { return "information_observations" }

type taskObservation struct {
	RequestID     string `gorm:"primaryKey;size:32"`
	ObservationID string `gorm:"primaryKey;size:32"`
}

func (taskObservation) TableName() string { return "information_task_observations" }

// A response is an investigation result even when it contains no records.
type taskDocument struct {
	RequestID       string `gorm:"primaryKey;size:32"`
	ReceiptID       uint64 `gorm:"primaryKey"`
	ResponseKey     string `gorm:"primaryKey;size:128"`
	DocID           string `gorm:"size:64"`
	Revision        string `gorm:"size:64"`
	Status          string `gorm:"size:32"`
	RecordCount     int
	HTTPStatus      int
	BodyValid       bool
	EvidenceDigest  string `gorm:"size:64"`
	SourceURL       string `gorm:"type:text"`
	RequestProfile  string `gorm:"size:64"`
	SnapshotKey     string `gorm:"size:64"`
	SnapshotHeaders string `gorm:"type:text"`
}

func (taskDocument) TableName() string { return "information_task_documents" }

func docEvidenceDigest(doc knowledge.Document) string {
	return agentdata.Hash([]string{"document", doc.URL, doc.Body, doc.RawBody})
}

func isValidDocumentForCompleted(td taskDocument) bool {
	return td.Status != "failed" && td.HTTPStatus < 400 && (td.BodyValid || td.RecordCount > 0)
}

func decodeObservation(row observationRow) (Observation, error) {
	o := Observation{ID: row.ID, ReceiptID: row.ReceiptID, RecordPosition: row.RecordPosition, FetchedAt: row.FetchedAt}
	if err := json.Unmarshal([]byte(row.FieldsJSON), &o.Fields); err != nil {
		return o, err
	}
	err := json.Unmarshal([]byte(row.ReferencesJSON), &o.References)
	return o, err
}

// IngestObservations accepts only an existing host receipt. Plan changes and
// view operations never move or reclassify the saved facts.
func (s *Service) IngestObservations(ctx context.Context, requestID string, receiptID uint64, records []AcquiredRecord) ([]Observation, error) {
	var result []Observation
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var request Request
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&request, "id = ?", requestID).Error; err != nil {
			return err
		}
		var receipt Step
		if err := tx.First(&receipt, "id = ? AND request_id = ?", receiptID, requestID).Error; err != nil {
			return err
		}
		var added int64
		for _, record := range records {
			if record.Position < 0 || record.ResponseKey == "" || record.FetchedAt.IsZero() {
				return businessError("INVALID_ACQUIRED_RECORD", "acquisition", "记录缺少稳定位置、响应身份或获取时间", "使用持久获取回执中的原始记录位置、响应身份与获取时间")
			}
			fields, err := json.Marshal(record.Fields)
			if err != nil {
				return err
			}
			refs, err := json.Marshal(record.References)
			if err != nil {
				return err
			}
			fetchedAt := record.FetchedAt.UTC().Truncate(time.Microsecond)
			row := observationRow{ID: agentdata.Token(), ReceiptID: receiptID, RecordPosition: record.Position, ResponseKey: record.ResponseKey, FieldsJSON: string(fields), ReferencesJSON: string(refs), FetchedAt: fetchedAt}
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
				return err
			}
			var saved observationRow
			if err := tx.First(&saved, "receipt_id = ? AND record_position = ?", receiptID, record.Position).Error; err != nil {
				return err
			}
			o, err := decodeObservation(saved)
			if err != nil {
				return err
			}
			var normalized map[string]any
			if err := json.Unmarshal(fields, &normalized); err != nil {
				return err
			}
			if !reflect.DeepEqual(o.Fields, normalized) || saved.ReferencesJSON != string(refs) || saved.ResponseKey != record.ResponseKey || !saved.FetchedAt.Equal(fetchedAt) {
				return businessError("RECEIPT_CONTENT_CONFLICT", "acquisition", "同一回执位置的内容发生变化", "读取已保存回执，保留原始记录顺序和内容，不覆盖已有事实")
			}
			linked := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&taskObservation{RequestID: requestID, ObservationID: saved.ID})
			if linked.Error != nil {
				return linked.Error
			}
			added += linked.RowsAffected
			result = append(result, o)
		}
		if added > 0 {
			return tx.Model(&Request{}).Where("id = ?", requestID).UpdateColumn("facts_revision", gorm.Expr("facts_revision + 1")).Error
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

type ObservationQuery struct {
	ObservationIDs []string `json:"observation_ids,omitempty"`
	Cursor         string   `json:"cursor,omitempty"`
	Limit          int      `json:"limit"`
	documents      DocumentQuery
}
type ObservationPage struct {
	Items             []Observation `json:"items"`
	Total             int64         `json:"total"`
	NextCursor        string        `json:"next_cursor,omitempty"`
	CollectionVersion string        `json:"collection_version"`
}

type observationCursor struct {
	RequestID string   `json:"request_id"`
	Revision  string   `json:"revision"`
	IDs       []string `json:"ids,omitempty"`
	Documents string   `json:"documents,omitempty"`
	After     string   `json:"after"`
}

func (s *Service) QueryObservations(ctx context.Context, requestID string, query ObservationQuery) (ObservationPage, error) {
	page := ObservationPage{Items: []Observation{}}
	if query.Limit <= 0 {
		return page, businessError("MISSING_PAGE_LIMIT", "facts", "limit 必须为正整数", "指定每页条数，并使用返回的 next_cursor 继续读取")
	}
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var request Request
		if err := tx.First(&request, "id = ?", requestID).Error; err != nil {
			return err
		}
		ids := append([]string(nil), query.ObservationIDs...)
		sort.Strings(ids)
		scoped := *s
		scoped.DB = tx
		base, err := scoped.observationQuery(ctx, requestID, query)
		if err != nil {
			return err
		}
		actualIDs := []string{}
		if err := base.Session(&gorm.Session{}).Order("id").Pluck("id", &actualIDs).Error; err != nil {
			return err
		}
		page.CollectionVersion = agentdata.Hash(actualIDs)
		cursor := ""
		documentFilter := ""
		if len(query.documents.documentIDs) > 0 || len(query.documents.sourceIDs) > 0 {
			docs := append([]string(nil), query.documents.documentIDs...)
			sources := append([]string(nil), query.documents.sourceIDs...)
			sort.Strings(docs)
			sort.Strings(sources)
			documentFilter = agentdata.Hash([][]string{docs, sources})
		}
		if query.Cursor != "" {
			var saved observationCursor
			bytes, err := base64.RawURLEncoding.DecodeString(query.Cursor)
			if err != nil || json.Unmarshal(bytes, &saved) != nil || saved.RequestID != requestID || saved.Revision != page.CollectionVersion || !reflect.DeepEqual(saved.IDs, ids) || saved.Documents != documentFilter || saved.After == "" {
				return businessError("FACTS_VERSION_CHANGED", "facts", "事实集合版本或游标无效", "清空游标重新查询，使用本次返回的集合版本")
			}
			cursor = saved.After
		}
		page.Total = int64(len(actualIDs))
		var rows []observationRow
		q := base.Order("id").Limit(query.Limit + 1)
		if cursor != "" {
			q = q.Where("id > ?", cursor)
		}
		if err := q.Find(&rows).Error; err != nil {
			return err
		}
		if len(rows) > query.Limit {
			rows = rows[:query.Limit]
			encoded, _ := json.Marshal(observationCursor{RequestID: requestID, Revision: page.CollectionVersion, IDs: ids, Documents: documentFilter, After: rows[len(rows)-1].ID})
			page.NextCursor = base64.RawURLEncoding.EncodeToString(encoded)
		}
		for _, row := range rows {
			o, err := decodeObservation(row)
			if err != nil {
				return err
			}
			page.Items = append(page.Items, o)
		}
		return nil
	})
	return page, err
}

func (s *Service) observationQuery(ctx context.Context, requestID string, filter ObservationQuery) (*gorm.DB, error) {
	base := s.DB.WithContext(ctx).Model(&observationRow{}).Where("id IN (?)", s.DB.Model(&taskObservation{}).Select("observation_id").Where("request_id = ?", requestID))
	if len(filter.ObservationIDs) > 0 {
		base = base.Where("id IN ?", filter.ObservationIDs)
	}
	if len(filter.documents.documentIDs) == 0 && len(filter.documents.sourceIDs) == 0 {
		return base, nil
	}
	docs, err := s.taskDocuments(ctx, requestID, filter.documents)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(docs))
	for _, doc := range docs {
		ids = append(ids, doc.DocID)
	}
	return base.Where("JSON_OVERLAPS(JSON_EXTRACT(references_json, '$[*].doc_id'), ?)", string(mustMarshal(ids))), nil
}
