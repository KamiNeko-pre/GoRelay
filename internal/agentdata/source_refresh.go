package agentdata

import "time"

// SourceRefresh records a user's new observation cycle, separately from tool calls.
type SourceRefresh struct {
	ID            string    `json:"id" gorm:"primaryKey;size:32"`
	RequestKey    string    `json:"-" gorm:"size:128;uniqueIndex"`
	SourceID      string    `json:"source_id" gorm:"size:128;index"`
	WorkspaceID   string    `json:"workspace_id" gorm:"size:32"`
	PreviousJobID string    `json:"previous_job_id" gorm:"size:64;index"`
	JobID         string    `json:"job_id" gorm:"size:64;index"`
	InputJSON     string    `json:"-" gorm:"type:longtext"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func (SourceRefresh) TableName() string { return "information_source_refreshes" }
