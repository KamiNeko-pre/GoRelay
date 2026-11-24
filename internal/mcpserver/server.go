package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Backend interface {
	ListSources(context.Context, ListSourcesInput) (ListSourcesOutput, error)
	ProbeSource(context.Context, ProbeSourceInput) (ProbeSourceOutput, error)
	DeployAndRun(context.Context, DeployAndRunInput) (ExecutionOutput, error)
	RunConnector(context.Context, RunConnectorInput) (ExecutionOutput, error)
	GetJobData(context.Context, JobDataInput) (ExecutionOutput, error)
	SearchEvidence(context.Context, SearchEvidenceInput) (SearchEvidenceOutput, error)
}

var defaultRecovery = map[string][]string{
	"gorelay_list_sources": {
		"Provide exact domain/url/kind filters or omit all fields to browse active connectors.",
		"Use gorelay_probe_source on discovered public URLs if no matching connector exists; for unknown targets, run a kind=discovery connector first.",
	},
	"gorelay_probe_source": {
		"Provide a valid public HTTP(S) url. Private/loopback IPs and disallowed robots.txt paths are rejected by SafeClient.",
		"If an API candidate is returned (recommended_executor=http_api), prefer gorelay_deploy_and_run with executor=http_api.",
	},
	"gorelay_deploy_and_run": {
		"Fix the reported JSON APISpec, Lua syntax/runtime error, or output_schema validation failure; failed trial runs never overwrite active versions.",
		"Set rollback_to to restore a previous healthy version.",
	},
	"gorelay_run_connector": {
		"Check params against input_schema from gorelay_list_sources.",
		"If the connector is marked degraded due to upstream schema drift, inspect drift diagnosis and deploy a new version with gorelay_deploy_and_run.",
	},
	"gorelay_get_job_data": {
		"Supply a job_id from deploy_and_run/run_connector, or a source_id with query/queries to read cached rows.",
		"Use export_urls in data for full CSV/JSON/Markdown downloads instead of paging raw records.",
	},
	"gorelay_search_evidence": {
		"Supply concrete source_ids for scoped BM25 search, or an exact snapshot_ref from record provenance.",
	},
}

func failure(err error, name, code string) *ToolFailure {
	var known *ToolFailure
	if errors.As(err, &known) {
		return known
	}
	return &ToolFailure{
		Code:          code,
		Message:       err.Error(),
		RecoverySteps: defaultRecovery[name],
	}
}

var compactOutputSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"schema_version": map[string]any{"type": "string"},
		"data":           map[string]any{"type": "object"},
		"error":          map[string]any{"type": "object"},
	},
}

var propertyEnums = map[string][]any{
	"kind":        {"data", "discovery"},
	"executor":    {"http_api", "lua_sandbox"},
	"use_browser": {"auto", "always", "never"},
	"pagination":  {"none", "page", "offset", "cursor"},
	"op":          {"eq", "ne", "gt", "gte", "lt", "lte", "contains", "icontains", "prefix", "suffix", "regex"},
}

func attachSchemaEnums(s *jsonschema.Schema, parentProp string) {
	if s == nil {
		return
	}
	for name, prop := range s.Properties {
		if prop == nil {
			continue
		}
		if enums, ok := propertyEnums[name]; ok && prop.Type == "string" {
			prop.Enum = enums
		} else if name == "type" && parentProp == "fields" && prop.Type == "string" {
			prop.Enum = []any{"string", "number", "integer", "boolean", "string_array"}
		}
		attachSchemaEnums(prop, name)
	}
	if s.Items != nil {
		attachSchemaEnums(s.Items, parentProp)
	}
}

func register[In, Out any](server *mcp.Server, name, description string, readOnly bool, handler func(context.Context, In) (Out, error)) {
	var inputSchema *jsonschema.Schema
	if s, err := jsonschema.For[In](nil); err == nil {
		attachSchemaEnums(s, "")
		inputSchema = s
	}
	mcp.AddTool(server, &mcp.Tool{
		Name:         name,
		Description:  description,
		InputSchema:  inputSchema,
		OutputSchema: compactOutputSchema,
		Annotations:  &mcp.ToolAnnotations{ReadOnlyHint: readOnly, IdempotentHint: readOnly},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, ToolReply[Out], error) {
		out, err := handler(ctx, in)
		if err != nil {
			return &mcp.CallToolResult{IsError: true}, ToolReply[Out]{SchemaVersion: ContractVersion, Data: &out, Error: failure(err, name, "TOOL_FAILED")}, nil
		}
		return nil, ToolReply[Out]{SchemaVersion: ContractVersion, Data: &out}, nil
	})
}

