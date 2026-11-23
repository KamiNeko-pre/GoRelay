package information

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/mcpserver"
	"go-lua-crawler/internal/repository"
	task "go-lua-crawler/internal/task"
	"gorm.io/gorm"
)

func (b sourceBackend) Collect(ctx context.Context, in mcpserver.CollectInput) (mcpserver.SourceJob, error) {
	out := mcpserver.SourceJob{}
	if err := b.checkRefresh(ctx, &in); err != nil {
		return out, err
	}
	crawler, err := b.crawler()
	if err != nil {
		return out, err
	}
	var workspace agentdata.Run
	if in.WorkspaceID == "" {
		workspace, err = crawler.OpenSourceWorkspace(ctx, in.Goal, nil)
	} else {
		workspace, err = crawler.SourceWorkspace(ctx, in.WorkspaceID)
	}
	if err != nil {
		return out, err
	}
	head, err := crawler.Data.SourceHead(ctx, in.SourceID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return b.collectNative(ctx, workspace, in)
	}
	if err != nil {
		return out, fmt.Errorf("published source %q: %w", in.SourceID, err)
	}
	if head.Status != "active" {
		return out, fmt.Errorf("source %q is %s; inspect the catalog for an active version", in.SourceID, head.Status)
	}
	if in.Version != "" && in.Version != head.ArtifactHash {
		return out, fmt.Errorf("source version changed: requested %s, active %s; use the current catalog version", in.Version, head.ArtifactHash)
	}
	artifact, err := crawler.Data.Artifact(ctx, head.ArtifactHash)
	if err != nil {
		return out, err
	}
	params := in.Params
	if params == nil {
		params = map[string]any{}
	}
	if err := aiapp.ValidateParameters(artifact.InputSchema, params); err != nil {
		return out, err
	}
	var spec aiapp.TaskSpec
	if err := json.Unmarshal([]byte(artifact.OutputSchema), &spec); err != nil {
		return out, err
	}
	spec.Goal = workspace.Goal
	var domains []string
	if err := json.Unmarshal([]byte(artifact.ScopeJSON), &domains); err != nil {
		return out, err
	}

	var workspaceInput aiapp.CreateRunRequest
	if err := json.Unmarshal([]byte(workspace.InputJSON), &workspaceInput); err != nil {
		return out, err
	}
	if len(workspaceInput.SourceScope) > 0 {
		domains = workspaceInput.SourceScope
	}
	key := "mcp-collect:" + agentdata.Hash([]any{workspace.ID, in.SourceID, head.ArtifactHash, in.URL, params})
	if in.RefreshID != "" {
		key = "mcp-collect:" + agentdata.Hash([]any{workspace.ID, in.SourceID, head.ArtifactHash, in.URL, params, in.RefreshID})
	}
	var prior agentdata.Run
	lookup := crawler.Data.DB.WithContext(ctx).Where("request_key = ?", key).First(&prior).Error
	if lookup == nil {
		out, err = b.GetJob(ctx, mcpserver.JobInput{JobID: prior.ID})
		if err != nil {
			return out, err
		}
		return out, &mcpserver.ToolFailure{Code: "ACTION_ALREADY_CALLED", Message: "the same source, version, URL and parameters were already submitted in this workspace", RecoverySteps: []string{"Use gorelay_get_job with the returned job_id to inspect progress.", "Use gorelay_read_data with that job_id to read saved results. Change real acquisition parameters only when the task needs different data."}, Details: map[string]any{"job_id": prior.ID, "workspace_id": workspace.ID}}
	}
	if !errors.Is(lookup, gorm.ErrRecordNotFound) {
		return out, lookup
	}
	request := aiapp.CreateRunRequest{SourceWorkspaceID: workspace.ID, RequestKey: key, Goal: workspace.Goal, EntryURLs: []string{in.URL}, SourceScope: domains, Spec: &spec}
	job, cause := crawler.SubmitSourceCollection(ctx, request, in.SourceID, head.ArtifactHash, in.URL, params)
	if job.ID != "" {
		out, err = b.GetJob(ctx, mcpserver.JobInput{JobID: job.ID})
		if err != nil {
			return out, errors.Join(cause, err)
		}
	}
	if cause != nil {
		return out, &mcpserver.ToolFailure{Code: "COLLECTION_SUBMISSION_FAILED", Message: cause.Error(), RecoverySteps: []string{"Submission was not committed, so no new job_id is available. Correct the exact source, version, URL or parameter diagnosis before resubmitting.", "For a saved job, use gorelay_get_job and gorelay_read_data instead of creating identical work."}, Details: map[string]any{"job_id": job.ID, "workspace_id": workspace.ID}}
	}
	return out, nil
}

