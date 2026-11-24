package mcpserver

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"gorelay/internal/connector"
	"gorelay/internal/pipeline"
	"gorelay/internal/probe"
)

type stubBackend struct {
	listCalls *atomic.Int32
}

func (b stubBackend) ListSources(context.Context, ListSourcesInput) (ListSourcesOutput, error) {
	if b.listCalls != nil {
		b.listCalls.Add(1)
	}
	now := time.Now().UTC()
	return ListSourcesOutput{
		Total: 1,
		Connectors: []connector.ConnectorView{{
			SourceID:            "github_go_releases",
			Kind:                connector.KindData,
			Executor:            connector.ExecutorHTTPAPI,
			Domain:              "api.github.com",
			URLPattern:          "https://api.github.com/repos/golang/go/releases",
			ActiveVersion:       1,
			Status:              connector.StatusActive,
			CachedRecords:       12,
			FreshCacheAvailable: true,
			LastVerifiedAt:      &now,
			OutputSchema: map[string]any{"fields": []any{
				map[string]any{"name": "tag_name", "type": "string"},
				map[string]any{"name": "published_at", "type": "string"},
				map[string]any{"name": "downloads", "type": "integer"},
			}},
		}},
	}, nil
}

func (stubBackend) ProbeSource(context.Context, ProbeSourceInput) (ProbeSourceOutput, error) {
	return ProbeSourceOutput{
		URL: "https://api.github.com/repos/golang/go/releases", Kind: "json_api", RecommendedExecutor: connector.ExecutorHTTPAPI,
		LuaSandboxContract: probe.CompactLuaContract,
	}, nil
}

func (stubBackend) DeployAndRun(context.Context, DeployAndRunInput) (ExecutionOutput, error) {
	return ExecutionOutput{JobID: 101, SourceID: "github_go_releases", Version: 1, Status: "succeeded", Terminal: true}, nil
}

func (stubBackend) RunConnector(context.Context, RunConnectorInput) (ExecutionOutput, error) {
	return ExecutionOutput{JobID: 102, SourceID: "github_go_releases", Version: 1, Status: "cache_hit", HitType: connector.HitCacheHit, Terminal: true}, nil
}

func (stubBackend) GetJobData(_ context.Context, in JobDataInput) (ExecutionOutput, error) {
	if in.Cancel {
		return ExecutionOutput{JobID: in.JobID, Status: "cancelled", Terminal: true}, nil
	}
	return ExecutionOutput{JobID: in.JobID, Status: "succeeded", Terminal: true}, nil
}

func (stubBackend) SearchEvidence(ctx context.Context, in SearchEvidenceInput) (SearchEvidenceOutput, error) {
	if in.Query == "db-error" {
		return SearchEvidenceOutput{}, &ToolFailure{
			Code:          "STORAGE_UNAVAILABLE",
			Message:       "database unavailable",
			RecoverySteps: []string{"Retry after database recovery."},
		}
	}
	return SearchEvidenceOutput{
		Total: 1,
		Entries: []pipeline.EvidenceHit{{
			SnapshotRef: "abc123",
			SourceID:    "github_go_releases",
			Snippet:     "Go 1.26 released",
			SnapshotURL: RequestBaseURL(ctx) + "/evidence?ref=abc123",
			FetchedAt:   time.Now().UTC(),
		}},
	}, nil
}