const serverInstructions = `GoRelay extracts structured web data into cached tables (never returns raw pages). Call independent tools concurrently.

Routing:
- Target in "Active connectors" below -> call gorelay_run_connector (instant 1h cache hit, 0 HTTP). Skip gorelay_list_sources.
- Known API or HTML target -> call gorelay_deploy_and_run directly (skip gorelay_probe_source).
- Unknown layout, dynamic SPA, or robots/SSRF check -> call gorelay_probe_source.
- Call gorelay_search_evidence ONLY if needed text is absent from structured fields (per-page SHA-256 hashes are already in data.snapshot_refs, indexed by row._snapshot_idx when >1 page).

Contracts:
- data.total and view matched always return full counts regardless of limit; pure group_by/extract_urls views omit rows and run over full untruncated values (extract_urls also returns markdown url_labels).
- target_url on gorelay_run_connector is isolated and read-only (never alters or degrades the stored connector; safe in parallel with cached reads).`

func extractOutputFieldSpecs(schema any) []string {
	m, ok := schema.(map[string]any)
	if !ok {
		return nil
	}
	rawFields, ok := m["fields"].([]any)
	if !ok {
		return nil
	}
	specs := make([]string, 0, len(rawFields))
	for _, item := range rawFields {
		fm, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, _ := fm["name"].(string)
		if name == "" {
			continue
		}
		typ, _ := fm["type"].(string)
		switch typ {
		case "string", "source_text":
			specs = append(specs, name+":str")
		case "number":
			specs = append(specs, name+":num")
		case "integer":
			specs = append(specs, name+":int")
		case "boolean":
			specs = append(specs, name+":bool")
		case "string_array":
			specs = append(specs, name+":str[]")
		default:
			specs = append(specs, name)
		}
	}
	return specs
}

func buildLiveInstructions(ctx context.Context, backend Backend) string {
	if backend == nil {
		return serverInstructions
	}
	if ctx == nil {
		ctx = context.Background()
	}
	qctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	page, err := backend.ListSources(qctx, ListSourcesInput{PageSize: 20, FreshnessTTLSec: 3600})
	if err != nil {
		return serverInstructions
	}
	var b strings.Builder
	b.WriteString(serverInstructions)
	if page.Total == 0 || len(page.Connectors) == 0 {
		b.WriteString("\n\nActive connectors (0): none yet.")
		return b.String()
	}
	fmt.Fprintf(&b, "\n\nActive connectors (%d of %d):", len(page.Connectors), page.Total)
	for _, c := range page.Connectors {
		freshStr := "stale"
		if c.FreshCacheAvailable {
			freshStr = "fresh"
		}
		fields := extractOutputFieldSpecs(c.OutputSchema)
		fieldStr := ""
		if len(fields) > 0 {
			fieldStr = " fields=[" + strings.Join(fields, ",") + "]"
		}
		fmt.Fprintf(&b, "\n- %s [%s, %d rows, %s]%s url=%s",
			c.SourceID, c.Executor, c.CachedRecords, freshStr, fieldStr, c.URLPattern)
	}
	return b.String()
}

func NewServer(backend Backend) *mcp.Server {
	return NewServerWithContext(context.Background(), backend)
}

func NewServerWithContext(ctx context.Context, backend Backend) *mcp.Server {
	return newServerWithInstructions(backend, buildLiveInstructions(ctx, backend), nil)
}

