package information

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/agentstatus"
	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/knowledge"
	task "go-lua-crawler/internal/task"
)

type Result struct {
	Records      []aiapp.Record        `json:"records"`
	Coverage     aiapp.CoverageReport  `json:"coverage"`
	Evidence     knowledge.EvidenceSet `json:"evidence"`
	Asset        *Asset                `json:"asset,omitempty"`
	CrawlerRunID string                `json:"crawler_run_id,omitempty"`
	Answer       *aiapp.RAGAnswer      `json:"answer,omitempty"`
	Limitations  []string              `json:"limitations,omitempty"`
}

type routeContext struct {
	Lookup          *PreflightResult       `json:"lookup,omitempty"`
	Search          *aiapp.SearchResponse  `json:"search,omitempty"`
	Observation     json.RawMessage        `json:"observation,omitempty"`
	URL             string                 `json:"url,omitempty"`
	Selection       *routeDecision         `json:"selection,omitempty"`
	Asset           *Asset                 `json:"asset,omitempty"`
	Records         []aiapp.Record         `json:"records,omitempty"`
	LuaFailure      *luaFailureReport      `json:"lua_failure,omitempty"`
	BrowserDecision *browserAssessment     `json:"browser_decision,omitempty"`
	AnswerEvidence  *knowledge.EvidenceSet `json:"answer_evidence,omitempty"`
}

func terminal(stage string) bool {
	return stage == "completed" || stage == "failed" || stage == "cancelled" || stage == "expired"
}

// Advance performs one host-owned stage. The SQL request is separate from every
// crawler Run. RAG results or prose are never inserted into a crawler prompt.
func (s *Service) Advance(parent context.Context, id string) error {
	r, err := s.Request(parent, id)
	if err != nil {
		return err
	}
	if terminal(r.Stage) {
		return nil
	}
	if !r.Deadline.IsZero() && !r.Deadline.After(time.Now()) {
		if r.ChildRunID != "" {
			s.Crawler.CancelWithDelivery(parent, r.ChildRunID)
		}
		return s.DB.WithContext(parent).Model(&Request{}).Where("id = ?", id).Updates(map[string]any{"stage": "expired", "last_error": "information request reached its frozen task-specific deadline"}).Error
	}
	token := agentdata.Token()
	now := time.Now()
	until := now.Add(30 * time.Second)
	claim := s.DB.WithContext(parent).Model(&Request{}).Where("id = ? AND (lease_until IS NULL OR lease_until <= ?) AND stage NOT IN ?", id, now, []string{"completed", "failed", "cancelled", "expired"}).Updates(map[string]any{"lease_token": token, "lease_until": until})
	if claim.Error != nil {
		return claim.Error
	}
	if claim.RowsAffected != 1 {
		return agentdata.ErrConflict
	}
	r.LeaseToken = token
	ctx, cancel := r.RequestContext(parent)
	defer cancel()
	ctx = context.WithValue(ctx, requestExecutionOwnerKey{}, requestExecutionOwner{r.ID, token})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				q := s.DB.WithContext(ctx).Model(&Request{}).Where("id = ? AND lease_token = ? AND stage NOT IN ?", id, token, []string{"cancelled", "expired"}).Update("lease_until", time.Now().Add(30*time.Second))
				if q.Error != nil || q.RowsAffected != 1 {
					cancel()
					return
				}
			}
		}
	}()
	defer func() { cancel(); <-done }()
	var in Input
	json.Unmarshal([]byte(r.InputJSON), &in)
	var state routeContext
	if r.ContextJSON != "" {
		if err := json.Unmarshal([]byte(r.ContextJSON), &state); err != nil {
			return err
		}
	}
	next, cause := s.advanceStage(ctx, &r, in, &state)
	if cause != nil {
		status := aiapp.OperationalStatus(cause)
		r.StatusJSON = string(mustMarshal(status))
		r.FeedbackJSON = s.routingFailureFeedback(parent, r, cause, false)
		if status.Owner == "business" || status.Owner == "system" || status.Code == agentstatus.BrowserAttemptsExhausted || status.Code == agentstatus.AgentExhausted {
			next = "failed"
		}
	}
	updates := map[string]any{"stage": next, "context_json": string(mustMarshal(state)), "spec_json": r.SpecJSON, "child_run_id": r.ChildRunID, "result_json": r.ResultJSON, "last_error": "", "lease_token": "", "lease_until": nil}
	updates["status_json"], updates["feedback_json"] = r.StatusJSON, r.FeedbackJSON
	updates["route_progress_json"] = r.RouteProgressJSON
	if cause != nil {
		updates["last_error"] = cause.Error()
	}
	q := s.DB.WithContext(context.WithoutCancel(parent)).Model(&Request{}).Where("id = ? AND lease_token = ?", r.ID, token).Updates(updates)
	if q.Error != nil {
		return q.Error
	}
	if q.RowsAffected != 1 {
		return agentdata.ErrConflict
	}
	return cause
}

