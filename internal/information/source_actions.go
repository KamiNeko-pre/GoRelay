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
	"gorm.io/gorm"
)

func (b sourceBackend) crawler() (*aiapp.Service, error) {
	if _, err := b.store(); err != nil {
		return nil, err
	}
	if b.service.Crawler == nil {
		return nil, &mcpserver.ToolFailure{Code: "SERVICE_UNAVAILABLE", Message: "GoRelay source execution is not initialized", RecoverySteps: []string{"Connect to the normal GoRelay application with its crawler and runner configured; saved evidence remains readable without a model API."}}
	}
	return b.service.Crawler, nil
}

func (b sourceBackend) InspectSource(ctx context.Context, in mcpserver.InspectSourceInput) (mcpserver.SourceAction, error) {
	out := mcpserver.SourceAction{}
	if (in.URL == "") == (in.SourceID == "") {
		return out, fmt.Errorf("provide exactly one url or source_id")
	}
	crawler, err := b.crawler()
	if err != nil {
		return out, err
	}
	var workspace agentdata.Run
	if in.WorkspaceID == "" {
		workspace, err = crawler.OpenSourceWorkspace(ctx, in.Goal, in.SourceScope)
	} else {
		workspace, err = crawler.SourceWorkspace(ctx, in.WorkspaceID)
	}
	if err != nil {
		return out, err
	}

	if in.SourceID != "" {
		if _, lookup := crawler.Data.SourceHead(ctx, in.SourceID); errors.Is(lookup, gorm.ErrRecordNotFound) {
			var asset Asset
			err := b.service.DB.WithContext(ctx).Where("source_id = ? AND active = ? AND kind IN ?", in.SourceID, true, []string{"api", "browser"}).Order("verified_at DESC").First(&asset).Error
			if err != nil {
				return mcpserver.SourceAction{WorkspaceID: workspace.ID}, fmt.Errorf("source_id %q is not a published source: %w", in.SourceID, err)
			}
			var manifest any
			if err := json.Unmarshal([]byte(asset.ManifestJSON), &manifest); err != nil {
				return mcpserver.SourceAction{WorkspaceID: workspace.ID}, err
			}
			return mcpserver.SourceAction{WorkspaceID: workspace.ID, Observation: map[string]any{"source_id": asset.SourceID, "kind": asset.Kind, "version": asset.Version, "entry_url": asset.EntryURL, "manifest": manifest, "parameter_binding": "Collect params update observed URL query parameters; the asset keeps its saved field extraction and Browser approval."}, RuntimeGuidance: aiapp.SourceProgramGuidance()}, nil
		} else if lookup != nil {
			return mcpserver.SourceAction{WorkspaceID: workspace.ID}, lookup
		}
	}
	args := map[string]any{}
	if in.URL != "" {
		args["url"] = in.URL
	} else {
		args["source_id"] = in.SourceID
	}
	out, err = b.sourceAction(ctx, workspace.ID, "inspect_source", args)
	out.RuntimeGuidance = aiapp.SourceProgramGuidance()
	if err == nil && in.URL != "" {
		if observation, ok := out.Observation.(map[string]any); ok {
			if source, ok := observation["source"].(map[string]any); ok {
				if receipt, ok := source["receipt"].(map[string]any); ok {
					if actual, ok := receipt["url"].(string); ok && actual != "" {
						if saveErr := b.service.rememberSources(ctx, workspace.ID, workspace.Goal, []aiapp.SearchCandidate{{URL: actual}}, true); saveErr != nil {
							return out, &mcpserver.ToolFailure{Code: "SOURCE_CATALOG_SAVE_FAILED", Message: saveErr.Error(), RecoverySteps: []string{"The fetched observation is saved under the returned workspace_id and handle. Continue using gorelay_read_observation; do not refetch it to repair a catalog storage error."}}
						}
					}
				}
			}
		}
	}
	return out, err
}

func (b sourceBackend) ReadObservation(ctx context.Context, in mcpserver.ObservationInput) (mcpserver.SourceAction, error) {
	raw, err := json.Marshal(in)
	if err != nil {
		return mcpserver.SourceAction{}, err
	}
	var args map[string]any
	if err := json.Unmarshal(raw, &args); err != nil {
		return mcpserver.SourceAction{}, err
	}
	delete(args, "workspace_id")
	return b.sourceAction(ctx, in.WorkspaceID, "read_data", args)
}

