package mcpserver

type InspectSourceInput struct {
	WorkspaceID string   `json:"workspace_id,omitempty" jsonschema:"Reuse the workspace_id returned by the first inspection of this user goal."`
	Goal        string   `json:"goal,omitempty" jsonschema:"The user's source acquisition goal. Required for the first inspection without workspace_id; reopening the same goal restores saved work."`
	SourceScope []string `json:"source_scope,omitempty" jsonschema:"Only caller-requested domain restrictions. Omit for ordinary public-source discovery."`
	URL         string   `json:"url,omitempty" jsonschema:"Absolute public HTTP(S) entry to inspect; provide exactly one of url and source_id."`
	SourceID    string   `json:"source_id,omitempty" jsonschema:"An existing executable source_id whose program and schemas should be inspected."`
}

type ObservationInput struct {
	WorkspaceID  string `json:"workspace_id" jsonschema:"Workspace that owns this saved observation."`
	Handle       string `json:"handle,omitempty" jsonschema:"Exact handle from a source inspection or its returned next_read."`
	Cursor       string `json:"cursor,omitempty" jsonschema:"Saved structural continuation when supplied by next_read."`
	ContinueFrom string `json:"continue_from,omitempty"`
	View         string `json:"view,omitempty" jsonschema:"dom lists real selectors or matched nodes; json reads a saved JSON node; receipt reads diagnostic metadata. Follow next_read."`
	Selector     string `json:"selector,omitempty" jsonschema:"Observed CSS selector for DOM nodes; omit to browse the actual selector catalog."`
	Pointer      string `json:"pointer,omitempty" jsonschema:"Observed JSON pointer such as /items; needed for view=json."`
	ViewVersion  string `json:"view_version,omitempty"`
	Offset       int    `json:"offset,omitempty" jsonschema:"Structural entry offset from next_read; never a text offset."`
	Limit        int    `json:"limit,omitempty"`
}

type OutputField struct {
	Name string `json:"name"`
	Type string `json:"type" jsonschema:"One of string, source_text, number, integer, boolean or string_array."`
}

type ValidateProgramInput struct {
	WorkspaceID  string         `json:"workspace_id"`
	SourceID     string         `json:"source_id" jsonschema:"A stable capability name to publish or update; not an observation handle."`
	URL          string         `json:"url" jsonschema:"Observed public target URL, provided to Lua as TARGET_URL."`
	Code         string         `json:"code" jsonschema:"Complete Lua program using the runtime described by inspection. Return true,{items={...}}; every item needs a fetched source_url. Attach _evidence field locators for verifiable extraction."`
	Purpose      string         `json:"purpose,omitempty"`
	Mode         string         `json:"mode,omitempty" jsonschema:"acquisition (default) validates a publishable source; probe explores intermediate fields without publishing."`
	Params       map[string]any `json:"params,omitempty" jsonschema:"Parameters exposed to Lua as PARAMS; credentials never belong here."`
	InputSchema  map[string]any `json:"input_schema,omitempty" jsonschema:"JSON object schema describing PARAMS. Omit to infer it from the provided values."`
	OutputFields []OutputField  `json:"output_fields" jsonschema:"Fields this source actually produces, not every column required by the user's broader task."`
	JoinKeys     []string       `json:"join_keys,omitempty" jsonschema:"Names of produced fields that identify distinct records."`
}

type PublishSourceInput struct {
	WorkspaceID string `json:"workspace_id"`
	CandidateID string `json:"candidate_id" jsonschema:"Successful acquisition validation candidate from this workspace."`
	Description string `json:"description" jsonschema:"What this source retrieves and when another task should use it."`
}

type SourceAction struct {
	WorkspaceID     string `json:"workspace_id"`
	Handle          string `json:"handle,omitempty"`
	Observation     any    `json:"observation,omitempty"`
	RuntimeGuidance string `json:"runtime_guidance,omitempty"`
}
