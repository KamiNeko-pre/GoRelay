package connector

import "time"

const (
	KindData      = "data"
	KindDiscovery = "discovery"

	ExecutorHTTPAPI    = "http_api"
	ExecutorLuaSandbox = "lua_sandbox"

	StatusActive   = "active"
	StatusDegraded = "degraded"
	StatusDisabled = "disabled"

	HitCacheHit  = "cache_hit"
	HitAPIReuse  = "api_reuse"
	HitLuaReuse  = "lua_reuse"
	HitColdBuild = "cold_build"
	HitRepair    = "repair"
)

// Connector is the deterministic directory entry stored in MySQL.
type Connector struct {
	SourceID            string     `json:"source_id" gorm:"primaryKey;size:64"`
	Kind                string     `json:"kind" gorm:"size:16;not null;default:'data';index"`
	Executor            string     `json:"executor" gorm:"size:16;not null;index"`
	Domain              string     `json:"domain" gorm:"size:128;index"`
	URLPattern          string     `json:"url_pattern" gorm:"size:512"`
	Description         string     `json:"description" gorm:"type:text"`
	ActiveVersion       int        `json:"active_version" gorm:"not null;default:0"`
	Status              string     `json:"status" gorm:"size:16;not null;default:'active';index"`
	ConsecutiveFailures int        `json:"consecutive_failures" gorm:"not null;default:0"`
	ReuseCount          int        `json:"reuse_count" gorm:"not null;default:0"`
	LastVerifiedAt      *time.Time `json:"last_verified_at,omitempty" gorm:"index"`
	LastUsedAt          *time.Time `json:"last_used_at,omitempty"`
	LastDriftReason     string     `json:"last_drift_reason,omitempty" gorm:"type:text"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
}

func (Connector) TableName() string { return "connectors" }

// ConnectorVersion holds an immutable published specification or Lua script.
type ConnectorVersion struct {
	ID             uint64    `json:"id" gorm:"primaryKey;autoIncrement"`
	SourceID       string    `json:"source_id" gorm:"size:64;not null;uniqueIndex:idx_conn_ver,priority:1"`
	Version        int       `json:"version" gorm:"not null;uniqueIndex:idx_conn_ver,priority:2"`
	Executor       string    `json:"executor" gorm:"size:16;not null"`
	ArtifactHash   string    `json:"artifact_hash" gorm:"size:64;not null"`
	SpecOrCode     string    `json:"spec_or_code" gorm:"type:longtext;not null"`
	InputSchema    string    `json:"input_schema,omitempty" gorm:"type:text"`
	OutputSchema   string    `json:"output_schema" gorm:"type:text;not null"`
	AuthProfileRef string    `json:"auth_profile_ref,omitempty" gorm:"size:64"`
	CreatedAt      time.Time `json:"created_at"`
}

func (ConnectorVersion) TableName() string { return "connector_versions" }

// DriftEvent records silent schema/soft-block degradations for host self-healing.
type DriftEvent struct {
	ID                uint64    `json:"id" gorm:"primaryKey;autoIncrement"`
	SourceID          string    `json:"source_id" gorm:"size:64;index"`
	ConnectorVersion  int       `json:"connector_version"`
	JobID             uint64    `json:"job_id" gorm:"index"`
	Code              string    `json:"code" gorm:"size:64"`
	Reason            string    `json:"reason" gorm:"type:text"`
	SampleJSON        string    `json:"sample_json,omitempty" gorm:"type:longtext"`
	ResolvedByVersion int       `json:"resolved_by_version" gorm:"not null;default:0"`
	CreatedAt         time.Time `json:"created_at"`
}

func (DriftEvent) TableName() string { return "drift_events" }

// ExecutionMetric tracks hit types, latency, and drift frequency for A/B benchmarking.
type ExecutionMetric struct {
	ID            uint64    `json:"id" gorm:"primaryKey;autoIncrement"`
	SourceID      string    `json:"source_id" gorm:"size:64;index"`
	JobID         uint64    `json:"job_id" gorm:"index"`
	HitType       string    `json:"hit_type" gorm:"size:32;index"`
	Executor      string    `json:"executor" gorm:"size:32"`
	LatencyMS     int64     `json:"latency_ms"`
	SavedRows     int       `json:"saved_rows"`
	DriftDetected bool      `json:"drift_detected"`
	CreatedAt     time.Time `json:"created_at"`
}

func (ExecutionMetric) TableName() string { return "execution_metrics" }

// APISpec defines a declarative HTTP JSON API connector without Lua code.
type APISpec struct {
	URLTemplate    string            `json:"url_template"`
	Method         string            `json:"method,omitempty"`
	Headers        map[string]string `json:"headers,omitempty"`
	BodyTemplate   string            `json:"body_template,omitempty"`
	ItemsPath      string            `json:"items_path,omitempty"`
	FieldMapping   map[string]string `json:"field_mapping,omitempty"`
	Pagination     string            `json:"pagination,omitempty"` // none | page | cursor | offset
	PageParam      string            `json:"page_param,omitempty"`
	NextCursorPath string            `json:"next_cursor_path,omitempty"`
	MaxPages       int               `json:"max_pages,omitempty"`
}