func TestMCPV2SixToolsContractAndRecovery(t *testing.T) {
	var listCalls atomic.Int32
	server := httptest.NewServer(NewHandler(stubBackend{listCalls: &listCalls}, ""))
	defer server.Close()

	ctx := t.Context()
	client := mcp.NewClient(&mcp.Implementation{Name: "v2-test", Version: "2.0.0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	// Handshake Instructions must embed live catalog concisely (< 1.5 KiB) so agents skip gorelay_list_sources.
	initRes := session.InitializeResult()
	if initRes == nil || !strings.Contains(initRes.Instructions, "github_go_releases") || !strings.Contains(initRes.Instructions, "12 rows") || !strings.Contains(initRes.Instructions, "fields=[tag_name:str,published_at:str,downloads:int]") {
		t.Fatalf("expected live catalog with typed fields in initialize instructions, got: %v", initRes)
	}
	if len(initRes.Instructions) > 1536 {
		t.Fatalf("server instructions bloated to %d bytes (must stay ultra-concise < 1.5 KiB)", len(initRes.Instructions))
	}
	if listCalls.Load() < 1 {
		t.Fatalf("expected live ListSources call during handshake, got %d", listCalls.Load())
	}
	catRes, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "gorelay://catalog"})
	if err != nil || len(catRes.Contents) == 0 || !strings.Contains(catRes.Contents[0].Text, "github_go_releases") {
		t.Fatalf("expected gorelay://catalog resource, got res=%+v err=%v", catRes, err)
	}
	callsAfterResource := listCalls.Load()

	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Tools) != 6 {
		t.Fatalf("expected exactly 6 MCP tools, got %d", len(listed.Tools))
	}
	listedRaw, _ := json.Marshal(listed.Tools)
	if len(listedRaw) > 16*1024 {
		t.Fatalf("tools/list schema bloated to %d bytes (must stay compact < 16 KiB)", len(listedRaw))
	}
	if strings.Contains(string(listedRaw), `"raw_page"`) {
		t.Fatalf("raw_page raw-context backdoor must not be exposed on MCP tools: %s", listedRaw)
	}
	// Verify key nested inputs are strongly typed WITH real JSON Schema enums (not blank objects).
	for _, mustContain := range []string{`"api_spec"`, `"url_template"`, `"extract_urls"`, `"order_by"`, `"queries"`, `"entity_keys"`, `"enum":["http_api","lua_sandbox"]`, `"enum":["none","page","offset","cursor"]`, `"enum":["eq","ne",`} {
		if !strings.Contains(string(listedRaw), mustContain) {
			t.Fatalf("expected strongly-typed property/enum %s in tools/list schema", mustContain)
		}
	}
	wantNames := map[string]bool{
		"gorelay_list_sources":    true,
		"gorelay_probe_source":    true,
		"gorelay_deploy_and_run":  true,
		"gorelay_run_connector":   true,
		"gorelay_get_job_data":    true,
		"gorelay_search_evidence": true,
	}
	for _, tool := range listed.Tools {
		if !wantNames[tool.Name] {
			t.Fatalf("unexpected tool registered: %s", tool.Name)
		}
		if tool.InputSchema == nil || tool.OutputSchema == nil {
			t.Fatalf("missing schema on %s", tool.Name)
		}
	}

	// Missing required arguments returns structured ToolFailure with RecoverySteps.
	badRes, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "gorelay_deploy_and_run",
		Arguments: map[string]any{},
	})
	if err != nil || !badRes.IsError {
		t.Fatalf("expected validation error on missing deploy arguments, got res=%v err=%v", badRes, err)
	}
	rawBad, _ := json.Marshal(badRes.StructuredContent)
	var badReply ToolReply[ExecutionOutput]
	if err := json.Unmarshal(rawBad, &badReply); err != nil || badReply.Error == nil || len(badReply.Error.RecoverySteps) == 0 {
		t.Fatalf("missing structured recovery error: %s", rawBad)
	}

	// Valid deploy_and_run with JSON object output_schema passes MCP schema validation.
	okRes, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "gorelay_deploy_and_run",
		Arguments: map[string]any{
			"source_id":     "github_go_releases",
			"executor":      connector.ExecutorHTTPAPI,
			"spec_or_code":  `{"url_template":"https://api.github.com/repos/golang/go/releases"}`,
			"output_schema": map[string]any{"entity_keys": []string{"id"}, "fields": []map[string]any{{"name": "id", "type": "string", "required": true}}},
		},
	})
	if err != nil || okRes.IsError {
		t.Fatalf("valid deploy_and_run rejected by MCP schema: res=%v err=%v", okRes, err)
	}

	// Valid deploy_and_run with plain JSON object api_spec (no double-escaping) and queries passes schema validation.
	apiObjRes, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "gorelay_deploy_and_run",
		Arguments: map[string]any{
			"source_id": "github_go_releases",
			"api_spec": map[string]any{
				"url_template": "https://api.github.com/repos/golang/go/releases?per_page=5",
				"items_path":   "",
				"pagination":   "page",
				"page_param":   "page",
				"max_pages":    2,
			},
			"output_schema": map[string]any{"entity_keys": []string{"id"}, "fields": []map[string]any{{"name": "id", "type": "string", "required": true}}},
			"queries": map[string]any{
				"list": map[string]any{"fields": []string{"id"}, "limit": 10},
				"urls": map[string]any{"extract_urls": map[string]any{"field": "body"}, "limit": 1},
			},
		},
	})
	if err != nil || apiObjRes.IsError {
		t.Fatalf("valid deploy_and_run with api_spec object rejected by MCP schema: res=%v err=%v", apiObjRes, err)
	}

	// Valid run_connector with queries multi-view and omitted freshness_ttl_sec passes MCP schema validation.
	runMultiRes, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "gorelay_run_connector",
		Arguments: map[string]any{
			"source_id": "github_go_releases",
			"queries": map[string]any{
				"list": map[string]any{"fields": []string{"tag_name"}, "order_by": "published_at", "limit": 10},
				"urls": map[string]any{"where": []map[string]any{{"field": "tag_name", "op": "eq", "value": "v3.7.4"}}, "extract_urls": map[string]any{"field": "body"}},
			},
		},
	})
	if err != nil || runMultiRes.IsError {
		t.Fatalf("valid run_connector with queries rejected by MCP schema: res=%v err=%v", runMultiRes, err)
	}

	// SearchEvidence propagates base URL properly.
	evRes, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "gorelay_search_evidence",
		Arguments: map[string]any{"source_ids": []string{"github_go_releases"}, "query": "Go 1.26"},
	})
	if err != nil || evRes.IsError {
		t.Fatalf("search evidence failed: res=%v err=%v", evRes, err)
	}
	rawEv, _ := json.Marshal(evRes.StructuredContent)
	if !strings.Contains(string(rawEv), server.URL+"/evidence?ref=abc123") {
		t.Fatalf("missing cited snapshot URL in response: %s", rawEv)
	}
	// Non-initialize MCP calls (tools/list + tools/call) must reuse singleton server without invoking ListSources again.
	if listCalls.Load() != callsAfterResource {
		t.Fatalf("non-initialize MCP calls unnecessarily rebuilt live catalog: before=%d after=%d", callsAfterResource, listCalls.Load())
	}
}