func (s *Service) advanceStage(ctx context.Context, r *Request, in Input, state *routeContext) (string, error) {
	switch r.Stage {
	case "local_lookup":
		lookup, err := s.Preflight(ctx, r.ID)
		if err != nil {
			return "local_lookup", err
		}
		state.Lookup = &lookup
		if r.SpecJSON == "" {
			spec, err := s.freezeSpec(ctx, r, lookup)
			if err != nil {
				return "local_lookup", err
			}
			r.SpecJSON = string(mustMarshal(spec))
			return "local_lookup", nil
		}
		if lookup.Next == "reuse_records" {
			state.Records = lookup.Records
			return s.finish(ctx, r, state, lookup.Evidence)
		}
		if len(lookup.Assets) > 0 {
			return "choose_asset", nil
		}
		return "known_api", nil
	case "choose_asset":
		decision, err := s.decide(ctx, r, "choose_asset", []string{"execute_asset", "no_matching_asset"}, state.Lookup.Assets)
		if err != nil {
			return "choose_asset", err
		}
		if decision.Action == "no_matching_asset" {
			return "known_api", nil
		}
		for _, asset := range state.Lookup.Assets {
			if asset.ID == decision.AssetID {
				state.Asset = &asset
				state.Selection = &decision
				if asset.Kind == "lua" {
					return "start_published_lua", nil
				}
				return "execute_asset", nil
			}
		}
		return "choose_asset", fmt.Errorf("asset_id is not an eligible asset from this request's catalog")
	case "known_api":
		decision, err := s.decide(ctx, r, "known_api", []string{"inspect_api", "no_known_api"}, map[string]any{"entry_urls": in.EntryURLs, "local_lookup": state.Lookup})
		if err != nil {
			return "known_api", err
		}
		if decision.Action == "no_known_api" {
			return "search_api", nil
		}
		state.URL = decision.URL
		return "inspect_api", nil
	case "search_api":
		if s.Search == nil {
			return "start_crawler", fmt.Errorf("API search provider unavailable; no verified search miss is claimed; continuing with the independent crawler")
		}
		query := in.Goal + " public API official documentation JSON endpoint"
		step, fresh, err := s.reserveStep(ctx, *r, "search_api", map[string]any{"query": query, "count": 5})
		if err != nil {
			return "search_api", err
		}
		var result aiapp.SearchResponse
		if fresh {
			result, err = s.Search.Search(ctx, query, 5)
			if save := s.finishStep(context.WithoutCancel(ctx), step, result, err); save != nil {
				return "search_api", save
			}
		} else {
			if step.Stage == "reserved" {
				return "search_api", ErrUnknownStep
			}
			json.Unmarshal([]byte(step.ResultJSON), &result)
			if step.Error != "" {
				err = errors.New(step.Error)
			}
		}
		state.Search = &result
		if err != nil || len(result.Candidates) == 0 {
			return "start_crawler", err
		}
		return "select_api_candidate", nil
	case "select_api_candidate":
		decision, err := s.decide(ctx, r, "select_api_candidate", []string{"inspect_api", "no_known_api"}, state.Search)
		if err != nil {
			return "select_api_candidate", err
		}
		if decision.Action == "no_known_api" {
			return "start_crawler", nil
		}
		state.URL = decision.URL
		return "inspect_searched_api", nil
	case "inspect_api", "inspect_searched_api":
		observation, err := s.observe(ctx, *r, state.URL)
		state.Observation = observation.Structure
		if err != nil || !json.Valid([]byte(observation.Source.Body)) {
			if r.Stage == "inspect_api" {
				return "search_api", err
			}
			return "start_crawler", err
		}
		return "plan_api", nil
	case "plan_api":
		decision, err := s.decide(ctx, r, "plan_api", []string{"trial_api", "api_unsuitable"}, state.Observation)
		if err != nil {
			return "plan_api", err
		}
		if decision.Action == "api_unsuitable" {
			return "start_crawler", nil
		}
		if decision.API == nil {
			return "plan_api", fmt.Errorf("api object with url, items_pointer and fields is required")
		}
		trial, err := s.TrialAPI(ctx, r.ID, decision.Reason, *decision.API)
		if err != nil {
			var failure *ActionError
			if errors.As(err, &failure) && failure.Code == "ACTION_ALREADY_CALLED" {
				var prior Step
				if e := s.DB.WithContext(ctx).First(&prior, "id = ? AND request_id = ? AND kind = ?", failure.PriorStepID, r.ID, "trial_api").Error; e != nil {
					return "plan_api", e
				}
				if prior.Stage != "succeeded" {
					return "plan_api", err
				}
				if e := json.Unmarshal([]byte(prior.ResultJSON), &trial); e != nil {
					return "plan_api", e
				}
				if trial.CandidateID == "" {
					return "plan_api", err
				}
				r.FeedbackJSON = s.routingFailureFeedback(ctx, *r, err, true)
			} else {
				return "plan_api", err
			}
		}
		asset, err := s.Publish(ctx, r.ID, trial.CandidateID)
		if err != nil {
			return "plan_api", err
		}
		state.Asset = &asset
		return "execute_asset", nil
	case "execute_asset":
		if state.Asset == nil {
			return "failed", fmt.Errorf("asset execution state missing")
		}
		rows, err := s.ExecuteAsset(ctx, r.ID, state.Asset.ID)
		if err != nil {
			if state.Asset.Kind == "browser" {
				return "plan_browser", err
			}
			return "start_crawler", err
		}
		state.Records = rows
		return s.finish(ctx, r, state, knowledge.EvidenceSet{Entries: []knowledge.Evidence{}})
	case "start_published_lua":
		if state.Asset == nil || state.Selection == nil {
			return "failed", fmt.Errorf("published Lua selection is missing")
		}
		if err := s.requireEligibleAsset(ctx, *r, *state.Asset); err != nil {
			return "choose_asset", err
		}
		var spec aiapp.TaskSpec
		json.Unmarshal([]byte(r.SpecJSON), &spec)
		spec.RequireSummary = false
		spec.RequireExtractiveSummary = false
		spec.Requirements = nil
		spec.RequirePublishedCapability = true
		selection := state.Selection
		target, err := publishedAssetTarget(in, *state.Asset, *selection)
		if err != nil {
			return "choose_asset", err
		}
		seconds := r.TimeboxSeconds
		child, err := s.Crawler.StartPublishedAcquisition(ctx, aiapp.CreateRunRequest{RequestKey: "information-" + r.ID, Goal: spec.Goal, Spec: &spec, SourceScope: in.SourceScope, EntryURLs: in.EntryURLs, TimeoutSeconds: &seconds, ParentDeadline: &r.Deadline}, state.Asset.SourceID, state.Asset.Version, target, selection.Params, r.Deadline)
		if err != nil {
			return "choose_asset", err
		}
		r.ChildRunID = child.ID
		return "wait_published_lua", nil
	case "start_crawler":
		var spec aiapp.TaskSpec
		json.Unmarshal([]byte(r.SpecJSON), &spec)
		spec.RequireSummary = false
		spec.RequireExtractiveSummary = false
		spec.Requirements = nil
		spec.RequirePublishedCapability = true
		seconds := r.TimeboxSeconds
		child, err := s.Crawler.Create(ctx, aiapp.CreateRunRequest{RequestKey: "information-" + r.ID, Goal: spec.Goal, Spec: &spec, SourceScope: in.SourceScope, EntryURLs: in.EntryURLs, TimeoutSeconds: &seconds, ParentDeadline: &r.Deadline})
		if err != nil {
			return "start_crawler", err
		}
		// The parent timebox bounds child work. No RAG context or article body
		// is passed to this independent crawler run.
		r.ChildRunID = child.ID
		return "wait_crawler", nil
	case "wait_crawler", "wait_published_lua":
		child, err := s.Crawler.Data.Run(ctx, r.ChildRunID)
		if err != nil {
			return r.Stage, err
		}
		deliveryMismatch := false
		if r.Stage == "wait_published_lua" && !child.IsTerminal() {
			completed, e := s.Crawler.CompletePublishedAcquisition(ctx, child.ID)
			if e == nil {
				child = completed
			} else if errors.Is(e, aiapp.ErrPublishedDeliveryMismatch) {
				deliveryMismatch = true
			} else if !errors.Is(e, aiapp.ErrPublishedPending) {
				return r.Stage, e
			}
		}
		links, err := s.Crawler.Data.Children(ctx, child.ID)
		if err != nil {
			return r.Stage, err
		}
		failed := deliveryMismatch
		for _, link := range links {
			var work task.CrawlTask
			if err := s.DB.WithContext(ctx).First(&work, "id = ?", link.TaskID).Error; err != nil {
				return r.Stage, err
			}
			failed = failed || work.Status == task.StatusFailed
		}
		if child.Status == "completed" {
			state.Records, err = s.Crawler.DeliveredAcquisitionRecords(ctx, child.ID)
			if err != nil {
				return r.Stage, err
			}
			if err := s.SyncLuaCatalog(ctx, r.ID); err != nil {
				return r.Stage, err
			}
			return s.finish(ctx, r, state, knowledge.EvidenceSet{Entries: []knowledge.Evidence{}})
		}
		if child.Status == "cancelled" || child.Status == "expired" {
			return child.Status, nil
		}
		if r.Stage == "wait_published_lua" && failed && !child.IsTerminal() {
			if _, err := s.Crawler.ResumePublishedCorrection(ctx, child.ID); err != nil {
				return r.Stage, err
			}
			return "wait_crawler", nil
		}
		// A failed physical attempt belongs to the cold crawler's own repair
		// loop until that Run is terminal.
		if !child.IsTerminal() {
			return r.Stage, nil
		}
		report, err := s.crawlerFailureReport(ctx, child)
		if err != nil {
			return r.Stage, err
		}
		state.LuaFailure = &report
		return "review_browser", nil
	case "review_browser":
		return s.reviewBrowser(ctx, r, in, state)
	case "inspect_browser":
		if s.Browser == nil {
			return "failed", fmt.Errorf("confirmed browser capability required but browser runtime is not configured")
		}
		target := state.URL
		if target == "" && len(in.EntryURLs) > 0 {
			target = in.EntryURLs[0]
		}
		if target == "" {
			return "failed", fmt.Errorf("browser fallback needs an observed source URL")
		}
		step, fresh, err := s.reserveBrowserStep(ctx, *r, "inspect_browser", target, map[string]any{"url": target, "scope": in.SourceScope})
		if err != nil {
			return "inspect_browser", err
		}
		var structure json.RawMessage
		if fresh {
			fetcher, err := s.NewFetcher(in.SourceScope)
			if err != nil {
				return "inspect_browser", err
			}
			runtime, cause := s.Browser.Render(ctx, BrowserPlan{URL: target}, in.SourceScope, fetcher)
			fetcher.Close()
			if cause == nil && len(runtime.SourceEvidence) > 0 {
				structure = aiapp.SourceStructure(runtime.SourceEvidence[0])
			}
			if err := s.finishStep(context.WithoutCancel(ctx), step, map[string]any{"structure": structure, "runtime": runtime}, cause); err != nil {
				return "inspect_browser", err
			}
			if cause != nil {
				return "failed", cause
			}
		} else {
			var saved struct {
				Structure json.RawMessage `json:"structure"`
			}
			json.Unmarshal([]byte(step.ResultJSON), &saved)
			structure = saved.Structure
			if step.Stage != "succeeded" {
				return "failed", fmt.Errorf("saved browser inspection: %s", step.Error)
			}
		}
		state.Observation = structure
		state.URL = target
		return "plan_browser", nil
	case "plan_browser":
		decision, err := s.decide(ctx, r, "plan_browser", []string{"trial_browser", "browser_unsuitable"}, state.Observation)
		if err != nil {
			return "plan_browser", err
		}
		if decision.Action == "browser_unsuitable" {
			return "failed", fmt.Errorf("browser source unsuitable: %s", decision.Reason)
		}
		if decision.Browser == nil {
			return "plan_browser", fmt.Errorf("browser plan with url and observed field selectors is required")
		}
		trial, err := s.TrialBrowser(ctx, r.ID, decision.Reason, *decision.Browser)
		if err != nil {
			return "plan_browser", err
		}
		asset, err := s.Publish(ctx, r.ID, trial.CandidateID)
		if err != nil {
			return "plan_browser", err
		}
		state.Asset = &asset
		return "execute_asset", nil
	case "await_evidence":
		set, err := s.currentAnswerEvidence(ctx, *r, in, state)
		if errors.Is(err, errEvidencePending) {
			return "await_evidence", nil
		}
		if err != nil {
			if aiapp.OperationalStatus(err).Code == agentstatus.SourceUnavailable {
				return "failed", err
			}
			return "await_evidence", err
		}
		state.AnswerEvidence = &set
		return "answer_information", nil
	case "answer_information":
		return s.answerFromEvidence(ctx, r, in, state)
	default:
		return "failed", fmt.Errorf("unknown information stage %q", r.Stage)
	}
}

