package mcpserver

import (
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
	Domain          string `json:"domain,omitempty" jsonschema:"Filter by exact hostname, e.g. api.github.com."`
	URL             string `json:"url,omitempty" jsonschema:"Filter by target URL hostname."`
	Keyword         string `json:"keyword,omitempty" jsonschema:"Substring filter across source_id, description, and url_pattern."`
	Kind            string `json:"kind,omitempty" jsonschema:"Filter by kind: data or discovery."`
	FreshnessTTLSec int    `json:"freshness_ttl_sec,omitempty" jsonschema:"Freshness window in seconds (default 3600)."`
	Cursor          string `json:"cursor,omitempty" jsonschema:"Pagination cursor from previous response."`
	PageSize        int    `json:"page_size,omitempty" jsonschema:"Results per page (default 20, max 50)."`
}

type ListSourcesOutput = connector.ListSourcesPage

type ProbeSourceInput struct {
	URL         string `json:"url" jsonschema:"Public HTTP(S) URL to inspect."`
	DetectAPI   bool   `json:"detect_api,omitempty" jsonschema:"Sniff JSON endpoints and embedded JSON in HTML."`
	UseBrowser  string `json:"use_browser,omitempty" jsonschema:"Headless browser policy: auto (default), always, or never."`
	AuthProfile string `json:"auth_profile,omitempty" jsonschema:"Optional configured auth_profile name for this domain."`
}

type ProbeSourceOutput = probe.ProbeResult

type APISpecInput struct {
	URLTemplate    string            `json:"url_template" jsonschema:"Target API URL, e.g. 'https://api.example.com/items?per_page=5'."`
	Method         string            `json:"method,omitempty" jsonschema:"HTTP method (default GET)."`
	Headers        map[string]string `json:"headers,omitempty" jsonschema:"Optional request headers."`
	BodyTemplate   string            `json:"body_template,omitempty" jsonschema:"Optional POST/PUT body."`
	ItemsPath      string            `json:"items_path,omitempty" jsonschema:"JSON pointer to item array, e.g. '/data/items' ('' for root array)."`
	FieldMapping   map[string]string `json:"field_mapping,omitempty" jsonschema:"output_field -> JSON pointer map (omit to keep all fields)."`
	Pagination     string            `json:"pagination,omitempty" jsonschema:"Pagination mode; auto-merges page_param with existing URL query params."`
	PageParam      string            `json:"page_param,omitempty" jsonschema:"Pagination query param name (default page/offset/cursor)."`
	NextCursorPath string            `json:"next_cursor_path,omitempty" jsonschema:"JSON pointer to next cursor when pagination='cursor'."`
	MaxPages       int               `json:"max_pages,omitempty" jsonschema:"Max pages to fetch (default 5, max 20)."`
}

type OutputFieldInput struct {
	Name     string `json:"name" jsonschema:"Field name."`
	Type     string `json:"type" jsonschema:"Value type."`
	Required bool   `json:"required,omitempty" jsonschema:"Fail validation if missing/empty."`
}

type OutputSchemaInput struct {
	Fields     []OutputFieldInput `json:"fields" jsonschema:"Output field definitions."`
	EntityKeys []string           `json:"entity_keys,omitempty" jsonschema:"Unique key field(s) for deduplication."`
	MinRows    int                `json:"min_rows,omitempty" jsonschema:"Min expected total rows across all pages."`
}

type FilterConditionInput struct {
	Field string `json:"field" jsonschema:"Field name."`
	Op    string `json:"op,omitempty" jsonschema:"Comparison operator (default eq)."`
	Value any    `json:"value" jsonschema:"Compare value."`
}

type URLExtractInput struct {
	Field   string `json:"field" jsonschema:"Text field to scan (or '*') before truncation."`
	Pattern string `json:"pattern,omitempty" jsonschema:"Case-insensitive URL substring (omit for all URLs)."`
	Regex   string `json:"regex,omitempty" jsonschema:"Optional Go regex filter."`
}

type RecordQueryInput struct {
	Fields        []string               `json:"fields,omitempty" jsonschema:"Fields to return per row."`
	ExcludeFields []string               `json:"exclude_fields,omitempty" jsonschema:"Fields to omit."`
	MaxFieldChars int                    `json:"max_field_chars,omitempty" jsonschema:"Max chars per string (default 240, max 1024)."`
	Where         []FilterConditionInput `json:"where,omitempty" jsonschema:"AND filters, e.g. [{field:'price', op:'gt', value:50}]."`
	OrderBy       string                 `json:"order_by,omitempty" jsonschema:"Sort field; prefix '-' for desc, e.g. '-price' or 'published_at'."`
	Limit         int                    `json:"limit,omitempty" jsonschema:"Max rows returned (matched still reports full count)."`
	GroupBy       string                 `json:"group_by,omitempty" jsonschema:"Group-count field (omits rows unless fields/order_by set)."`
	GroupPrefix   int                    `json:"group_prefix,omitempty" jsonschema:"Prefix length for group_by keys, e.g. 4 turns 'v3.7.4' into 'v3.7'."`
	ExtractURLs   *URLExtractInput       `json:"extract_urls,omitempty" jsonschema:"Extract URLs + markdown url_labels across matched rows (omits rows unless fields/order_by set), e.g. {field:'body'}."`
}

