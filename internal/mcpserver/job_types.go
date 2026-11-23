package mcpserver

import "time"

type CollectInput struct {
	RefreshID   string         `json:"refresh_id,omitempty" jsonschema:"Existing server-issued refresh cycle from an explicit update request. Never invent a value to repeat work."`
	WorkspaceID string         `json:"workspace_id,omitempty" jsonschema:"Reuse the source workspace for this user goal; otherwise include goal to open it."`
	Goal        string         `json:"goal,omitempty" jsonschema:"The user's goal when starting without a workspace_id."`
	SourceID    string         `json:"source_id" jsonschema:"Executable source_id from the catalog or successful publication."`
	Version     string         `json:"version,omitempty" jsonschema:"Published version from the catalog. Omit to select and pin the active version."`
	URL         string         `json:"url" jsonschema:"Observed target URL for TARGET_URL."`
	Params      map[string]any `json:"params,omitempty" jsonschema:"Real acquisition parameters matching the source input schema."`
}

type JobInput struct {
	JobID string `json:"job_id" jsonschema:"Persistent job_id returned by collection, or a source workspace_id to inspect saved building activity."`
}

type SourceTask struct {
	TaskID       uint64 `json:"task_id"`
	Status       string `json:"status"`
	FailureCode  string `json:"failure_code,omitempty"`
	Error        string `json:"error,omitempty"`
	RecordsSaved int    `json:"records_saved"`
}

type SourceActivity struct {
	Handle string `json:"handle,omitempty"`
	Tool   string `json:"tool"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

type SourceJob struct {
	JoinKeys      []string         `json:"join_keys,omitempty"`
	Goal          string           `json:"goal,omitempty"`
	URL           string           `json:"url,omitempty"`
	Params        map[string]any   `json:"params,omitempty"`
	RefreshID     string           `json:"refresh_id,omitempty"`
	PreviousJobID string           `json:"previous_job_id,omitempty"`
	Failure       *ToolFailure     `json:"failure,omitempty"`
	SourceChecks  []any            `json:"source_checks,omitempty"`
	JobID         string           `json:"job_id"`
	WorkspaceID   string           `json:"workspace_id,omitempty"`
	Status        string           `json:"status"`
	Terminal      bool             `json:"terminal"`
	SourceID      string           `json:"source_id,omitempty"`
	Version       string           `json:"version,omitempty"`
	RecordsSaved  int              `json:"records_saved"`
	Tasks         []SourceTask     `json:"tasks,omitempty"`
	Activities    []SourceActivity `json:"activities,omitempty"`
	Error         string           `json:"error,omitempty"`
	Notices       []string         `json:"notices,omitempty"`
	CreatedAt     time.Time        `json:"created_at"`
	UpdatedAt     time.Time        `json:"updated_at"`
	ReadData      map[string]any   `json:"read_data,omitempty"`
}
