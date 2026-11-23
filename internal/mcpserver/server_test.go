package mcpserver

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"gorelay/internal/connector"
	"gorelay/internal/pipeline"
	"gorelay/internal/probe"
)

type stubBackend struct{}

func (stubBackend) ListSources(context.Context, ListSourcesInput) (ListSourcesOutput, error) {
	return ListSourcesOutput{
		Total: 1,
		Connectors: []connector.ConnectorView{{
			SourceID: "github_go_releases", Kind: connector.KindData, Executor: connector.ExecutorHTTPAPI, Domain: "api.github.com",
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
	server := httptest.NewServer(NewHandler(stubBackend{}, ""))
	defer server.Close()

	ctx := t.Context()
	client := mcp.NewClient(&mcp.Implementation{Name: "v2-test", Version: "2.0.0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Tools) != 6 {
		t.Fatalf("expected exactly 6 MCP tools, got %d", len(listed.Tools))
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
}