type DeployAndRunInput struct {
	SourceID     string                    `json:"source_id" jsonschema:"Connector ID, e.g. 'github_releases'."`
	Kind         string                    `json:"kind,omitempty" jsonschema:"Connector kind (default data)."`
	Executor     string                    `json:"executor,omitempty" jsonschema:"Executor type (auto-inferred from api_spec/spec_or_code if omitted)."`
	Description  string                    `json:"description,omitempty" jsonschema:"Short description."`
	TargetURL    string                    `json:"target_url,omitempty" jsonschema:"Target URL (required for lua_sandbox; optional if api_spec.url_template is set)."`
	APISpec      *APISpecInput             `json:"api_spec,omitempty" jsonschema:"Plain JSON object for http_api (do not stringify)."`
	SpecOrCode   string                    `json:"spec_or_code,omitempty" jsonschema:"Lua script for lua_sandbox (or JSON string for http_api). Globals: TARGET_URL, PARAMS. Helpers: http_request({url,method,headers,body})->{ok,status,final_url,body,headers}; http_render(url[,sel]); json_decode(s); html_find(h,css); html_find_all(h,css); html_attr_all(h,css,attr); clean_text(s); url_resolve(b,r); checkpoint.get/save. Set source_url=resp.final_url per item on multi-page scrapes; end with 'return true, {items=records}'."`
	InputSchema  map[string]any            `json:"input_schema,omitempty" jsonschema:"Optional runtime params schema (auto-inferred if omitted)."`
	OutputSchema *OutputSchemaInput        `json:"output_schema,omitempty" jsonschema:"Output schema (multi-page deploys validate page 1 first, then full min_rows across pages). Required unless rollback_to is set."`
	Params       map[string]any            `json:"params,omitempty" jsonschema:"Optional runtime params."`
	AuthProfile  string                    `json:"auth_profile,omitempty" jsonschema:"Optional auth_profile name."`
	WaitMS       int                       `json:"wait_ms,omitempty" jsonschema:"Sync wait timeout in ms (default 5000)."`
	RollbackTo   int                       `json:"rollback_to,omitempty" jsonschema:"Restore this prior version number instead of deploying new code."`
	Query        *RecordQueryInput         `json:"query,omitempty" jsonschema:"Single server-side view over saved rows. Omit for compact receipt + export_urls."`
	Queries      map[string]map[string]any `json:"queries,omitempty" jsonschema:"Up to 4 named views (name -> same schema as query), returned in data.views."`
}

type RunConnectorInput struct {
	SourceID        string                    `json:"source_id" jsonschema:"Connector source_id from 'Active connectors' or gorelay_list_sources."`
	Query           *RecordQueryInput         `json:"query,omitempty" jsonschema:"Single server-side view over rows. Omit for compact receipt + export_urls."`
	Queries         map[string]map[string]any `json:"queries,omitempty" jsonschema:"Up to 4 named views (name -> same schema as query), returned in data.views."`
	TargetURL       string                    `json:"target_url,omitempty" jsonschema:"Isolated URL override; bypasses cache, never modifies stored connector version or degrades it on error."`
	Params          map[string]any            `json:"params,omitempty" jsonschema:"Optional runtime params."`
	FreshnessTTLSec int                       `json:"freshness_ttl_sec,omitempty" jsonschema:"Cache freshness window in seconds (default 3600 for instant 0-HTTP reuse)."`
	ForceRefresh    bool                      `json:"force_refresh,omitempty" jsonschema:"Set true to bypass cache and fetch live."`
	WaitMS          int                       `json:"wait_ms,omitempty" jsonschema:"Sync wait timeout in ms when fetching live (default 5000)."`
}

type ExecutionOutput = connector.ExecutionResponse

type JobDataInput struct {
	JobID    uint64                    `json:"job_id,omitempty" jsonschema:"Job ID from deploy_and_run or run_connector."`
	SourceID string                    `json:"source_id,omitempty" jsonschema:"Connector source_id to query cached rows without job_id."`
	Query    *RecordQueryInput         `json:"query,omitempty" jsonschema:"Single server-side view over rows."`
	Queries  map[string]map[string]any `json:"queries,omitempty" jsonschema:"Up to 4 named views (name -> same schema as query), returned in data.views."`
	Cursor   string                    `json:"cursor,omitempty" jsonschema:"Next cursor from a truncated query."`
	Cancel   bool                      `json:"cancel,omitempty" jsonschema:"Set true to cancel a running job."`
}

type SearchEvidenceInput struct {
	SourceIDs   []string `json:"source_ids,omitempty" jsonschema:"Connector source_ids to search across archived raw responses."`
	Query       string   `json:"query,omitempty" jsonschema:"Search keywords for ranking archived excerpts."`
	SnapshotRef string   `json:"snapshot_ref,omitempty" jsonschema:"Exact SHA-256 snapshot_ref to read a compact paginated excerpt (1200 runes/page)."`
	Cursor      string   `json:"cursor,omitempty" jsonschema:"Pagination cursor from previous response."`
	PageSize    int      `json:"page_size,omitempty" jsonschema:"Hits per page (default 5, max 10)."`
}

type SearchEvidenceOutput = pipeline.EvidencePage