// GetJob derives status from durable physical tasks. Reading does not run an
// Agent, advance a workflow, revalidate delivery, or submit more collection.
func (b sourceBackend) GetJob(ctx context.Context, in mcpserver.JobInput) (mcpserver.SourceJob, error) {
	if strings.HasPrefix(in.JobID, savedJobPrefix) {
		return b.savedSourceJob(ctx, in.JobID)
	}
	if strings.HasPrefix(in.JobID, nativeJobPrefix) {
		if _, err := b.store(); err != nil {
			return mcpserver.SourceJob{}, err
		}
		return b.nativeJob(ctx, strings.TrimPrefix(in.JobID, nativeJobPrefix))
	}
	out := mcpserver.SourceJob{JobID: in.JobID}
	crawler, err := b.crawler()
	if err != nil {
		return out, err
	}
	run, err := crawler.Data.Run(ctx, in.JobID)
	if err != nil {
		return out, fmt.Errorf("job_id %q: %w", in.JobID, err)
	}
	out.Status, out.CreatedAt, out.UpdatedAt = run.Status, run.CreatedAt, run.UpdatedAt
	out.Goal = run.Goal
	var request aiapp.CreateRunRequest
	if err := json.Unmarshal([]byte(run.InputJSON), &request); err != nil {
		return out, err
	}
	out.WorkspaceID = request.SourceWorkspaceID
	if request.Spec != nil {
		out.JoinKeys = request.Spec.JoinKeys
	}
	if strings.HasPrefix(run.RequestKey, "mcp-source:") {
		out.WorkspaceID = run.ID
		out.Terminal = run.IsTerminal()
		operations, err := crawler.Data.Operations(ctx, run.ID)
		if err != nil {
			return out, err
		}
		for _, operation := range operations {
			var receipt aiapp.ToolResult
			if operation.ResultJSON != "" {
				if err := json.Unmarshal([]byte(operation.ResultJSON), &receipt); err != nil {
					return out, err
				}
			}
			out.Activities = append(out.Activities, mcpserver.SourceActivity{Handle: receipt.CallID, Tool: sourceRecoveryNames.Replace(operation.Kind), Status: operation.Stage, Error: receipt.Error})
		}
		return out, nil
	}
	if !strings.HasPrefix(run.RequestKey, "mcp-collect:") {
		return out, fmt.Errorf("job_id must identify a job returned by gorelay_collect")
	}
	links, err := crawler.Data.Children(ctx, run.ID)
	if err != nil {
		return out, err
	}
	if len(links) == 0 {
		out.Status = "queued"
		if run.LastError != "" {
			out.Status, out.Terminal, out.Error = "failed", true, run.LastError
		}
		if run.Cancelled || run.Status == "cancelled" {
			out.Status, out.Terminal = "cancelled", true
		}
		return out, nil
	}
	succeeded, failed, active := 0, 0, 0
	for _, link := range links {
		var child task.CrawlTask
		if err := b.service.DB.WithContext(ctx).First(&child, "id = ?", link.TaskID).Error; err != nil {
			return out, err
		}
		item := mcpserver.SourceTask{TaskID: child.ID, Status: string(child.Status), FailureCode: child.FailureCode, Error: child.LastError}
		if child.UpdatedAt.After(out.UpdatedAt) {
			out.UpdatedAt = child.UpdatedAt
		}
		out.SourceID, out.Version = link.SourceID, child.ProgramHash
		out.URL = child.URL
		if child.ProgramParams != "" {
			var input struct {
				Params map[string]any `json:"params"`
			}
			if err := json.Unmarshal([]byte(child.ProgramParams), &input); err != nil {
				return out, fmt.Errorf("read saved collection parameters: %w", err)
			}
			out.Params = input.Params
		}
		switch child.Status {
		case task.StatusSucceeded:
			var saved repository.CrawlResult
			if err := b.service.DB.WithContext(ctx).First(&saved, "task_id = ?", child.ID).Error; err != nil {
				return out, fmt.Errorf("successful task %d has no readable saved result: %w", child.ID, err)
			}
			var result aiapp.AcquisitionResult
			if err := json.Unmarshal([]byte(saved.Payload), &result); err != nil {
				return out, err
			}
			item.RecordsSaved = len(result.Records)
			out.RecordsSaved += item.RecordsSaved
			succeeded++
		case task.StatusFailed:
			failed++
			out.Error = child.LastError
		default:
			active++
		}
		out.Tasks = append(out.Tasks, item)
	}
	out.Status = "queued"
	for _, child := range out.Tasks {
		if child.Status == "running" {
			out.Status = "running"
		}
	}
	if active == 0 {
		out.Terminal = true
		switch {
		case failed == 0:
			out.Status = "succeeded"
		case succeeded > 0:
			out.Status = "partial"
		default:
			out.Status = "failed"
		}
	}
	if run.Cancelled || run.Status == "cancelled" {
		out.Status, out.Terminal = "cancelled", true
		if active > 0 {
			out.Notices = append(out.Notices, "Cancellation is recorded. In-flight physical work may still be stopping; the cancelled run cannot accept a new result commit.")
		}
	}
	if failed > 0 {
		out.Failure = sourceJobFailure(out.Error, out.WorkspaceID, out.JobID, out.SourceID, out.Version)
	}
	out.ReadData = map[string]any{"tool": "gorelay_read_data", "arguments": map[string]any{"job_id": run.ID}}
	return out, nil
}

