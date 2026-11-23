package information

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/mcpserver"
	"gorm.io/gorm"
)

const nativeSourcePurpose = "mcp_source"
const nativeJobPrefix = "native_"

type nativeSourceJob struct {
	Acquisition AcquisitionRequest `json:"acquisition"`
	RefreshID   string             `json:"refresh_id,omitempty"`
	WorkspaceID string             `json:"workspace_id"`
	AssetID     string             `json:"asset_id"`
	SourceID    string             `json:"source_id"`
	Version     string             `json:"version"`
	URL         string             `json:"url"`
	Params      map[string]any     `json:"params"`
}

func (b sourceBackend) collectNative(ctx context.Context, workspace agentdata.Run, in mcpserver.CollectInput) (mcpserver.SourceJob, error) {
	out := mcpserver.SourceJob{WorkspaceID: workspace.ID}
	var asset Asset
	err := b.service.DB.WithContext(ctx).Where("source_id = ? AND active = ? AND kind IN ?", in.SourceID, true, []string{"api", "browser"}).Order("verified_at DESC").First(&asset).Error
	if err != nil {
		return out, fmt.Errorf("source_id %q has no published active acquisition capability: %w", in.SourceID, err)
	}
	if in.Version != "" && in.Version != asset.Version {
		return out, fmt.Errorf("source version changed; use active version %s", asset.Version)
	}
	var spec aiapp.TaskSpec
	if err := json.Unmarshal([]byte(asset.SpecJSON), &spec); err != nil {
		return out, err
	}
	spec.Goal = workspace.Goal
	var domains []string
	if err := json.Unmarshal([]byte(asset.ScopeJSON), &domains); err != nil {
		return out, err
	}

	var workspaceInput aiapp.CreateRunRequest
	if err := json.Unmarshal([]byte(workspace.InputJSON), &workspaceInput); err != nil {
		return out, err
	}
	if len(workspaceInput.SourceScope) > 0 {
		domains = workspaceInput.SourceScope
	}
	params := in.Params
	if params == nil {
		params = map[string]any{}
	}
	selected := assetInvocation{URL: in.URL, Params: params}
	requestInput := Input{Goal: workspace.Goal, RecordsOnly: true, SourceScope: domains, EntryURLs: []string{in.URL}}
	if _, err := nativeAssetForExecution(requestInput, asset, selected); err != nil {
		return out, err
	}
	job := nativeSourceJob{Acquisition: AcquisitionRequest{Goal: workspace.Goal, Fields: spec.RequiredFields, JoinKeys: spec.JoinKeys}, WorkspaceID: workspace.ID, AssetID: asset.ID, SourceID: asset.SourceID, Version: asset.Version, URL: in.URL, Params: params}
	job.RefreshID = in.RefreshID
	requestInput.RequestKey = "mcp-native:" + agentdata.Hash(job)
	var prior Request
	lookup := b.service.DB.WithContext(ctx).Where("request_key = ?", requestInput.RequestKey).First(&prior).Error
	if lookup == nil {
		out, err = b.nativeJob(ctx, prior.ID)
		if err != nil {
			return out, err
		}
		return out, &mcpserver.ToolFailure{Code: "ACTION_ALREADY_CALLED", Message: "the same source, version, URL and parameters were already submitted in this workspace", RecoverySteps: []string{"Read the returned job_id with gorelay_get_job or gorelay_read_data; no second native execution was started."}, Details: map[string]any{"job_id": out.JobID}}
	}
	if !errors.Is(lookup, gorm.ErrRecordNotFound) {
		return out, lookup
	}
	var request Request
	err = b.service.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		scoped := *b.service
		scoped.DB = tx
		var err error
		request, err = scoped.Create(ctx, requestInput)
		if err != nil {
			return err
		}
		if request.Purpose == nativeSourcePurpose {
			return nil
		}
		return tx.Model(&Request{}).Where("id = ?", request.ID).Updates(map[string]any{"purpose": nativeSourcePurpose, "status": "queued", "stage": "queued", "native_job_json": string(mustMarshal(job))}).Error
	})
	if err != nil {
		return out, err
	}
	return b.nativeJob(ctx, request.ID)
}

