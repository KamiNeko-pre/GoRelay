package knowledge

import (
	"context"
	"encoding/json"
	"time"
)

type EmbeddingUsage struct {
	PromptTokens int64 `json:"prompt_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
}
type EmbeddingMeasurement struct {
	Vectors          [][]float32
	Usage            *EmbeddingUsage
	Model, RequestID string
	DurationMS       int64
}

// The optional rich interface retains provider usage without forcing test or
// local embedders to invent billing data. Missing usage stays nil.
type MeasuredEmbedder interface {
	EmbedMeasured(context.Context, []string) (EmbeddingMeasurement, error)
}
type IndexMetrics struct {
	PlanMode         string          `json:"plan_mode"`
	Chunks           int             `json:"chunks"`
	EmbeddingCalls   int             `json:"embedding_calls"`
	EmbeddingInputs  int             `json:"embedding_inputs"`
	EmbeddingOutcome string          `json:"embedding_outcome"`
	Usage            *EmbeddingUsage `json:"usage"`
	Model            string          `json:"model,omitempty"`
	RequestID        string          `json:"request_id,omitempty"`
	QueueMS          int64           `json:"queue_ms"`
	PlanMS           int64           `json:"plan_ms"`
	SplitMS          int64           `json:"split_ms"`
	EmbeddingMS      int64           `json:"embedding_ms"`
	VectorMS         int64           `json:"vector_ms"`
	SQLMS            int64           `json:"sql_ms"`
	TotalMS          int64           `json:"total_ms"`
}
type IndexAttempt struct {
	ID          uint64     `json:"id" gorm:"primaryKey"`
	RunID       string     `json:"run_id" gorm:"size:32;index"`
	TaskID      uint64     `json:"task_id"`
	WorkID      uint64     `json:"work_id" gorm:"uniqueIndex:index_attempt"`
	Attempt     int        `json:"attempt" gorm:"uniqueIndex:index_attempt"`
	DocID       string     `json:"doc_id" gorm:"size:64;index"`
	Generation  string     `json:"generation" gorm:"size:64;index"`
	Status      string     `json:"status" gorm:"size:32"`
	MetricsJSON string     `json:"metrics_json" gorm:"type:longtext"`
	Error       string     `json:"error,omitempty" gorm:"type:text"`
	StartedAt   time.Time  `json:"started_at"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
}

func (IndexAttempt) TableName() string { return "knowledge_index_attempts" }
func (s *Store) startIndexAttempt(ctx context.Context, work IndexWork) (IndexAttempt, error) {
	a := IndexAttempt{RunID: work.OriginRunID, TaskID: work.OriginTaskID, WorkID: work.ID, Attempt: work.Attempts, DocID: work.DocID, Generation: work.Generation, Status: "running", MetricsJSON: "{}", StartedAt: time.Now()}
	err := s.DB.WithContext(ctx).Create(&a).Error
	return a, err
}
func (s *Store) saveIndexMetrics(ctx context.Context, a IndexAttempt, metrics IndexMetrics, finished bool, cause error) error {
	raw, err := json.Marshal(metrics)
	if err != nil {
		return err
	}
	updates := map[string]any{"metrics_json": string(raw)}
	if finished {
		updates["status"], updates["finished_at"] = "succeeded", time.Now()
		if cause != nil {
			updates["status"], updates["error"] = "failed", cause.Error()
		}
	}
	return s.DB.WithContext(ctx).Model(&IndexAttempt{}).Where("id = ? AND status = ?", a.ID, "running").Updates(updates).Error
}
func (s *Store) IndexAttempts(ctx context.Context, docID string) ([]IndexAttempt, error) {
	rows := []IndexAttempt{}
	q := s.DB.WithContext(ctx).Order("id")
	if docID != "" {
		q = q.Where("doc_id = ?", docID)
	}
	err := q.Find(&rows).Error
	return rows, err
}
func (s *Store) IndexAttemptsForRun(ctx context.Context, id string) ([]IndexAttempt, error) {
	rows := []IndexAttempt{}
	err := s.DB.WithContext(ctx).Where("run_id = ?", id).Order("id").Find(&rows).Error
	return rows, err
}
