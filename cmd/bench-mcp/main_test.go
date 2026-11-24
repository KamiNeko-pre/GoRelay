package main

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"gorelay/internal/connector"
	"gorelay/internal/mcpserver"
	"gorelay/internal/pipeline"
	"gorelay/internal/probe"
)

func TestBenchSimulationInvariants(t *testing.T) {
	rep, err := runSimulation(context.Background())
	if err != nil {
		t.Fatalf("runSimulation failed: %v", err)
	}
	if !rep.HTTPAPIPromoted || !rep.CheckpointResumedPage2 || !rep.DriftBlockedOverwrite {
		t.Fatalf("simulation invariants failed: %+v", rep)
	}
	if rep.PayloadReductionPct <= 90 {
		t.Fatalf("expected >90%% payload projection reduction, got %.2f%%", rep.PayloadReductionPct)
	}
}

type liveBenchMockBackend struct {
	deployed bool
}

func (m *liveBenchMockBackend) ListSources(context.Context, mcpserver.ListSourcesInput) (mcpserver.ListSourcesOutput, error) {
	return mcpserver.ListSourcesOutput{}, nil
}
func (m *liveBenchMockBackend) ProbeSource(_ context.Context, req mcpserver.ProbeSourceInput) (mcpserver.ProbeSourceOutput, error) {
	return probe.ProbeResult{
		URL:                 req.URL,
		FinalURL:            req.URL,
		Status:              200,
		Kind:                "json_api",
		RecommendedExecutor: connector.ExecutorHTTPAPI,
		DraftAPISpec: &probe.DraftAPISpec{
			URLTemplate:  req.URL,
			Method:       "GET",
			ItemsPath:    "/items",
			FieldMapping: map[string]string{"id": "/id", "name": "/name"},
			Pagination:   "none",
		},
	}, nil
}
func (m *liveBenchMockBackend) DeployAndRun(_ context.Context, in mcpserver.DeployAndRunInput) (mcpserver.ExecutionOutput, error) {
	m.deployed = true
	return connector.ExecutionResponse{
		JobID:        1,
		SourceID:     in.SourceID,
		Version:      1,
		Executor:     in.Executor,
		Status:       "succeeded",
		HitType:      connector.HitColdBuild,
		Terminal:     true,
		RecordsSaved: 1,
		Data: &pipeline.DeliveredPage{
			Returned: 1,
			Records:  []map[string]any{{"id": "1", "name": "GoRelay"}},
		},
	}, nil
}
func (m *liveBenchMockBackend) RunConnector(_ context.Context, in mcpserver.RunConnectorInput) (mcpserver.ExecutionOutput, error) {
	if !in.ForceRefresh && in.FreshnessTTLSec > 0 {
		return connector.ExecutionResponse{
			SourceID: in.SourceID,
			Version:  1,
			Executor: connector.ExecutorHTTPAPI,
			Status:   connector.HitCacheHit,
			HitType:  connector.HitCacheHit,
			Terminal: true,
			Data: &pipeline.DeliveredPage{
				Returned: 1,
				Records:  []map[string]any{{"id": "1", "name": "GoRelay"}},
			},
		}, nil
	}
	return connector.ExecutionResponse{
		JobID:        2,
		SourceID:     in.SourceID,
		Version:      1,
		Executor:     connector.ExecutorHTTPAPI,
		Status:       "succeeded",
		HitType:      connector.HitAPIReuse,
		Terminal:     true,
		RecordsSaved: 1,
	}, nil
}
func (m *liveBenchMockBackend) GetJobData(context.Context, mcpserver.JobDataInput) (mcpserver.ExecutionOutput, error) {
	return connector.ExecutionResponse{}, nil
}
func (m *liveBenchMockBackend) SearchEvidence(context.Context, mcpserver.SearchEvidenceInput) (mcpserver.SearchEvidenceOutput, error) {
	return pipeline.EvidencePage{}, nil
}

func TestRunLiveCheckRequiresTargetURLAndMeasuresRealMCPFlow(t *testing.T) {
	if _, err := runLiveCheck(context.Background(), "http://127.0.0.1:1/mcp", "", "", ""); err == nil || !strings.Contains(err.Error(), "-target-url is required") {
		t.Fatalf("expected explicit -target-url requirement error, got %v", err)
	}

	mock := &liveBenchMockBackend{}
	srv := httptest.NewServer(mcpserver.NewHandler(mock, "http://localhost:8080"))
	defer srv.Close()

	rep, err := runLiveCheck(context.Background(), srv.URL, "", "https://api.example.org/items", "bench_items")
	if err != nil {
		t.Fatalf("runLiveCheck failed: %v", err)
	}
	if !mock.deployed || !rep.HTTPAPIPromoted || !rep.CacheHitVerified || rep.GoRelayProjectedBytes == 0 {
		t.Fatalf("unexpected live bench report: %+v", rep)
	}
}
