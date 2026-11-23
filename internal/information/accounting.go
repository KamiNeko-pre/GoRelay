package information

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/knowledge"
)

type RetrievalAccounting struct {
	ID      string                      `json:"id"`
	Purpose string                      `json:"purpose"`
	Stage   string                      `json:"stage"`
	Metrics *knowledge.RetrievalMetrics `json:"metrics"`
	Error   string                      `json:"error,omitempty"`
}

type RouteSemanticResult struct {
	Status      string                    `json:"status"`
	Model       string                    `json:"model,omitempty"`
	ModelStepID uint64                    `json:"model_step_id"`
	RequestID   string                    `json:"provider_request_id,omitempty"`
	Usage       *knowledge.EmbeddingUsage `json:"usage"`
	BatchSize   int                       `json:"batch_size"`
	DurationMS  int64                     `json:"duration_ms"`
	Repeated    bool                      `json:"repeated"`
	Similarity  float64                   `json:"similarity"`
	Error       string                    `json:"error,omitempty"`
}

type RouteSemanticAccounting struct {
	ID     string              `json:"id"`
	Stage  string              `json:"stage"`
	Result RouteSemanticResult `json:"result"`
}

// This is a read-only projection of durable receipts, never a model retry or a
// second ledger of totals. ModelCalls includes upper and child calls exactly
// once; ChildOperations remains available for non-model resource diagnostics.
type RequestAccounting struct {
	SemanticChecks  []RouteSemanticAccounting   `json:"semantic_checks"`
	AsOf            time.Time                   `json:"as_of"`
	RequestID       string                      `json:"request_id"`
	ChildRunIDs     []string                    `json:"child_run_ids,omitempty"`
	ChildRunID      string                      `json:"child_run_id,omitempty"`
	ChildActionMode string                      `json:"child_action_mode,omitempty"`
	ModelCalls      []aiapp.ModelCallAccounting `json:"model_calls"`
	SearchCalls     []string                    `json:"search_calls"`
	Retrievals      []RetrievalAccounting       `json:"retrievals"`
	ChildOperations []agentdata.Operation       `json:"child_operations"`
	RunAccounting   *aiapp.RunAccounting        `json:"run_accounting"`
}

func (s *Service) Accounting(ctx context.Context, r Request) (RequestAccounting, error) {
	var steps []Step
	if err := s.DB.WithContext(ctx).Where("request_id = ?", r.ID).Order("id").Find(&steps).Error; err != nil {
		return RequestAccounting{}, err
	}
	return s.accountingFromSteps(ctx, r, steps)
}

