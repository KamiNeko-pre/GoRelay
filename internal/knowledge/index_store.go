package knowledge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go-lua-crawler/internal/agentdata"
	task "go-lua-crawler/internal/task"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"time"
)

var ErrIndexConflict = errors.New("index target/lease conflict")

type Head struct {
	ID               string     `json:"id" gorm:"primaryKey;size:64"`
	Revision         string     `json:"revision" gorm:"size:64"`
	BodyHash         string     `json:"body_hash" gorm:"size:64"`
	Fingerprint      string     `json:"fingerprint" gorm:"size:128"`
	Epoch            uint64     `json:"target_epoch"`
	Active           bool       `json:"active"`
	Generation       string     `json:"generation" gorm:"size:64"`
	ParentGeneration string     `json:"parent_generation" gorm:"size:64"`
	Title            string     `json:"title" gorm:"size:512"`
	URL              string     `json:"url" gorm:"type:text"`
	Source           string     `json:"source" gorm:"size:128"`
	FetchedAt        *time.Time `json:"fetched_at,omitempty"`
	PublishedAt      *time.Time `json:"published_at,omitempty"`
	DataVersion      string     `json:"data_version,omitempty" gorm:"size:64"`
	TaskID           uint64     `json:"task_id,omitempty"`
	RecordKey        string     `json:"record_key,omitempty" gorm:"type:text"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

func (Head) TableName() string { return "knowledge_heads" }

type DocumentVersion struct {
	DocID     string `gorm:"primaryKey;size:64"`
	Revision  string `gorm:"primaryKey;size:64"`
	JSON      string `gorm:"type:longtext"`
	CreatedAt time.Time
}
type IndexWork struct {
	ID              uint64     `json:"id" gorm:"primaryKey"`
	OriginTaskID    uint64     `json:"origin_task_id"`
	OriginRunID     string     `json:"origin_run_id" gorm:"size:32;index"`
	OriginRequestID string     `json:"origin_request_id,omitempty" gorm:"size:32;index"`
	DocID           string     `json:"doc_id" gorm:"size:64;index;uniqueIndex:index_target"`
	Revision        string     `json:"revision" gorm:"size:64"`
	BodyHash        string     `json:"body_hash" gorm:"size:64"`
	Epoch           uint64     `json:"target_epoch" gorm:"uniqueIndex:index_target"`
	Fingerprint     string     `json:"fingerprint" gorm:"size:128;uniqueIndex:index_target"`
	Generation      string     `json:"generation" gorm:"size:64;uniqueIndex"`
	Status          string     `json:"status" gorm:"size:32;index"`
	Token           string     `json:"-" gorm:"size:32"`
	LeaseUntil      *time.Time `json:"lease_until,omitempty"`
	Attempts        int        `json:"attempts"`
	NextRetryAt     *time.Time `json:"next_retry_at,omitempty"`
	ExpectedChunks  int        `json:"expected_chunks"`
	LastError       string     `json:"last_error,omitempty" gorm:"type:text"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}
type Chunk struct {
	ID         string `json:"id" gorm:"primaryKey;size:36"`
	DocID      string `json:"doc_id" gorm:"size:64;index"`
	Revision   string `json:"revision" gorm:"size:64"`
	Generation string `json:"generation" gorm:"size:64;index"`
	Number     int    `json:"number"`
	Start      int    `json:"start"`
	End        int    `json:"end"`
	Text       string `json:"text" gorm:"type:text"`
	VectorJSON string `json:"-" gorm:"type:longtext"`
}
type Store struct{ DB *gorm.DB }

func NewStore(db *gorm.DB) *Store { return &Store{db} }
func (s *Store) Migrate() error {
	return s.DB.AutoMigrate(&Head{}, &DocumentVersion{}, &IndexWork{}, &Chunk{}, &IndexAttempt{})
}
func (s *Store) Head(ctx context.Context, id string) (Head, error) {
	var h Head
	err := s.DB.WithContext(ctx).First(&h, "id = ?", id).Error
	return h, err
}
func (s *Store) Heads(ctx context.Context) ([]Head, error) {
	var rows []Head
	err := s.DB.WithContext(ctx).Order("updated_at DESC").Limit(100).Find(&rows).Error
	return rows, err
}