func newServerWithInstructions(backend Backend, instructions string, schemaCache *mcp.SchemaCache) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "gorelay", Version: "2.0.0"}, &mcp.ServerOptions{
		Instructions: instructions,
		SchemaCache:  schemaCache,
	})
	register(server, "gorelay_list_sources", "Lists stored connectors and cached row counts. Skip if Active connectors in server instructions already lists your target.", true, backend.ListSources)
	register(server, "gorelay_probe_source", "Checks robots.txt/SSRF safety on a public URL, detects JSON APIs, and extracts sample DOM selectors.", true, backend.ProbeSource)
	register(server, "gorelay_deploy_and_run", "Creates or updates a connector (pass api_spec object for JSON APIs, or Lua script in spec_or_code for HTML), validates before publishing, persists cleaned rows, and returns a receipt or query/queries view. Pass rollback_to to restore a prior version.", false, backend.DeployAndRun)
	register(server, "gorelay_run_connector", "Queries a stored connector (defaults to instant 1h cache hit, 0 HTTP) with deduplicated data.snapshot_refs. Pass query or queries for server-side filtering/sorting/grouping/URL extraction, or target_url+force_refresh=true for an isolated read-only URL check.", false, backend.RunConnector)
	register(server, "gorelay_get_job_data", "Polls or cancels an in-flight job by job_id, or queries cached rows by source_id.", false, backend.GetJobData)
	register(server, "gorelay_search_evidence", "Searches archived raw response bodies by keyword within source_ids, or reads a compact excerpt by snapshot_ref. Do not call merely to retrieve snapshot_ref.", true, backend.SearchEvidence)

	server.AddResource(&mcp.Resource{
		URI:         "gorelay://catalog",
		Name:        "Active GoRelay Connectors",
		Description: "Live catalog of published GoRelay connectors, active versions, output fields, and cached record counts.",
		MIMEType:    "application/json",
	}, func(rctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		page, err := backend.ListSources(rctx, ListSourcesInput{PageSize: 50, FreshnessTTLSec: 3600})
		if err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(page)
		if err != nil {
			return nil, err
		}
		return &mcp.ReadResourceResult{
			Contents: []*mcp.ResourceContents{{
				URI:      req.Params.URI,
				MIMEType: "application/json",
				Text:     string(encoded),
			}},
		}, nil
	})

	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			result, err := next(ctx, method, req)
			call, isCall := req.(*mcp.CallToolRequest)
			toolResult, isResult := result.(*mcp.CallToolResult)
			if method != "tools/call" || !isCall || !isResult || !toolResult.IsError || toolResult.StructuredContent != nil {
				return result, err
			}
			var messages []string
			for _, content := range toolResult.Content {
				if text, ok := content.(*mcp.TextContent); ok {
					messages = append(messages, text.Text)
				}
			}
			body := ToolReply[any]{
				SchemaVersion: ContractVersion,
				Error:         failure(fmt.Errorf("%s", strings.Join(messages, "\n")), call.Params.Name, "INVALID_ARGUMENTS"),
			}
			encoded, encodeErr := json.Marshal(body)
			if encodeErr != nil {
				return nil, encodeErr
			}
			toolResult.StructuredContent = body
			toolResult.Content = []mcp.Content{&mcp.TextContent{Text: string(encoded)}}
			return toolResult, err
		}
	})
	return server
}

type requestBaseURLKey struct{}
type requestIsInitKey struct{}

func RequestBaseURL(ctx context.Context) string {
	base, _ := ctx.Value(requestBaseURLKey{}).(string)
	return base
}

func isHandshakeMethod(method string) bool {
	return method == "initialize" || method == "server/discover"
}

func isInitializeRequest(r *http.Request) bool {
	if r == nil || r.Method != http.MethodPost || r.Body == nil {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return false
	}
	if trimmed[0] == '[' {
		var batch []struct {
			Method string `json:"method"`
		}
		if json.Unmarshal(trimmed, &batch) == nil {
			for _, item := range batch {
				if isHandshakeMethod(item.Method) {
					return true
				}
			}
		}
		return false
	}
	var rpc struct {
		Method string `json:"method"`
	}
	return json.Unmarshal(trimmed, &rpc) == nil && isHandshakeMethod(rpc.Method)
}

func NewHandler(backend Backend, publicBaseURL string) http.Handler {
	schemaCache := mcp.NewSchemaCache()
	baseServer := newServerWithInstructions(backend, serverInstructions, schemaCache)
	transport := mcp.NewStreamableHTTPHandler(
		func(r *http.Request) *mcp.Server {
			if isInit, _ := r.Context().Value(requestIsInitKey{}).(bool); isInit {
				return newServerWithInstructions(backend, buildLiveInstructions(r.Context(), backend), schemaCache)
			}
			return baseServer
		},
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, PropagateRequestCancellation: true},
	)
	protected := http.NewCrossOriginProtection().Handler(transport)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := strings.TrimRight(publicBaseURL, "/")
		if base == "" {
			scheme := "http"
			if r.TLS != nil {
				scheme = "https"
			}
			base = scheme + "://" + r.Host
		}
		isInit := isInitializeRequest(r)
		ctx := context.WithValue(r.Context(), requestBaseURLKey{}, base)
		ctx = context.WithValue(ctx, requestIsInitKey{}, isInit)
		protected.ServeHTTP(w, r.WithContext(ctx))
	})
}