func (b sourceBackend) ValidateProgram(ctx context.Context, in mcpserver.ValidateProgramInput) (mcpserver.SourceAction, error) {
	out := mcpserver.SourceAction{WorkspaceID: in.WorkspaceID}
	crawler, err := b.crawler()
	if err != nil {
		return out, err
	}
	workspace, err := crawler.SourceWorkspace(ctx, in.WorkspaceID)
	if err != nil {
		return out, err
	}
	output := aiapp.TaskSpec{Goal: workspace.Goal, ContractVersion: "v2", SchemaVersion: "v1", JoinKeys: in.JoinKeys}
	for _, field := range in.OutputFields {
		output.RequiredFields = append(output.RequiredFields, aiapp.FieldRequirement{Name: field.Name, Type: field.Type})
	}
	mode := in.Mode
	if mode == "" {
		mode = "acquisition"
	}
	params := in.Params
	if params == nil {
		params = map[string]any{}
	}
	args := map[string]any{"source_id": in.SourceID, "url": in.URL, "code": in.Code, "kind": "adapter", "mode": mode, "purpose": in.Purpose, "params": params, "output_contract": output}
	if in.InputSchema != nil {
		args["input_schema"] = in.InputSchema
	}
	out, err = b.sourceAction(ctx, in.WorkspaceID, "validate_program", args)
	if observation, ok := out.Observation.(map[string]any); ok {
		if candidate, ok := observation["candidate_id"].(string); ok && candidate != "" && err == nil {
			observation["next_action"] = map[string]any{"tool": "gorelay_publish_source", "arguments": map[string]any{"workspace_id": in.WorkspaceID, "candidate_id": candidate, "description": in.Purpose}}
		}
	}
	return out, err
}

func (b sourceBackend) PublishSource(ctx context.Context, in mcpserver.PublishSourceInput) (mcpserver.SourceAction, error) {
	return b.sourceAction(ctx, in.WorkspaceID, "register_source", map[string]any{"candidate_id": in.CandidateID, "description": in.Description})
}

var sourceRecoveryNames = strings.NewReplacer("read_data", "gorelay_read_observation", "inspect_source", "gorelay_inspect_source", "validate_program", "gorelay_validate_program", "register_source", "gorelay_publish_source", "list_sources", "gorelay_find_sources", "submit_acquisition", "gorelay_collect", "get_task_results", "gorelay_get_job", "evaluate_result", "gorelay_get_job", "finish_task", "host answer")

func (b sourceBackend) sourceAction(ctx context.Context, workspaceID, name string, args any) (mcpserver.SourceAction, error) {
	out := mcpserver.SourceAction{WorkspaceID: workspaceID}
	crawler, err := b.crawler()
	if err != nil {
		return out, err
	}
	result, cause := crawler.ExecuteSourceTool(ctx, workspaceID, name, args)
	out.Handle = result.CallID
	if len(result.Data) > 0 {
		if err := json.Unmarshal(result.Data, &out.Observation); err != nil {
			return out, fmt.Errorf("decode saved %s receipt: %w", name, err)
		}
		if observation, ok := out.Observation.(map[string]any); ok {
			// Original bytes remain in the durable receipt. This tool exposes observed
			// structure; original passages are read through the separate evidence path.
			if source, ok := observation["source"].(map[string]any); ok {
				delete(source, "body")
			}
			if publication, ok := observation["publication"].(map[string]any); ok {
				if instruction, ok := publication["instruction"].(string); ok {
					publication["instruction"] = sourceRecoveryNames.Replace(instruction)
				}
			}
			delete(observation, "information")
			delete(observation, "next_action")
			publicObservationReads(observation, workspaceID)
		}
	}
	if cause == nil {
		return out, nil
	}
	failure := &mcpserver.ToolFailure{Code: "SOURCE_TOOL_FAILED", Message: cause.Error(), RecoverySteps: []string{}, Details: map[string]any{"workspace_id": workspaceID, "handle": out.Handle, "original_error": result.Error}}
	if result.Failure != nil {
		failure.Details["diagnostic"] = result.Failure.Details
		if result.Failure.Code != "" {
			failure.Code = result.Failure.Code
		}
		if recovery := result.Failure.Recovery; recovery != nil {
			if recovery.Instruction != "" {
				failure.RecoverySteps = append(failure.RecoverySteps, sourceRecoveryNames.Replace(recovery.Instruction))
			}
			for _, step := range recovery.Steps {
				if step.Instruction != "" {
					failure.RecoverySteps = append(failure.RecoverySteps, sourceRecoveryNames.Replace(step.Instruction))
				}
			}
		}
	}
	if out.Handle != "" {
		failure.Details["read_with"] = map[string]any{"workspace_id": workspaceID, "handle": out.Handle, "view": "receipt"}
		failure.RecoverySteps = append(failure.RecoverySteps, "Call gorelay_read_observation with error.details.read_with to recover the saved diagnostic. Correct the reported real arguments; a new handle or call ID cannot repeat the same business request.")
	} else {
		failure.RecoverySteps = append(failure.RecoverySteps, "Check workspace_id with gorelay_get_job. No tool handle was created; resolve the reported workspace or service error before continuing.")
	}
	return out, failure
}

// Structural continuations are actual public-tool arguments, never internal
// run IDs or a second set of tool names the connected host cannot invoke.
func publicObservationReads(value any, workspace string) {
	switch v := value.(type) {
	case map[string]any:
		for key, item := range v {
			if key == "next_read" || key == "read_with" {
				if args, ok := item.(map[string]any); ok {
					delete(args, "run_id")
					delete(args, "receipt_id")
					delete(args, "fields")
					delete(args, "block_id")
					args["workspace_id"] = workspace
				}
			}
			publicObservationReads(item, workspace)
		}
	case []any:
		for _, item := range v {
			publicObservationReads(item, workspace)
		}
	}
}
