package information

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/knowledge"
	"go-lua-crawler/internal/repository"
	"gorm.io/gorm"
)

type PreflightResult struct {
	RetrievalStatus string                `json:"retrieval_status"`
	RetrievalError  string                `json:"retrieval_error,omitempty"`
	Evidence        knowledge.EvidenceSet `json:"evidence"`
	Records         []aiapp.Record        `json:"records"`
	Coverage        aiapp.CoverageReport  `json:"coverage"`
	Assets          []Asset               `json:"assets"`
	Next            string                `json:"next"`
	CheckedAt       time.Time             `json:"checked_at"`
	Warnings        []string              `json:"warnings,omitempty"`
	Experiment      *ExperimentControls   `json:"experiment"`
}

func (s *Service) Preflight(ctx context.Context, id string) (PreflightResult, error) {
	r, err := s.Request(ctx, id)
	if err != nil {
		return PreflightResult{}, err
	}
	step, fresh, err := s.reserveStep(ctx, r, "local_lookup", map[string]any{"goal_input": r.InputHash, "spec": r.SpecJSON, "refresh_required": r.RefreshRequired})
	if err != nil {
		return PreflightResult{}, err
	}
	if !fresh {
		if step.Stage == "reserved" {
			return PreflightResult{}, ErrUnknownStep
		}
		if step.Stage == "failed" {
			return PreflightResult{}, fmt.Errorf("saved local lookup failed: %s", step.Error)
		}
		var out PreflightResult
		err = json.Unmarshal([]byte(step.ResultJSON), &out)
		return out, err
	}
	out, cause := s.lookup(ctx, r)
	if err := s.finishStep(context.WithoutCancel(ctx), step, out, cause); err != nil {
		return out, err
	}
	return out, cause
}

func (s *Service) lookup(ctx context.Context, r Request) (PreflightResult, error) {
	out := PreflightResult{RetrievalStatus: "unavailable", Next: "known_api", CheckedAt: time.Now(), Records: []aiapp.Record{}, Assets: []Asset{}, Evidence: knowledge.EvidenceSet{Entries: []knowledge.Evidence{}}}
	var in Input
	if err := json.Unmarshal([]byte(r.InputJSON), &in); err != nil {
		return out, err
	}
	controls, err := requestExperiment(r)
	if err != nil {
		return out, err
	}
	out.Experiment = controls
	if err := s.SyncLuaCatalog(ctx, r.ID); err != nil {
		return out, err
	}
	// Retrieval runs before any acquisition, even when the local catalog or
	// caller scope already provides an obvious refresh candidate.
	if controls != nil && controls.DisableKnowledgeReuse {
		out.RetrievalStatus = "disabled_by_experiment"
	} else if s.Retriever == nil {
		out.RetrievalError = "embedding/vector retrieval is not configured; this is not a verified local miss"
	} else {
		policy := s.Retriever.Policy
		policy.Scope.URLHosts = in.SourceScope
		set, err := s.Retriever.RetrieveWithPolicy(ctx, in.Goal, 10, policy)
		// Embedding may already have completed before vector retrieval fails.
		// Retain resource measurements without admitting failed partial evidence.
		out.Evidence.Metrics = set.Metrics
		if err != nil {
			out.RetrievalError = err.Error()
		} else {
			out.RetrievalStatus = "checked"
			out.Evidence = set
		}
	}
	var spec aiapp.TaskSpec
	if r.SpecJSON != "" {
		if err := json.Unmarshal([]byte(r.SpecJSON), &spec); err != nil {
			return out, err
		}
		in.Spec = &spec
	}
	seen := map[string]bool{}
	for _, ev := range out.Evidence.Entries {
		if ev.TaskID == 0 || ev.RecordKey == "" || !urlInScope(ev.URL, in.SourceScope) {
			continue
		}
		if _, err := (knowledge.SetReader{Store: s.Crawler.Knowledge, Set: out.Evidence}).ReadEvidence(ctx, ev.ID); err != nil {
			out.Warnings = append(out.Warnings, err.Error())
			continue
		}
		var saved repository.CrawlResult
		if err := s.DB.WithContext(ctx).First(&saved, "task_id = ?", ev.TaskID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				out.Warnings = append(out.Warnings, "retrieved evidence has no acquired record snapshot")
				continue
			}
			return out, err
		}
		var acquired aiapp.AcquisitionResult
		if err := json.Unmarshal([]byte(saved.Payload), &acquired); err != nil {
			return out, err
		}
		for _, row := range acquired.Records {
			if row.Key != ev.RecordKey || row.Provenance.TaskID != ev.TaskID || row.Provenance.DataVersion != ev.DataVersion || !urlInScope(row.Provenance.URL, in.SourceScope) {
				continue
			}
			key := agentdata.Hash([]any{ev.TaskID, row.Key, row.Provenance.DataVersion})
			if seen[key] {
				continue
			}
			seen[key] = true
			out.Records = append(out.Records, row)
		}
	}
	assets, err := s.localAssets(ctx, in)
	if err != nil {
		return out, err
	}
	for _, asset := range assets {
		allowed, err := s.canUseAsset(ctx, r, asset)
		if err != nil {
			return out, err
		}
		if allowed {
			out.Assets = append(out.Assets, asset)
		}
	}
	if r.SpecJSON != "" {
		merged, err := aiapp.MergeRecords(out.Records, spec.JoinKeys)
		if err != nil {
			return out, err
		}
		out.Coverage, err = aiapp.EvaluateMergedRecords(spec, merged, out.CheckedAt)
		if err != nil {
			return out, err
		}
		if out.Coverage.Satisfied && !in.Refresh && !r.RefreshRequired {
			out.Next = "reuse_records"
			return out, nil
		}
	}
	if len(out.Assets) > 0 {
		out.Next = "refresh_asset"
	}
	return out, nil
}

