package mcpserver

import (
	"encoding/json"

	"gorelay/internal/connector"
	"gorelay/internal/pipeline"
	"gorelay/internal/probe"
)

const ContractVersion = "2"

type ToolFailure struct {
	Code          string         `json:"code"`
	Message       string         `json:"message"`
	RecoverySteps []string       `json:"recovery_steps"`
	Details       map[string]any `json:"details,omitempty"`
}

func (f *ToolFailure) Error() string { return f.Message }

type ToolReply[T any] struct {
	SchemaVersion string       `json:"schema_version"`
	Data          *T           `json:"data,omitempty"`
	Error         *ToolFailure `json:"error,omitempty"`
}

type ListSourcesInput struct {
	Domain          string `json:"domain,omitempty" jsonschema:"Exact hostname filter, e.g. api.github.com."`
	URL             string `json:"url,omitempty" jsonschema:"Optional target URL whose domain will be extracted for matching."`
	Keyword         string `json:"keyword,omitempty" jsonschema:"Optional deterministic substring filter across source_id, description, and url_pattern."`
	Kind            string `json:"kind,omitempty" jsonschema:"Filter by connector kind: data or discovery."`
	FreshnessTTLSec int    `json:"freshness_ttl_sec,omitempty" jsonschema:"Optional freshness threshold in seconds; marks fresh_cache_available if verified within TTL."`
	Cursor          string `json:"cursor,omitempty" jsonschema:"next_cursor from the previous page."`
	PageSize        int    `json:"page_size,omitempty" jsonschema:"Connectors per page (default 20, max 50)."`
}

type ListSourcesOutput = connector.ListSourcesPage

type ProbeSourceInput struct {
	URL         string `json:"url" jsonschema:"Public HTTP(S) target URL to probe."`
	DetectAPI   bool   `json:"detect_api,omitempty" jsonschema:"Inspect JSON endpoints, embedded HTML JSON, and optional XHR API captures before falling back to DOM selectors."`
	UseBrowser  string `json:"use_browser,omitempty" jsonschema:"Optional dynamic rendering policy: auto (default), always, or never."`
	AuthProfile string `json:"auth_profile,omitempty" jsonschema:"Optional configured auth_profile name allowed for this domain."`
}

type ProbeSourceOutput = probe.ProbeResult

type DeployAndRunInput struct {
	SourceID     string          `json:"source_id" jsonschema:"Stable semantic connector ID, e.g. github_go_releases."`
	Kind         string          `json:"kind,omitempty" jsonschema:"data (default) or discovery (recursive URL discovery subtasks)."`
	Executor     string          `json:"executor" jsonschema:"http_api (declarative JSON API without Lua) or lua_sandbox (gopher-lua script)."`
	Description  string          `json:"description,omitempty" jsonschema:"Concise description of what this connector retrieves and outputs."`
	TargetURL    string          `json:"target_url,omitempty" jsonschema:"Target public URL (optional for http_api if url_template includes host)."`
	SpecOrCode   string          `json:"spec_or_code" jsonschema:"JSON APISpec string for executor=http_api, or complete Lua program for executor=lua_sandbox."`
	InputSchema  json.RawMessage `json:"input_schema,omitempty" jsonschema:"Optional JSON object schema for params; inferred from params when omitted."`
	OutputSchema json.RawMessage `json:"output_schema" jsonschema:"Output schema JSON: {fields:[{name,type,required}], entity_keys:[...], min_rows}."`
	Params       map[string]any  `json:"params,omitempty" jsonschema:"Runtime parameters passed to URL template placeholders or Lua PARAMS."`
	AuthProfile  string          `json:"auth_profile,omitempty" jsonschema:"Optional auth_profile name for authorized endpoints."`
	WaitMS       int             `json:"wait_ms,omitempty" jsonschema:"Milliseconds to wait for synchronous fast-path completion (e.g. 3000) before returning job_id."`
	RollbackTo   int             `json:"rollback_to,omitempty" jsonschema:"Optional previous version integer to restore active pointer immediately instead of deploying new code."`
}

type RunConnectorInput struct {
	SourceID        string         `json:"source_id" jsonschema:"Published connector source_id from gorelay_list_sources."`
	TargetURL       string         `json:"target_url,omitempty" jsonschema:"Optional URL override matching the connector target."`
	Params          map[string]any `json:"params,omitempty" jsonschema:"Parameters satisfying the connector input_schema."`
	FreshnessTTLSec int            `json:"freshness_ttl_sec,omitempty" jsonschema:"Return cached records with 0 network calls if verified within TTL seconds."`
	WaitMS          int            `json:"wait_ms,omitempty" jsonschema:"Short wait budget in ms to return cleaned JSON in one step if job finishes quickly."`
	ForceRefresh    bool           `json:"force_refresh,omitempty" jsonschema:"Bypass freshness cache and enqueue a fresh run."`
}

type ExecutionOutput = connector.ExecutionResponse

type JobDataInput struct {
	JobID  uint64 `json:"job_id" jsonschema:"Durable job_id returned by gorelay_deploy_and_run or gorelay_run_connector."`
	Cursor string `json:"cursor,omitempty" jsonschema:"Pagination cursor returned in data.next_cursor."`
	Cancel bool   `json:"cancel,omitempty" jsonschema:"Set true to cancel an in-flight job and terminate its sandbox process."`
}

type SearchEvidenceInput struct {
	SourceIDs   []string `json:"source_ids,omitempty" jsonschema:"Explicit connector source_ids to restrict BM25 search over archived snapshots."`
	Query       string   `json:"query,omitempty" jsonschema:"Search keywords for BM25 passage ranking inside source_ids."`
	SnapshotRef string   `json:"snapshot_ref,omitempty" jsonschema:"Exact SHA-256 snapshot_ref to read paginated archived text directly."`
	Cursor      string   `json:"cursor,omitempty" jsonschema:"Pagination cursor returned in next_cursor."`
	PageSize    int      `json:"page_size,omitempty" jsonschema:"Number of hits per page (default 10, max 20)."`
}

type SearchEvidenceOutput = pipeline.EvidencePage
