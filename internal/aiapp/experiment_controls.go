package aiapp

import (
	"context"
	"encoding/json"
	"fmt"
	"go-lua-crawler/internal/agentdata"
	"strconv"
)

type CapabilityRef struct {
	SourceID string `json:"source_id"`
	Version  string `json:"version"`
}
type ExperimentControls struct {
	DisableCapabilityReuse bool            `json:"disable_capability_reuse"`
	DisableKnowledgeReuse  bool            `json:"disable_knowledge_reuse"`
	InitialCapabilities    []CapabilityRef `json:"initial_capabilities,omitempty"`
}

func experimentControls(r agentdata.Run) ExperimentControls {
	var request CreateRunRequest
	_ = json.Unmarshal([]byte(r.InputJSON), &request)
	if request.Experiment == nil {
		return ExperimentControls{}
	}
	return *request.Experiment
}
func (s *Service) captureExperiment(ctx context.Context) (ExperimentControls, error) {
	var controls ExperimentControls
	var err error
	controls.DisableCapabilityReuse, err = strconv.ParseBool(envDefault("GORELAY_DISABLE_CAPABILITY_REUSE", "false"))
	if err != nil {
		return controls, fmt.Errorf("invalid capability reuse control")
	}
	controls.DisableKnowledgeReuse, err = strconv.ParseBool(envDefault("GORELAY_DISABLE_KNOWLEDGE_REUSE", "false"))
	if err != nil {
		return controls, fmt.Errorf("invalid knowledge reuse control")
	}
	if controls.DisableCapabilityReuse {
		sources, err := s.Data.ListSources(ctx)
		if err != nil {
			return controls, err
		}
		for _, source := range sources {
			controls.InitialCapabilities = append(controls.InitialCapabilities, CapabilityRef{source.SourceID, source.ArtifactHash})
		}
	}
	return controls, nil
}
func (s *Service) canUseCapability(ctx context.Context, r agentdata.Run, source, version string) (bool, error) {
	controls := experimentControls(r)
	if !controls.DisableCapabilityReuse {
		return true, nil
	}
	blocked := false
	for _, ref := range controls.InitialCapabilities {
		if ref.SourceID == source && ref.Version == version {
			blocked = true
			break
		}
	}
	if !blocked {
		return true, nil
	}
	// A freshly generated and trialled candidate within this Run is not reuse of
	// the initial capability, even if immutable code happens to deduplicate.
	var n int64
	err := s.Data.DB.WithContext(ctx).Model(&Candidate{}).Where("run_id = ? AND source_id = ? AND artifact_hash = ?", r.ID, source, version).Count(&n).Error
	return n > 0, err
}
