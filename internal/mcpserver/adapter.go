package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"gorelay/internal/connector"
	"gorelay/internal/fetchgov"
	"gorelay/internal/pipeline"
	"gorelay/internal/probe"
)

func toPipelineQuery(in *RecordQueryInput) (*pipeline.RecordQuery, error) {
	if in == nil {
		return nil, nil
	}
	q := &pipeline.RecordQuery{
		Fields:        in.Fields,
		ExcludeFields: in.ExcludeFields,
		MaxFieldChars: in.MaxFieldChars,
		OrderBy:       in.OrderBy,
		Limit:         in.Limit,
		GroupBy:       in.GroupBy,
		GroupPrefix:   in.GroupPrefix,
	}
	if len(in.Where) > 0 {
		q.Where = make([]pipeline.FilterCondition, 0, len(in.Where))
		for _, c := range in.Where {
			q.Where = append(q.Where, pipeline.FilterCondition{
				Field: c.Field,
				Op:    c.Op,
				Value: c.Value,
			})
		}
	}
	if in.ExtractURLs != nil {
		q.ExtractURLs = &pipeline.URLExtractSpec{
			Field:   in.ExtractURLs.Field,
			Pattern: in.ExtractURLs.Pattern,
			Regex:   in.ExtractURLs.Regex,
		}
	}
	if err := q.Validate(); err != nil {
		return nil, err
	}
	if q.IsEmpty() {
		return nil, nil
	}
	return q, nil
}

func toPipelineQueries(in map[string]map[string]any) (map[string]*pipeline.RecordQuery, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if len(in) > pipeline.MaxMCPMultiQueries {
		return nil, fmt.Errorf("queries supports at most %d named views per call", pipeline.MaxMCPMultiQueries)
	}
	out := make(map[string]*pipeline.RecordQuery, len(in))
	for name, spec := range in {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" {
			return nil, fmt.Errorf("queries view name must not be empty")
		}
		q, err := pipeline.ParseRecordQuery(spec)
		if err != nil {
			return nil, fmt.Errorf("queries[%q]: %w", trimmed, err)
		}
		if q == nil {
			q = &pipeline.RecordQuery{}
		}
		out[trimmed] = q
	}
	return out, nil
}

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
			details := map[string]any{"host": ge.Host}
			if ge.StatusCode > 0 {
				details["status_code"] = ge.StatusCode
			}
			if ge.RetryAfterSec > 0 {
				details["retry_after_sec"] = ge.RetryAfterSec
			}
			steps := []string{"Check target access policy or retry later."}
			switch ge.Code {
			case fetchgov.ErrCodeRobotsDisallowed:
				steps = []string{"Respect robots.txt; do not scrape disallowed paths."}
			case fetchgov.ErrCodeSSRFBlocked:
				steps = []string{"Only public HTTP(S) URLs are allowed; private/loopback IPs are blocked."}
			}
			return res, &ToolFailure{
				Code:          ge.Code,
				Message:       ge.Error(),
				RecoverySteps: steps,
				Details:       details,
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
	if in.OutputSchema != nil {
		raw, err := json.Marshal(in.OutputSchema)
		if err != nil {
			return ExecutionOutput{}, &ToolFailure{Code: "INVALID_ARGUMENTS", Message: "invalid output_schema: " + err.Error(), RecoverySteps: defaultRecovery["gorelay_deploy_and_run"]}
		}
		outSchemaRaw = raw
	}
	executor := strings.TrimSpace(in.Executor)
	specOrCode := in.SpecOrCode
	targetURL := strings.TrimSpace(in.TargetURL)
	if in.APISpec != nil {
		raw, err := json.Marshal(in.APISpec)
		if err != nil {
			return ExecutionOutput{}, &ToolFailure{Code: "INVALID_ARGUMENTS", Message: "invalid api_spec: " + err.Error(), RecoverySteps: defaultRecovery["gorelay_deploy_and_run"]}
		}
		specOrCode = string(raw)
		if executor == "" {
			executor = connector.ExecutorHTTPAPI
		}
		if targetURL == "" {
			targetURL = strings.TrimSpace(in.APISpec.URLTemplate)
		}
	} else if executor == "" && in.RollbackTo == 0 && strings.TrimSpace(specOrCode) != "" {
		if strings.HasPrefix(strings.TrimSpace(specOrCode), "{") {
			executor = connector.ExecutorHTTPAPI
		} else {
			executor = connector.ExecutorLuaSandbox
		}
	}

	query, err := toPipelineQuery(in.Query)
	if err != nil {
		return ExecutionOutput{}, &ToolFailure{Code: "INVALID_ARGUMENTS", Message: err.Error(), RecoverySteps: defaultRecovery["gorelay_deploy_and_run"]}
	}
	queries, err := toPipelineQueries(in.Queries)
	if err != nil {
		return ExecutionOutput{}, &ToolFailure{Code: "INVALID_ARGUMENTS", Message: err.Error(), RecoverySteps: defaultRecovery["gorelay_deploy_and_run"]}
	}
	waitMS := in.WaitMS
	if waitMS <= 0 {
		waitMS = 5000
	}
	res, err := b.Service.DeployAndRun(ctx, connector.DeployAndRunInput{
		SourceID:     in.SourceID,
		Kind:         in.Kind,
		Executor:     executor,
		Description:  in.Description,
		TargetURL:    targetURL,
		SpecOrCode:   specOrCode,
		InputSchema:  inSchemaRaw,
		OutputSchema: outSchemaRaw,
		Params:       in.Params,
		AuthProfile:  in.AuthProfile,
		WaitMS:       waitMS,
		RollbackTo:   in.RollbackTo,
		Query:        query,
		Queries:      queries,
	}, RequestBaseURL(ctx))
	if err != nil {
		details := map[string]any{"active_version": res.ActiveVersion, "intact_cached_rows": res.IntactCachedRows}
		if res.Drift != nil {
			details["drift"] = res.Drift
		}
		return res, &ToolFailure{
			Code:          "DEPLOY_FAILED",
			Message:       err.Error(),
			RecoverySteps: defaultRecovery["gorelay_deploy_and_run"],
			Details:       details,
		}
	}
	return res, nil
}