func (s *Service) finish(ctx context.Context, r *Request, state *routeContext, evidence knowledge.EvidenceSet) (string, error) {
	var spec aiapp.TaskSpec
	if err := json.Unmarshal([]byte(r.SpecJSON), &spec); err != nil {
		return "failed", err
	}
	if err := knowledge.CheckReferences(ctx, knowledge.SetReader{Store: s.Crawler.Knowledge, Set: evidence}, evidenceIDs(evidence)); err != nil {
		return "local_lookup", err
	}
	merged, err := aiapp.MergeRecords(state.Records, spec.JoinKeys)
	if err != nil {
		return "failed", err
	}
	coverage, err := aiapp.EvaluateMergedRecords(spec, merged, time.Now())
	if err != nil {
		return "failed", err
	}
	if !coverage.Satisfied {
		return "known_api", fmt.Errorf("records no longer satisfy requested fields, source support or freshness: %+v", coverage)
	}
	result := Result{Records: state.Records, Coverage: coverage, Evidence: evidence, Asset: state.Asset, CrawlerRunID: r.ChildRunID}
	r.ResultJSON = string(mustMarshal(result))
	if spec.RequireSummary {
		return "await_evidence", nil
	}
	r.StatusJSON = ""
	r.FeedbackJSON = ""
	return "completed", nil
}
func evidenceIDs(set knowledge.EvidenceSet) []string {
	out := []string{}
	for _, e := range set.Entries {
		out = append(out, e.ID)
	}
	return out
}
