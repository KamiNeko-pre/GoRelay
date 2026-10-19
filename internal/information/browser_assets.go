package information

import (
	"context"
	"fmt"
	"time"

	"go-lua-crawler/internal/agentdata"
)

func (s *Service) TrialBrowser(ctx context.Context, id, description string, plan BrowserPlan) (TrialResult, error) {
	r, in, spec, err := s.trialRequest(ctx, id)
	if err != nil {
		return TrialResult{}, err
	}
	if s.Browser == nil {
		return TrialResult{}, actionFailure("BROWSER_UNAVAILABLE", fmt.Errorf("browser runtime is not configured"), "Configure the independent browser runtime; changing the plan cannot install a missing runtime.", "Use a verified API or the independent crawler when the source supports it.")
	}
	if !urlInScope(plan.URL, in.SourceScope) {
		return TrialResult{}, actionFailure("ASSET_SCOPE_INVALID", fmt.Errorf("browser URL is outside the original scope or malformed"), "Use an observed HTTP(S) URL inside the granted scope.")
	}
	step, fresh, err := s.reserveBrowserStep(ctx, r, "trial_browser", plan.URL, map[string]any{"browser": plan, "scope": in.SourceScope, "spec": spec})
	if err != nil {
		return TrialResult{}, err
	}
	if !fresh {
		return TrialResult{}, duplicateStep(step)
	}
	ctx, cancel := r.RequestContext(ctx)
	defer cancel()
	fetcher, err := s.NewFetcher(in.SourceScope)
	if err != nil {
		s.finishStep(context.WithoutCancel(ctx), step, nil, err)
		return TrialResult{}, err
	}
	defer fetcher.Close()
	runtime, cause := s.Browser.Render(ctx, plan, in.SourceScope, fetcher)
	asset := Asset{Kind: "browser", SourceID: "browser-" + agentdata.Hash(plan)[:24], Version: agentdata.Hash([]any{plan, in.SourceScope, spec}), Description: description, EntryURL: plan.URL, SpecJSON: r.SpecJSON, ScopeJSON: string(mustMarshal(in.SourceScope)), ManifestJSON: string(mustMarshal(plan)), VerifiedAt: time.Now()}
	asset.ID = agentdata.Hash([]string{asset.Kind, asset.SourceID, asset.Version})
	return s.saveTrial(ctx, r, step, asset, runtime, cause)
}