// RetargetModel walks the corpus rather than the bounded workbench page.
// Each current head is rechecked under its lock so deletion cannot resurrect it.
func (s *Store) RetargetModel(ctx context.Context, fingerprint string) (int, error) {
	if fingerprint == "" {
		return 0, fmt.Errorf("embedding fingerprint required")
	}
	count := 0
	var batch []Head
	err := s.DB.WithContext(ctx).Where("active = ? AND fingerprint <> ?", true, fingerprint).FindInBatches(&batch, 100, func(_ *gorm.DB, _ int) error {
		for _, candidate := range batch {
			if err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
				var current Head
				if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&current, "id = ?", candidate.ID).Error; err != nil {
					return err
				}
				if !current.Active || current.Fingerprint == fingerprint {
					return nil
				}
				doc, err := NewStore(tx).Document(ctx, current.ID, current.Revision)
				if err != nil {
					return err
				}
				if _, err = PutDocument(tx, doc, fingerprint); err != nil {
					return err
				}
				count++
				return nil
			}); err != nil {
				return err
			}
		}
		return nil
	}).Error
	return count, err
}
func (s *Store) Document(ctx context.Context, id, revision string) (Document, error) {
	var v DocumentVersion
	err := s.DB.WithContext(ctx).First(&v, "doc_id = ? AND revision = ?", id, revision).Error
	var d Document
	if err == nil {
		err = json.Unmarshal([]byte(v.JSON), &d)
	}
	return d, err
}
func (s *Store) Put(ctx context.Context, doc Document, fingerprint string) (Head, error) {
	var head Head
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error { var err error; head, err = PutDocument(tx, doc, fingerprint); return err })
	return head, err
}

