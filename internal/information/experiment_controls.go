package information

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"

	"go-lua-crawler/internal/agentdata"
)

// Policy is host configuration, never model arguments. The receipt is frozen
// at request admission; changing a CLI variant label cannot change this policy.
type ExperimentPolicy struct {
	ActionMode             string `json:"action_mode"`
	DisableCapabilityReuse bool   `json:"disable_capability_reuse"`
	DisableKnowledgeReuse  bool   `json:"disable_knowledge_reuse"`
}
type AssetRef struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}
type ExperimentControls struct {
	ExperimentPolicy
	InitialAssets []AssetRef `json:"initial_assets"`
}

func configuredExperimentPolicy() (ExperimentPolicy, error) {
	out := ExperimentPolicy{ActionMode: os.Getenv("GORELAY_ACTION_MODE")}
	if out.ActionMode == "" {
		out.ActionMode = "code"
	}
	if out.ActionMode != "code" && out.ActionMode != "per_tool" {
		return out, fmt.Errorf("GORELAY_ACTION_MODE must be code or per_tool")
	}
	for _, setting := range []struct {
		name string
		dest *bool
	}{{"GORELAY_DISABLE_CAPABILITY_REUSE", &out.DisableCapabilityReuse}, {"GORELAY_DISABLE_KNOWLEDGE_REUSE", &out.DisableKnowledgeReuse}} {
		text := os.Getenv(setting.name)
		if text == "" {
			text = "false"
		}
		value, err := strconv.ParseBool(text)
		if err != nil {
			return out, fmt.Errorf("invalid %s", setting.name)
		}
		*setting.dest = value
	}
	return out, nil
}

func (s *Service) captureExperiment(ctx context.Context) (ExperimentControls, error) {
	policy, err := configuredExperimentPolicy()
	out := ExperimentControls{ExperimentPolicy: policy, InitialAssets: []AssetRef{}}
	if err != nil {
		return out, err
	}
	var assets []Asset
	if err = s.DB.WithContext(ctx).Where("active = ? AND kind <> ?", true, "lua").Order("id").Find(&assets).Error; err != nil {
		return out, err
	}
	for _, a := range assets {
		out.InitialAssets = append(out.InitialAssets, AssetRef{a.ID, a.Version})
	}
	if s.Crawler != nil {
		sources, err := s.Crawler.Data.ListSources(ctx)
		if err != nil {
			return out, err
		}
		for _, source := range sources {
			out.InitialAssets = append(out.InitialAssets, AssetRef{agentdata.Hash([]string{"lua", source.SourceID, source.ArtifactHash}), source.ArtifactHash})
		}
	}
	sort.Slice(out.InitialAssets, func(i, j int) bool { return out.InitialAssets[i].ID < out.InitialAssets[j].ID })
	return out, nil
}

func requestExperiment(r Request) (*ExperimentControls, error) {
	if r.ExperimentJSON == "" {
		return nil, nil
	} // Historical receipts are unknown, not retroactively frozen.
	var out ExperimentControls
	err := json.Unmarshal([]byte(r.ExperimentJSON), &out)
	return &out, err
}

func (s *Service) canUseAsset(ctx context.Context, r Request, a Asset) (bool, error) {
	controls, err := requestExperiment(r)
	if err != nil {
		return false, err
	}
	if controls == nil || !controls.DisableCapabilityReuse {
		return true, nil
	}
	blocked := false
	for _, ref := range controls.InitialAssets {
		if ref.ID == a.ID && ref.Version == a.Version {
			blocked = true
			break
		}
	}
	if !blocked {
		return true, nil
	}
	// A newly trialled plan is construction within this request, even when its
	// immutable content deduplicates to a previously published asset.
	var trials []AssetTrial
	if err = s.DB.WithContext(ctx).Where("request_id = ?", r.ID).Find(&trials).Error; err != nil {
		return false, err
	}
	for _, trial := range trials {
		var candidate Asset
		if json.Unmarshal([]byte(trial.AssetJSON), &candidate) == nil && candidate.ID == a.ID && candidate.Version == a.Version {
			return true, nil
		}
	}
	return false, nil
}

func (s *Service) requireEligibleAsset(ctx context.Context, r Request, a Asset) error {
	allowed, err := s.canUseAsset(ctx, r, a)
	if err != nil {
		return err
	}
	if !allowed {
		return actionFailure("EXPERIMENT_REUSE_DISABLED", fmt.Errorf("this request's frozen policy excludes the initial asset %s version %s", a.ID, a.Version), "Construct and trial a fresh acquisition plan for this request; do not resubmit the excluded initial asset.")
	}
	return nil
}