// Existing information-worker owns this persisted native work. It selects a
// deterministic execution branch and never constructs an Eino/model runner.
func (s *Service) advanceNativeSourceJob(parent context.Context, request Request) (cause error) {
	if terminal(request.Status) {
		return nil
	}
	var job nativeSourceJob
	if err := json.Unmarshal([]byte(request.NativeJobJSON), &job); err != nil {
		return err
	}
	now := time.Now()
	token := agentdata.Token()
	until := now.Add(2 * time.Minute)
	claim := s.DB.WithContext(parent).Model(&Request{}).Where("id = ? AND purpose = ? AND status NOT IN ? AND (lease_until IS NULL OR lease_until <= ?)", request.ID, nativeSourcePurpose, terminalStages(), now).Updates(map[string]any{"lease_token": token, "lease_until": until, "status": "running", "stage": "execute_asset"})
	if claim.Error != nil {
		return claim.Error
	}
	if claim.RowsAffected != 1 {
		return agentdata.ErrConflict
	}
	request.LeaseToken = token
	request.LeaseUntil = &until
	request.Status = "running"
	request.Stage = "execute_asset"
	ctx, cancel := request.RequestContext(parent)
	ctx = context.WithValue(ctx, requestExecutionOwnerKey{}, requestExecutionOwner{request.ID, token})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				q := s.DB.WithContext(ctx).Model(&Request{}).Where("id = ? AND lease_token = ? AND status = ?", request.ID, token, "running").Update("lease_until", time.Now().Add(2*time.Minute))
				if q.Error != nil || q.RowsAffected != 1 {
					cancel()
					return
				}
			}
		}
	}()
	defer func() { cancel(); <-done }()
	defer func() {
		_ = s.DB.WithContext(context.WithoutCancel(parent)).Model(&Request{}).Where("id = ? AND lease_token = ?", request.ID, token).Updates(map[string]any{"lease_token": "", "lease_until": nil}).Error
	}()
	// A crash can occur after native rows/receipt commit but before the job's
	// final state. Restore that receipt; never repeat the HTTP/browser action.
	var saved []Step
	if err := s.DB.WithContext(ctx).Where("request_id = ? AND kind IN ?", request.ID, []string{"execute_asset", "execute_browser"}).Order("id").Find(&saved).Error; err != nil {
		return err
	}
	for _, step := range saved {
		step = executionResult(step)
		if step.Stage == "succeeded" || step.Stage == "failed" {
			var receipt struct {
				Records []aiapp.Record `json:"records"`
				Failure *ActionError   `json:"failure"`
			}
			if err := json.Unmarshal([]byte(step.ResultJSON), &receipt); err != nil {
				return err
			}
			var cause error
			if step.Stage == "failed" {
				if receipt.Failure != nil {
					cause = receipt.Failure
				} else {
					cause = fmt.Errorf("%s", step.Error)
				}
			}
			return s.finishNativeSourceJob(parent, request, receipt.Records, cause)
		}
	}
	var asset Asset
	if err := s.DB.WithContext(ctx).First(&asset, "id = ? AND active = ?", job.AssetID, true).Error; err != nil {
		return s.finishNativeSourceJob(parent, request, nil, err)
	}
	if asset.Version != job.Version {
		return s.finishNativeSourceJob(parent, request, nil, fmt.Errorf("source version changed after admission; original version %s is no longer active", job.Version))
	}
	rows, cause := s.ExecuteAsset(ctx, request.ID, job.AssetID, assetInvocation{URL: job.URL, Params: job.Params})
	return s.finishNativeSourceJob(parent, request, rows, cause)
}