func (b sourceBackend) CancelJob(ctx context.Context, in mcpserver.JobInput) (mcpserver.SourceJob, error) {
	if strings.HasPrefix(in.JobID, nativeJobPrefix) {
		current, err := b.GetJob(ctx, in)
		if err != nil || current.Terminal {
			return current, err
		}
		if err := b.service.Cancel(ctx, strings.TrimPrefix(in.JobID, nativeJobPrefix)); err != nil {
			return current, err
		}
		return b.GetJob(ctx, in)
	}
	current, err := b.GetJob(ctx, in)
	if err != nil || current.Terminal {
		return current, err
	}
	crawler, err := b.crawler()
	if err != nil {
		return current, err
	}
	if err := crawler.Data.Cancel(ctx, in.JobID); err != nil {
		return current, err
	}
	return b.GetJob(ctx, in)
}

func sourceJobFailure(message, workspace, job, source, version string) *mcpserver.ToolFailure {
	return &mcpserver.ToolFailure{Code: "SOURCE_EXECUTION_FAILED", Message: message, RecoverySteps: []string{
		"Preserve saved rows with gorelay_read_data(job_id); source or execution failure does not erase earlier results.",
		"Read the actual source program with gorelay_inspect_source(source_id) and inspect its target URL or saved diagnostics. Use the returned runtime API and output fields to identify the exact input, locator or Lua error.",
		"For a source repair, change the diagnosed program or extraction and validate it with gorelay_validate_program in the returned workspace, then publish and collect the new version. Do not resubmit identical failed work or fabricate unavailable facts.",
	}, Details: map[string]any{"workspace_id": workspace, "job_id": job, "source_id": source, "version": version}}
}