// The SQL catalog is also checked: vector publication lag/outage cannot turn
// an existing executable asset into a false negative. Relevance is narrowed by
// the caller's immutable output and network contract before reaching a model.
func (s *Service) localAssets(ctx context.Context, in Input) ([]Asset, error) {
	out := []Asset{}
	sources, err := s.Crawler.Data.ListSources(ctx)
	if err != nil {
		return nil, err
	}
	for _, source := range sources {
		program, err := s.Crawler.Data.Artifact(ctx, source.ArtifactHash)
		if err != nil {
			return nil, err
		}
		a := Asset{ID: agentdata.Hash([]string{"lua", source.SourceID, source.ArtifactHash}), Kind: "lua", SourceID: source.SourceID, Version: source.ArtifactHash, Description: source.Description, SpecJSON: program.OutputSchema, ScopeJSON: program.ScopeJSON, ManifestJSON: string(mustMarshal(map[string]any{"source_id": source.SourceID, "artifact_hash": source.ArtifactHash, "input_schema": program.InputSchema, "input_example": program.InputExampleJSON})), Active: true, VerifiedAt: source.CreatedAt}
		if assetFits(a, in) {
			out = append(out, a)
		}
	}
	var native []Asset
	if err := s.DB.WithContext(ctx).Where("active = ? AND kind <> ?", true, "lua").Order("created_at,id").Find(&native).Error; err != nil {
		return nil, err
	}
	for _, a := range native {
		if assetFits(a, in) {
			out = append(out, a)
		}
	}
	return out, nil
}

func assetFits(a Asset, in Input) bool {
	var domains []string
	if json.Unmarshal([]byte(a.ScopeJSON), &domains) != nil {
		return false
	}
	if len(in.SourceScope) > 0 && len(domains) == 0 {
		return false
	}
	for _, d := range domains {
		if !hostInScope(d, in.SourceScope) {
			return false
		}
	}
	if in.Spec == nil {
		return true
	}
	var spec aiapp.TaskSpec
	if json.Unmarshal([]byte(a.SpecJSON), &spec) != nil || spec.SchemaVersion != in.Spec.SchemaVersion {
		return false
	}
	available := map[string]string{}
	for _, f := range spec.RequiredFields {
		available[f.Name] = f.Type
	}
	for _, f := range in.Spec.RequiredFields {
		if t, ok := available[f.Name]; !ok || t != f.Type && !(t == "integer" && f.Type == "number") {
			return false
		}
	}
	return true
}