func (b ConnectorBackend) RunConnector(ctx context.Context, in RunConnectorInput) (ExecutionOutput, error) {
	query, err := toPipelineQuery(in.Query)
	if err != nil {
		return ExecutionOutput{}, &ToolFailure{Code: "INVALID_ARGUMENTS", Message: err.Error(), RecoverySteps: defaultRecovery["gorelay_run_connector"]}
	}
	queries, err := toPipelineQueries(in.Queries)
	if err != nil {
		return ExecutionOutput{}, &ToolFailure{Code: "INVALID_ARGUMENTS", Message: err.Error(), RecoverySteps: defaultRecovery["gorelay_run_connector"]}
	}
	waitMS := in.WaitMS
	if waitMS <= 0 {
		waitMS = 5000
	}
	freshnessTTL := in.FreshnessTTLSec
	if freshnessTTL == 0 && !in.ForceRefresh && strings.TrimSpace(in.TargetURL) == "" {
		freshnessTTL = 3600
	}
	res, err := b.Service.RunConnector(ctx, connector.RunConnectorInput{
		SourceID:        in.SourceID,
		TargetURL:       in.TargetURL,
		Params:          in.Params,
		FreshnessTTLSec: freshnessTTL,
		WaitMS:          waitMS,
		ForceRefresh:    in.ForceRefresh,
		Query:           query,
		Queries:         queries,
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
			Details:       map[string]any{"drift": res.Drift, "active_version": res.ActiveVersion, "intact_cached_rows": res.IntactCachedRows},
		}
	}
	return res, nil
}

func (b ConnectorBackend) GetJobData(ctx context.Context, in JobDataInput) (ExecutionOutput, error) {
	query, err := toPipelineQuery(in.Query)
	if err != nil {
		return ExecutionOutput{}, &ToolFailure{Code: "INVALID_ARGUMENTS", Message: err.Error(), RecoverySteps: defaultRecovery["gorelay_get_job_data"]}
	}
	queries, err := toPipelineQueries(in.Queries)
	if err != nil {
		return ExecutionOutput{}, &ToolFailure{Code: "INVALID_ARGUMENTS", Message: err.Error(), RecoverySteps: defaultRecovery["gorelay_get_job_data"]}
	}
	if in.JobID == 0 {
		if in.SourceID != "" && !in.Cancel {
			var page pipeline.DeliveredPage
			switch {
			case len(queries) > 0:
				page, err = b.Service.Pipeline.DeliverRecordsMultiQuery(ctx, in.SourceID, 0, b.Service.Pipeline.QuerySafetyMaxBytes(), pipeline.DefaultMCPPageSize, RequestBaseURL(ctx), queries)
			case query != nil:
				page, err = b.Service.Pipeline.DeliverRecordsPageWithQuery(ctx, in.SourceID, 0, in.Cursor, b.Service.Pipeline.QuerySafetyMaxBytes(), pipeline.DefaultMCPPageSize, RequestBaseURL(ctx), query)
			default:
				page, err = b.Service.Pipeline.DeliverReceipt(ctx, in.SourceID, 0, RequestBaseURL(ctx))
			}
			if err != nil {
				return ExecutionOutput{}, err
			}
			page.SourceID = ""
			page.JobID = 0
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
			Message:       "positive job_id (or source_id for cached query/receipt) is required",
			RecoverySteps: defaultRecovery["gorelay_get_job_data"],
		}
	}
	if in.Cancel {
		return b.Service.CancelJob(ctx, in.JobID)
	}
	return b.Service.BuildJobResponseWithDelivery(ctx, in.JobID, in.Cursor, RequestBaseURL(ctx), query, queries, false)
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