// PutDocument is called inside the result/terminal-state transaction. No external
// Embedding or vector calls are performed while holding this database transaction.
func PutDocument(tx *gorm.DB, doc Document, fingerprint string, requestIDs ...string) (Head, error) {
	var head Head
	if doc.ID == "" || doc.Revision == "" || fingerprint == "" {
		return head, fmt.Errorf("document identity and embedding fingerprint required")
	}
	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&Head{ID: doc.ID}).Error; err != nil {
		return head, err
	}
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&head, "id = ?", doc.ID).Error; err != nil {
		return head, err
	}
	if head.Active && head.Revision == doc.Revision && head.Fingerprint == fingerprint {
		return head, nil
	}
	data, err := json.Marshal(doc)
	if err != nil {
		return head, err
	}
	if err = tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&DocumentVersion{DocID: doc.ID, Revision: doc.Revision, JSON: string(data)}).Error; err != nil {
		return head, err
	}
	// Arrival order is not acquisition order: a slow worker can commit an old
	// snapshot after a newer one. Preserve its audit version without publishing
	// it as current knowledge or scheduling an obsolete index generation.
	if head.FetchedAt != nil && !doc.FetchedAt.IsZero() && doc.FetchedAt.Before(*head.FetchedAt) {
		return head, nil
	}
	head.Epoch++
	if head.Generation != "" {
		head.ParentGeneration = head.Generation
	}
	head.Generation = ""
	head.Active = true
	head.Revision = doc.Revision
	head.BodyHash = agentdata.Hash(doc.Body)
	head.Fingerprint = fingerprint
	head.Title = doc.Title
	head.URL = doc.URL
	head.Source = doc.Source
	head.FetchedAt = nil
	if !doc.FetchedAt.IsZero() {
		head.FetchedAt = &doc.FetchedAt
	}
	head.PublishedAt = doc.PublishedAt
	head.DataVersion = doc.DataVersion
	head.TaskID = doc.TaskID
	head.RecordKey = doc.RecordKey
	if err = tx.Save(&head).Error; err != nil {
		return head, err
	}
	work := IndexWork{DocID: head.ID, Revision: head.Revision, BodyHash: head.BodyHash, Epoch: head.Epoch, Fingerprint: head.Fingerprint, Generation: agentdata.Hash([]any{head.ID, head.Epoch, head.Fingerprint}), Status: "pending"}
	work.OriginTaskID = doc.TaskID
	// Request attribution belongs to the new work receipt, never to document
	// identity or revision. Reusing an unchanged head creates no new charge.
	if len(requestIDs) > 0 {
		work.OriginRequestID = requestIDs[0]
	}
	if doc.TaskID > 0 {
		var origin task.CrawlTask
		if err := tx.First(&origin, doc.TaskID).Error; err == nil {
			work.OriginRunID = origin.RunID
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return head, err
		}
	}
	err = tx.Create(&work).Error
	return head, err
}
func (s *Store) Invalidate(ctx context.Context, id string) error {
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		q := tx.Model(&Head{}).Where("id = ?", id).Updates(map[string]any{"active": false, "epoch": gorm.Expr("epoch + 1"), "generation": ""})
		if q.Error != nil {
			return q.Error
		}
		if q.RowsAffected != 1 {
			return gorm.ErrRecordNotFound
		}
		return tx.Model(&IndexWork{}).Where("doc_id = ? AND status IN ?", id, []string{"pending", "running", "failed"}).Updates(map[string]any{"status": "superseded", "token": "", "lease_until": nil}).Error
	})
}
func (s *Store) Claim(ctx context.Context, ttl time.Duration) (IndexWork, error) {
	var work IndexWork
	now := time.Now()
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).Where("(status = ? OR (status = ? AND next_retry_at <= ?) OR (status = ? AND lease_until <= ?))", "pending", "failed", now, "running", now).Order("id").First(&work).Error; err != nil {
			return err
		}
		until := now.Add(ttl)
		work.Status = "running"
		work.Token = agentdata.Token()
		work.LeaseUntil = &until
		work.Attempts++
		return tx.Save(&work).Error
	})
	return work, err
}
func workOwned(tx *gorm.DB, work IndexWork) error {
	var stored IndexWork
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&stored, work.ID).Error; err != nil {
		return err
	}
	if stored.Status != "running" || stored.Token != work.Token || stored.LeaseUntil == nil || !stored.LeaseUntil.After(time.Now()) {
		return ErrIndexConflict
	}
	return nil
}
func (s *Store) SaveChunk(ctx context.Context, work IndexWork, chunk Chunk, vector []float32) error {
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := workOwned(tx, work); err != nil {
			return err
		}
		if chunk.DocID != work.DocID || chunk.Generation != work.Generation || chunk.Revision != work.Revision || len(vector) == 0 {
			return ErrIndexConflict
		}
		data, err := json.Marshal(vector)
		if err != nil {
			return err
		}
		chunk.VectorJSON = string(data)
		return tx.Clauses(clause.OnConflict{UpdateAll: true}).Create(&chunk).Error
	})
}
func (s *Store) Publish(ctx context.Context, work IndexWork, count int) error {
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := workOwned(tx, work); err != nil {
			return err
		}
		var head Head
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&head, "id = ?", work.DocID).Error; err != nil {
			return err
		}
		if !head.Active || head.Epoch != work.Epoch || head.Revision != work.Revision || head.Fingerprint != work.Fingerprint {
			return ErrIndexConflict
		}
		var chunks []Chunk
		if err := tx.Where("generation = ?", work.Generation).Order("number").Find(&chunks).Error; err != nil {
			return err
		}
		if count <= 0 || len(chunks) != count {
			return fmt.Errorf("incomplete index generation")
		}
		for i, c := range chunks {
			if c.Number != i || c.VectorJSON == "" {
				return fmt.Errorf("incomplete chunk manifest")
			}
		}
		if err := tx.Model(&Head{}).Where("id = ?", head.ID).Update("generation", work.Generation).Error; err != nil {
			return err
		}
		return tx.Model(&IndexWork{}).Where("id = ?", work.ID).Updates(map[string]any{"status": "ready", "expected_chunks": count, "lease_until": nil, "token": "", "last_error": ""}).Error
	})
}
func (s *Store) Fail(ctx context.Context, work IndexWork, cause error) error {
	var next *time.Time
	if work.Attempts < 3 && !errors.Is(cause, ErrCorePending) && !errors.Is(cause, ErrIndexConflict) {
		v := time.Now().Add(time.Duration(work.Attempts) * 5 * time.Second)
		next = &v
	}
	status := "failed"
	if errors.Is(cause, ErrIndexConflict) {
		status = "superseded"
	}
	q := s.DB.WithContext(ctx).Model(&IndexWork{}).Where("id = ? AND token = ? AND status = ?", work.ID, work.Token, "running").Updates(map[string]any{"status": status, "next_retry_at": next, "last_error": cause.Error(), "lease_until": nil, "token": ""})
	return q.Error
}
func (s *Store) Retry(ctx context.Context, id string) error {
	return s.DB.WithContext(ctx).Model(&IndexWork{}).Where("doc_id = ? AND status = ?", id, "failed").Updates(map[string]any{"status": "pending", "next_retry_at": nil}).Error
}
func (s *Store) Work(ctx context.Context, generation string) (IndexWork, error) {
	var w IndexWork
	err := s.DB.WithContext(ctx).First(&w, "generation = ?", generation).Error
	return w, err
}
func (s *Store) Works(ctx context.Context) ([]IndexWork, error) {
	var rows []IndexWork
	err := s.DB.WithContext(ctx).Order("id DESC").Limit(100).Find(&rows).Error
	return rows, err
}
func (s *Store) GenerationChunks(ctx context.Context, generation string) ([]Chunk, error) {
	var rows []Chunk
	err := s.DB.WithContext(ctx).Where("generation = ?", generation).Order("number").Find(&rows).Error
	return rows, err
}
func (s *Store) CurrentChunks(ctx context.Context, ids []string) ([]Chunk, error) {
	var rows []Chunk
	if len(ids) == 0 {
		return rows, nil
	}
	err := s.DB.WithContext(ctx).Table("chunks c").Select("c.*").Joins("JOIN knowledge_heads h ON h.id = c.doc_id AND h.generation = c.generation AND h.revision = c.revision").Where("c.id IN ? AND h.active = ?", ids, true).Find(&rows).Error
	return rows, err
}
