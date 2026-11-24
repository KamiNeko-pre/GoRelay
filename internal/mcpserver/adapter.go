package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"gorelay/internal/connector"
	"gorelay/internal/fetchgov"
	"gorelay/internal/probe"
)

// ConnectorBackend bridges the 6 MCP tools cleanly to connector, probe, and pipeline.
type ConnectorBackend struct {
	Service *connector.Service
}

func (b ConnectorBackend) ListSources(ctx context.Context, in ListSourcesInput) (ListSourcesOutput, error) {
	return b.Service.ListSources(ctx, connector.ListSourcesInput{
		Domain:          in.Domain,
		URL:             in.URL,
		Keyword:         in.Keyword,
		Kind:            in.Kind,
		FreshnessTTLSec: in.FreshnessTTLSec,
		Cursor:          in.Cursor,
		PageSize:        in.PageSize,
	})
}

func (b ConnectorBackend) ProbeSource(ctx context.Context, in ProbeSourceInput) (ProbeSourceOutput, error) {
	if in.URL == "" {
		return ProbeSourceOutput{}, &ToolFailure{
			Code:          "INVALID_ARGUMENTS",
			Message:       "url is required",
			RecoverySteps: defaultRecovery["gorelay_probe_source"],
		}
	}
	res, err := b.Service.Prober.Probe(ctx, probe.ProbeRequest{
		URL:         in.URL,
		DetectAPI:   in.DetectAPI,
		UseBrowser:  in.UseBrowser,
		AuthProfile: in.AuthProfile,
	})
	if err != nil {
		var ge *fetchgov.GovernanceError
		if errors.As(err, &ge) {
			return res, &ToolFailure{
				Code:          ge.Code,
				Message:       ge.Error(),
				RecoverySteps: defaultRecovery["gorelay_probe_source"],
				Details:       map[string]any{"host": ge.Host, "status_code": ge.StatusCode, "retry_after_sec": ge.RetryAfterSec},
			}
		}
		return res, err
	}
	return res, nil
}

func (b ConnectorBackend) DeployAndRun(ctx context.Context, in DeployAndRunInput) (ExecutionOutput, error) {
	var inSchemaRaw, outSchemaRaw json.RawMessage
	if len(in.InputSchema) > 0 {
		raw, err := json.Marshal(in.InputSchema)
		if err != nil {
			return ExecutionOutput{}, &ToolFailure{Code: "INVALID_ARGUMENTS", Message: "invalid input_schema: " + err.Error(), RecoverySteps: defaultRecovery["gorelay_deploy_and_run"]}
		}
		inSchemaRaw = raw
	}
	if len(in.OutputSchema) > 0 {
		raw, err := json.Marshal(in.OutputSchema)
		if err != nil {
			return ExecutionOutput{}, &ToolFailure{Code: "INVALID_ARGUMENTS", Message: "invalid output_schema: " + err.Error(), RecoverySteps: defaultRecovery["gorelay_deploy_and_run"]}
		}
		outSchemaRaw = raw
	}
	res, err := b.Service.DeployAndRun(ctx, connector.DeployAndRunInput{
		SourceID:     in.SourceID,
		Kind:         in.Kind,
		Executor:     in.Executor,
		Description:  in.Description,
		TargetURL:    in.TargetURL,
		SpecOrCode:   in.SpecOrCode,
		InputSchema:  inSchemaRaw,
		OutputSchema: outSchemaRaw,
		Params:       in.Params,
		AuthProfile:  in.AuthProfile,
		WaitMS:       in.WaitMS,
		RollbackTo:   in.RollbackTo,
	}, RequestBaseURL(ctx))
	if err != nil {
		return res, &ToolFailure{
			Code:          "DEPLOY_FAILED",
			Message:       err.Error(),
			RecoverySteps: defaultRecovery["gorelay_deploy_and_run"],
		}
	}
	return res, nil
}

func (b ConnectorBackend) RunConnector(ctx context.Context, in RunConnectorInput) (ExecutionOutput, error) {
	res, err := b.Service.RunConnector(ctx, connector.RunConnectorInput{
		SourceID:        in.SourceID,
		TargetURL:       in.TargetURL,
		Params:          in.Params,
		FreshnessTTLSec: in.FreshnessTTLSec,
		Cursor:          in.Cursor,
		WaitMS:          in.WaitMS,
		ForceRefresh:    in.ForceRefresh,
	}, RequestBaseURL(ctx))
	if err != nil {
		code := "RUN_FAILED"
		if res.Status == connector.StatusDegraded {
			code = "SKILL_DRIFT_DETECTED"
		}
		return res, &ToolFailure{
			Code:          code,
			Message:       err.Error(),
			RecoverySteps: defaultRecovery["gorelay_run_connector"],
			Details:       map[string]any{"drift": res.Drift},
		}
	}
	return res, nil
}

func (b ConnectorBackend) GetJobData(ctx context.Context, in JobDataInput) (ExecutionOutput, error) {
	if in.JobID == 0 {
		if in.SourceID != "" && !in.Cancel {
			page, err := b.Service.Pipeline.DeliverRecordsPage(ctx, in.SourceID, 0, in.Cursor, 16*1024, 100, RequestBaseURL(ctx))
			if err != nil {
				return ExecutionOutput{}, err
			}
			return ExecutionOutput{
				SourceID: in.SourceID,
				Status:   connector.HitCacheHit,
				HitType:  connector.HitCacheHit,
				Terminal: true,
				Data:     &page,
			}, nil
		}
		return ExecutionOutput{}, &ToolFailure{
			Code:          "INVALID_ARGUMENTS",
			Message:       "positive job_id (or source_id for cached pagination) is required",
			RecoverySteps: defaultRecovery["gorelay_get_job_data"],
		}
	}
	if in.Cancel {
		return b.Service.CancelJob(ctx, in.JobID)
	}
	return b.Service.BuildJobResponse(ctx, in.JobID, in.Cursor, RequestBaseURL(ctx))
}

func (b ConnectorBackend) SearchEvidence(ctx context.Context, in SearchEvidenceInput) (SearchEvidenceOutput, error) {
	if len(in.SourceIDs) == 0 && in.SnapshotRef == "" {
		return SearchEvidenceOutput{}, &ToolFailure{
			Code:          "INVALID_ARGUMENTS",
			Message:       fmt.Sprintf("source_ids or snapshot_ref is required"),
			RecoverySteps: defaultRecovery["gorelay_search_evidence"],
		}
	}
	return b.Service.Pipeline.SearchEvidence(ctx, in.SourceIDs, in.Query, in.SnapshotRef, in.Cursor, in.PageSize, RequestBaseURL(ctx))
}