func (s *Service) accountingFromSteps(ctx context.Context, r Request, steps []Step) (RequestAccounting, error) {
	out := RequestAccounting{AsOf: time.Now(), RequestID: r.ID, ChildRunID: r.ChildRunID, ModelCalls: []aiapp.ModelCallAccounting{}, SearchCalls: []string{}, Retrievals: []RetrievalAccounting{}, ChildOperations: []agentdata.Operation{}}
	out.SemanticChecks = []RouteSemanticAccounting{}
	for _, step := range steps {
		id := fmt.Sprintf("information:%s/step:%d", r.ID, step.ID)
		switch step.Kind {
		case "route_semantic":
			check := RouteSemanticAccounting{ID: id, Stage: step.Stage, Result: RouteSemanticResult{Status: "outcome_unknown"}}
			if step.ResultJSON != "" {
				if err := json.Unmarshal([]byte(step.ResultJSON), &check.Result); err != nil {
					check.Result.Error = err.Error()
				}
			}
			out.SemanticChecks = append(out.SemanticChecks, check)
		case "route_model", "eino_model", "eino_summary", "delivery_audit", "search_api":
			call := aiapp.ModelCallAccounting{ID: id, ParentID: "information:" + r.ID, Scope: "information", Purpose: step.Kind, Stage: step.Stage, InputHash: step.InputHash, Error: step.Error}
			if step.Kind != "search_api" {
				var request aiapp.ChatRequest
				if json.Unmarshal([]byte(step.InputJSON), &request) == nil {
					call.PromptHash, call.ToolSchemaHash = agentdata.Hash(request.Messages), agentdata.Hash(request.Tools)
					if len(request.Tools) == 1 {
						call.Purpose = request.Tools[0].Function.Name
					}
				}
				var response aiapp.ChatResponse
				if err := json.Unmarshal([]byte(step.ResultJSON), &response); err != nil {
					call.Error = "decode response: " + err.Error()
				}
				call.Model, call.RequestID, call.DurationMS, call.Usage = response.Model, response.RequestID, response.DurationMS, response.Usage
				call.TTFTMS, call.DecodeMS = response.TTFTMS, response.DecodeMS
				call.ReasoningChars, call.ContentChars, call.ToolArgsChars = response.ReasoningChars, response.ContentChars, response.ToolArgsChars
			} else {
				out.SearchCalls = append(out.SearchCalls, id)
				var response aiapp.SearchResponse
				if err := json.Unmarshal([]byte(step.ResultJSON), &response); err != nil {
					call.Error = "decode search response: " + err.Error()
				}
				// A search service need not invoke an LLM. Search fees are separate.
				if response.Model == "" && response.Usage == nil {
					continue
				}
				call.Model, call.RequestID, call.DurationMS, call.Usage = response.Model, response.RequestID, response.DurationMS, response.Usage
			}
			out.ModelCalls = append(out.ModelCalls, call)
		case "post_acquisition_rag":
			retrieval := RetrievalAccounting{ID: id, Purpose: step.Kind, Stage: step.Stage, Error: step.Error}
			var set knowledge.EvidenceSet
			var err error
			err = json.Unmarshal([]byte(step.ResultJSON), &set)
			if err == nil && step.Stage != "reserved" {
				retrieval.Metrics = &set.Metrics
			} else if err != nil {
				retrieval.Error = "decode retrieval: " + err.Error()
			}
			out.Retrievals = append(out.Retrievals, retrieval)
		}
	}
	accounting := aiapp.RunAccounting{AsOf: out.AsOf, IndexAttempts: []knowledge.IndexAttempt{}, IndexWork: []knowledge.IndexWork{}, IndexSettled: true}
	candidates := []string{r.ChildRunID}
	for _, step := range steps {
		candidates = append(candidates, step.ChildRunID)
	}
	seenRuns := map[string]bool{}
	for _, id := range candidates {
		if id == "" || seenRuns[id] {
			continue
		}
		seenRuns[id] = true
		out.ChildRunIDs = append(out.ChildRunIDs, id)
		if s.Crawler == nil {
			return out, fmt.Errorf("child crawler accounting unavailable")
		}
		run, err := s.Crawler.Data.Run(ctx, id)
		if err != nil {
			return out, err
		}
		if id == r.ChildRunID {
			var input aiapp.CreateRunRequest
			if err = json.Unmarshal([]byte(run.InputJSON), &input); err != nil {
				return out, err
			}
			out.ChildActionMode = input.ActionMode
		}
		operations, err := s.Crawler.Data.Operations(ctx, id)
		if err != nil {
			return out, err
		}
		childAccounting, err := s.Crawler.Accounting(ctx, run)
		if err != nil {
			return out, err
		}
		accounting.IndexSettled = accounting.IndexSettled && childAccounting.IndexSettled
		accounting.IndexWork = append(accounting.IndexWork, childAccounting.IndexWork...)
		accounting.IndexAttempts = append(accounting.IndexAttempts, childAccounting.IndexAttempts...)
		if accounting.Interventions == nil {
			accounting.Interventions = []aiapp.HumanIntervention{}
		}
		accounting.Interventions = append(accounting.Interventions, childAccounting.Interventions...)
		accounting.Replacements = append(accounting.Replacements, childAccounting.Replacements...)
		out.ChildOperations = append(out.ChildOperations, operations...)
		out.ModelCalls = append(out.ModelCalls, aiapp.CrawlerModelCalls(operations)...)
		for _, op := range operations {
			if op.Kind == "discover_sources" {
				out.SearchCalls = append(out.SearchCalls, fmt.Sprintf("crawler:%s/operation:%d", id, op.ID))
			}
		}
	}
	// Indexing belongs to the actual triggering request, including native
	// acquisitions whose only execution receipt is the host Step.
	query := s.DB.WithContext(ctx).Where("origin_request_id = ?", r.ID)
	var work []knowledge.IndexWork
	if err := query.Order("id").Find(&work).Error; err != nil {
		return out, err
	}
	seenWork := map[uint64]bool{}
	for _, w := range accounting.IndexWork {
		seenWork[w.ID] = true
	}
	var workIDs []uint64
	for _, w := range work {
		if seenWork[w.ID] {
			continue
		}
		seenWork[w.ID] = true
		workIDs = append(workIDs, w.ID)
		accounting.IndexWork = append(accounting.IndexWork, w)
		if w.Status != "ready" && w.Status != "superseded" {
			accounting.IndexSettled = false
		}
	}
	if len(workIDs) > 0 {
		var attempts []knowledge.IndexAttempt
		if err := s.DB.WithContext(ctx).Where("work_id IN ?", workIDs).Order("id").Find(&attempts).Error; err != nil {
			return out, err
		}
		accounting.IndexAttempts = append(accounting.IndexAttempts, attempts...)
	}
	accounting.InterventionAssessment = aiapp.AssessInterventions(&accounting)
	out.RunAccounting = &accounting
	return out, nil
}