func (s *Service) finishNativeSourceJob(ctx context.Context, r Request, rows []aiapp.Record, cause error) error {
	stage, message := "completed", ""
	if cause != nil {
		stage, message = "failed", cause.Error()
	}
	result := sourceJobResult{Records: rows}
	updates := map[string]any{"status": stage, "stage": stage, "last_error": message, "result_json": string(mustMarshal(result))}
	q := s.DB.WithContext(context.WithoutCancel(ctx)).Model(&Request{}).Where("id = ? AND lease_token = ? AND status = ?", r.ID, r.LeaseToken, "running").Updates(updates)
	if q.Error != nil {
		return errors.Join(cause, q.Error)
	}
	if q.RowsAffected != 1 {
		return errors.Join(cause, agentdata.ErrConflict)
	}
	return cause
}

func (b sourceBackend) nativeJob(ctx context.Context, id string) (mcpserver.SourceJob, error) {
	out := mcpserver.SourceJob{JobID: nativeJobPrefix + id}
	request, err := b.service.Request(ctx, id)
	if err != nil {
		return out, err
	}
	if request.Purpose != nativeSourcePurpose {
		return out, fmt.Errorf("job_id is not a native source job")
	}
	var job nativeSourceJob
	if err := json.Unmarshal([]byte(request.NativeJobJSON), &job); err != nil {
		return out, err
	}
	out.WorkspaceID, out.SourceID, out.Version = job.WorkspaceID, job.SourceID, job.Version
	var input Input
	if err := json.Unmarshal([]byte(request.InputJSON), &input); err != nil {
		return out, err
	}
	out.URL, out.Params, out.Goal, out.RefreshID = job.URL, job.Params, input.Goal, job.RefreshID
	out.JoinKeys = job.Acquisition.JoinKeys
	out.CreatedAt, out.UpdatedAt = request.CreatedAt, request.UpdatedAt
	out.Terminal, out.Error = terminal(request.Status), request.LastError
	out.Status = request.Status
	if request.Status == "completed" {
		out.Status = "succeeded"
	}
	if request.ResultJSON != "" {
		var result sourceJobResult
		if err := json.Unmarshal([]byte(request.ResultJSON), &result); err != nil {
			return out, err
		}
		out.RecordsSaved = len(result.Records)
	}
	var steps []Step
	if err := b.service.DB.WithContext(ctx).Where("request_id = ?", request.ID).Order("id").Find(&steps).Error; err != nil {
		return out, err
	}
	for _, step := range steps {
		if step.ResultJSON != "" {
			var receipt map[string]any
			if err := json.Unmarshal([]byte(step.ResultJSON), &receipt); err != nil {
				return out, err
			}
			if observation := receipt["observation"]; observation != nil {
				out.SourceChecks = append(out.SourceChecks, observation)
			}
		}
		out.Activities = append(out.Activities, mcpserver.SourceActivity{Tool: step.Kind, Status: step.Stage, Error: step.Error})
	}
	if request.Status == "failed" {
		out.Failure = sourceJobFailure(out.Error, out.WorkspaceID, out.JobID, out.SourceID, out.Version)
		for _, step := range steps {
			if step.Stage != "failed" {
				continue
			}
			var receipt struct {
				Failure        *ActionError `json:"failure"`
				InternalStatus any          `json:"failure_status"`
			}
			if err := json.Unmarshal([]byte(step.ResultJSON), &receipt); err != nil {
				return out, err
			}
			if receipt.Failure != nil {
				out.Failure.Code = receipt.Failure.Code
				out.Failure.Details["diagnostic"] = receipt.Failure.Details
				out.Failure.Details["internal_status"] = receipt.InternalStatus
			}
		}
	}
	out.ReadData = map[string]any{"tool": "gorelay_read_data", "arguments": map[string]any{"job_id": out.JobID}}
	if !out.Terminal {
		out.Notices = append(out.Notices, "This native source job is dispatched by information-worker; reading its status does not execute it.")
	}
	return out, nil
}
