package pipeline

import (
	"errors"
	"time"

	"gorelay/internal/acquisition"
	"gorelay/internal/contract"
)

const (
	DefaultMCPResponseMaxBytes = 16 * 1024
	DefaultMCPPageSize         = 20
	DefaultMaxFieldChars       = 2048
	DefaultSnapshotExcerptSize = 64 * 1024
)

var ErrSilentDrift = errors.New("silent schema drift or soft-block detected")

// SourceRecord stores cleaned, deduplicated structured records with nullable
// is_current unique constraint so only one current row exists per (source_id, entity_key).
type SourceRecord struct {
	ID               uint64    `json:"id" gorm:"primaryKey;autoIncrement"`
	SourceID         string    `json:"source_id" gorm:"size:64;not null;uniqueIndex:idx_source_entity_current,priority:1;index:idx_source_job,priority:1"`
	EntityKey        string    `json:"entity_key" gorm:"size:191;not null;uniqueIndex:idx_source_entity_current,priority:2"`
	IsCurrent        *bool     `json:"is_current,omitempty" gorm:"uniqueIndex:idx_source_entity_current,priority:3;index"`
	ConnectorVersion int       `json:"connector_version" gorm:"not null;default:1"`
	JobID            uint64    `json:"job_id" gorm:"index:idx_source_job,priority:2;index"`
	ContentHash      string    `json:"content_hash" gorm:"size:64;not null;index"`
	PayloadJSON      string    `json:"payload_json" gorm:"type:longtext;not null"`
	SnapshotRef      string    `json:"snapshot_ref,omitempty" gorm:"size:64;index"`
	SourceURL        string    `json:"source_url,omitempty" gorm:"type:text"`
	FetchedAt        time.Time `json:"fetched_at" gorm:"index"`
	CreatedAt        time.Time `json:"created_at"`
}

func (SourceRecord) TableName() string { return "source_records" }

// SnapshotBlob stores content-addressed metadata + gzip file reference for raw source bodies.
type SnapshotBlob struct {
	SnapshotRef  string    `json:"snapshot_ref" gorm:"primaryKey;size:64"`
	SourceID     string    `json:"source_id" gorm:"size:64;index"`
	JobID        uint64    `json:"job_id" gorm:"index"`
	SourceURL    string    `json:"source_url" gorm:"type:text"`
	Title        string    `json:"title,omitempty" gorm:"size:256"`
	ContentType  string    `json:"content_type,omitempty" gorm:"size:128"`
	GzipPath     string    `json:"gzip_path,omitempty" gorm:"size:255"`
	PlainExcerpt string    `json:"-" gorm:"type:mediumtext"`
	ByteSize     int64     `json:"byte_size"`
	FetchedAt    time.Time `json:"fetched_at" gorm:"index"`
	CreatedAt    time.Time `json:"created_at"`
}

func (SnapshotBlob) TableName() string { return "snapshot_blobs" }

type DriftDiagnosis struct {
	Code        string         `json:"code"`
	Reason      string         `json:"reason"`
	FailedRows  int            `json:"failed_rows"`
	TotalRows   int            `json:"total_rows"`
	SampleRow   map[string]any `json:"sample_row,omitempty"`
	HTTPStatus  int            `json:"http_status,omitempty"`
}

type IngestInput struct {
	SourceID         string
	ConnectorVersion int
	JobID            uint64
	OutputSchema     contract.OutputSchema
	Records          []map[string]any
	Evidence         []acquisition.Response
	AllowEmptyBatch  bool
	RequireNonEmpty  bool
	SnapshotDir      string
}

type IngestResult struct {
	InputRows         int             `json:"input_rows"`
	SavedRows         int             `json:"saved_rows"`
	SkippedDuplicates int             `json:"skipped_duplicates"`
	SnapshotRefs      []string        `json:"snapshot_refs,omitempty"`
	Drift             *DriftDiagnosis `json:"drift,omitempty"`
}

type DeliveredPage struct {
	SourceID          string           `json:"source_id,omitempty"`
	JobID             uint64           `json:"job_id,omitempty"`
	Returned          int              `json:"returned"`
	Total             int64            `json:"total"`
	TruncatedToBudget bool             `json:"truncated_to_budget,omitempty"`
	NextCursor        string           `json:"next_cursor,omitempty"`
	Summary           string           `json:"summary,omitempty"`
	Records           []map[string]any `json:"records"`
	SnapshotURLs      []string         `json:"snapshot_urls,omitempty"`
}

type EvidenceHit struct {
	SnapshotRef string    `json:"snapshot_ref"`
	SourceID    string    `json:"source_id"`
	SourceURL   string    `json:"source_url"`
	Title       string    `json:"title,omitempty"`
	Snippet     string    `json:"snippet"`
	SnapshotURL string    `json:"snapshot_url"`
	FetchedAt   time.Time `json:"fetched_at"`
	Score       float64   `json:"score,omitempty"`
}

type EvidencePage struct {
	Entries    []EvidenceHit `json:"entries"`
	Total      int           `json:"total"`
	NextCursor string        `json:"next_cursor,omitempty"`
}
