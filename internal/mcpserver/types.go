package mcpserver

import (
	"time"

	"go-lua-crawler/internal/knowledge"
)

type SourceQuery struct {
	View     string `json:"view,omitempty" form:"view" jsonschema:"Optional catalog view: saved (has local documents), reusable (can collect again), or discovered (candidate URLs). Omit for all sources."`
	Query    string `json:"query,omitempty" form:"query" jsonschema:"Source name or topic keywords; omit to browse saved sources"`
	Cursor   string `json:"cursor,omitempty" form:"cursor" jsonschema:"next_cursor from the preceding catalog page"`
	PageSize int    `json:"page_size,omitempty" form:"page_size" jsonschema:"Results per page; omitted means 20, and next_cursor exposes the remainder"`
}

type DeliveryQuery struct {
	Query    string `json:"query,omitempty" form:"query" jsonschema:"Keywords from the original task. Omit to browse saved reports and comparison tables."`
	Cursor   string `json:"cursor,omitempty" form:"cursor" jsonschema:"next_cursor returned by the preceding delivery page"`
	PageSize int    `json:"page_size,omitempty" form:"page_size" jsonschema:"Entries per page; omitted means 20, with the remainder available through next_cursor"`
}

type DeliveryID struct {
	ID string `json:"delivery_id" jsonschema:"Exact delivery_id returned by gorelay_find_deliveries or a completed GoRelay task"`
}

type DeliveryEntry struct {
	ID         string    `json:"delivery_id"`
	RequestID  string    `json:"request_id"`
	Goal       string    `json:"goal"`
	Outcome    string    `json:"outcome"`
	BaselineID string    `json:"baseline_delivery_id,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

type DeliveryPage struct {
	Deliveries []DeliveryEntry `json:"deliveries"`
	Total      int64           `json:"total"`
	NextCursor string          `json:"next_cursor,omitempty"`
}

type Source struct {
	Title           string         `json:"title,omitempty"`
	ID              string         `json:"source_id"`
	Kind            string         `json:"kind"`
	Role            string         `json:"role,omitempty"`
	State           string         `json:"state,omitempty"`
	Description     string         `json:"description"`
	EntryURL        string         `json:"entry_url,omitempty"`
	Version         string         `json:"version,omitempty"`
	InputSchema     map[string]any `json:"input_schema,omitempty"`
	InputExample    map[string]any `json:"input_example,omitempty"`
	OutputSchema    map[string]any `json:"output_schema,omitempty"`
	CanExecute      bool           `json:"can_execute"`
	SavedDocuments  int            `json:"saved_documents"`
	LatestCaptureAt *time.Time     `json:"latest_capture_at,omitempty"`
	Issue           string         `json:"issue,omitempty"`
}

type SourcePage struct {
	Counts     map[string]int `json:"counts,omitempty"`
	Sources    []Source       `json:"sources"`
	Discovery  []Source       `json:"discovery_capabilities,omitempty"`
	Total      int            `json:"total_sources"`
	NextCursor string         `json:"next_cursor,omitempty"`
}

type EvidenceInput struct {
	Query       string   `json:"query" jsonschema:"The fact, topic or version-specific question to find evidence for"`
	SourceIDs   []string `json:"source_ids,omitempty" jsonschema:"Optional saved source IDs from the catalog"`
	DocumentIDs []string `json:"document_ids,omitempty" jsonschema:"Optional document IDs from earlier evidence results"`
	Cursor      string   `json:"cursor,omitempty" jsonschema:"Use the returned next_cursor without changing the query or scope"`
	PageSize    int      `json:"page_size,omitempty" jsonschema:"Passages per page; omitted means 20"`
}

type EvidenceIDs struct {
	IDs        []string `json:"evidence_ids,omitempty" jsonschema:"Actual evidence IDs from evidence results. Alternatively provide document_id and revision from a saved data row."`
	DocumentID string   `json:"document_id,omitempty" jsonschema:"doc_id from evidence_documents; read together with its exact revision instead of evidence_ids."`
	Revision   string   `json:"revision,omitempty"`
	Cursor     string   `json:"cursor,omitempty"`
	PageSize   int      `json:"page_size,omitempty"`
}

type CitedEvidence struct {
	knowledge.EvidenceSnapshot
	SnapshotURL string `json:"snapshot_url"`
}

type EvidenceResult struct {
	Entries        []CitedEvidence `json:"entries"`
	Total          int             `json:"total_passages"`
	CorpusRevision string          `json:"corpus_revision,omitempty"`
	NextCursor     string          `json:"next_cursor,omitempty"`
}

type DataInput struct {
	SourceID string `json:"source_id,omitempty" form:"source_id" jsonschema:"Source ID returned by gorelay_find_sources"`
	JobID    string `json:"job_id,omitempty" form:"job_id" jsonschema:"An existing acquisition run ID; narrows rows to that job"`
	Cursor   string `json:"cursor,omitempty" form:"cursor" jsonschema:"The next_cursor from the preceding data page"`
	PageSize int    `json:"page_size,omitempty" form:"page_size" jsonschema:"Rows per page; omitted means 20 with all remaining rows available via next_cursor"`
}

type DocumentReference struct {
	ID          string     `json:"doc_id"`
	Revision    string     `json:"revision"`
	URL         string     `json:"url"`
	SnapshotURL string     `json:"snapshot_url" jsonschema:"Open this exact saved document revision; usable in ordinary Markdown citations."`
	Origin      string     `json:"origin"`
	FetchedAt   *time.Time `json:"fetched_at,omitempty"`
}

type FieldSupport struct {
	Status         string         `json:"status" jsonschema:"supported means a mechanical match to captured source content, not a semantic truth guarantee."`
	DiagnosticCode string         `json:"diagnostic_code,omitempty"`
	ActualLocator  map[string]any `json:"actual_locator,omitempty" jsonschema:"DOM selector, JSON pointer or declared source-backed calculation used to check this field."`
	SourceDigest   string         `json:"source_digest,omitempty"`
	SourceDigests  []string       `json:"source_digests,omitempty"`
	FullSource     bool           `json:"full_source,omitempty"`
	CheckerVersion string         `json:"checker_version"`
	EvidenceRefs   []string       `json:"evidence_refs" jsonschema:"Source-locator references only. These are not evidence IDs accepted by gorelay_read_evidence; find quotable passages using evidence_documents."`
	Reason         string         `json:"reason,omitempty"`
}

type RecordProvenance struct {
	SourceID     string                  `json:"source_id"`
	URL          string                  `json:"url"`
	FetchedAt    *time.Time              `json:"fetched_at,omitempty" jsonschema:"Actual source acquisition time, never the time this saved row was read."`
	PublishedAt  *time.Time              `json:"published_at,omitempty"`
	DataVersion  string                  `json:"data_version" jsonschema:"Source program version. This does not prove current source content or freshness."`
	TaskID       uint64                  `json:"task_id"`
	FieldSupport map[string]FieldSupport `json:"field_support,omitempty"`
}

type DataRecord struct {
	Key           string              `json:"key"`
	SchemaVersion string              `json:"schema_version"`
	Fields        map[string]any      `json:"fields"`
	Provenance    RecordProvenance    `json:"provenance"`
	Documents     []DocumentReference `json:"evidence_documents"`
}

type DataPage struct {
	Records      []DataRecord `json:"records"`
	Total        int          `json:"total_records"`
	DataRevision string       `json:"data_revision"`
	NextCursor   string       `json:"next_cursor,omitempty"`
}
