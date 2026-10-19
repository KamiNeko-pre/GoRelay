package information

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/knowledge"
	"go-lua-crawler/internal/task"
)

type RetrievalAccounting struct {
	ID      string                      `json:"id"`
	Purpose string                      `json:"purpose"`
	Stage   string                      `json:"stage"`
	Metrics *knowledge.RetrievalMetrics `json:"metrics"`
	Error   string                      `json:"error,omitempty"`
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
	ChildRunID      string                      `json:"child_run_id,omitempty"`
	ChildActionMode string                      `json:"child_action_mode,omitempty"`
	ModelCalls      []aiapp.ModelCallAccounting `json:"model_calls"`
	SearchCalls     []string                    `json:"search_calls"`
	Retrievals      []RetrievalAccounting       `json:"retrievals"`
	ChildOperations []agentdata.Operation       `json:"child_operations"`
	NativeTaskIDs   []uint64                    `json:"native_task_ids"`
	RunAccounting   *aiapp.RunAccounting        `json:"run_accounting"`
	Experiment      *ExperimentControls         `json:"experiment"`
}

func (s *Service) Accounting(ctx context.Context, r Request) (RequestAccounting, error) {
	var steps []Step
	if err := s.DB.WithContext(ctx).Where("request_id = ?", r.ID).Order("id").Find(&steps).Error; err != nil {
		return RequestAccounting{}, err
	}
	return s.accountingFromSteps(ctx, r, steps)
}

func (s *Service) accountingFromSteps(ctx context.Context, r Request, steps []Step) (RequestAccounting, error) {
	out := RequestAccounting{AsOf: time.Now(), RequestID: r.ID, ChildRunID: r.ChildRunID, ModelCalls: []aiapp.ModelCallAccounting{}, SearchCalls: []string{}, Retrievals: []RetrievalAccounting{}, ChildOperations: []agentdata.Operation{}, NativeTaskIDs: []uint64{}}
	out.SemanticChecks = []RouteSemanticAccounting{}
	controls, err := requestExperiment(r)
	if err != nil {
		return out, err
	}
	out.Experiment = controls
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
		case "route_model", "search_api":
			call := aiapp.ModelCallAccounting{ID: id, ParentID: "information:" + r.ID, Scope: "information", Purpose: step.Kind, Stage: step.Stage, InputHash: step.InputHash, Error: step.Error}
			if step.Kind == "route_model" {
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
		case "local_lookup", "post_acquisition_rag":
			retrieval := RetrievalAccounting{ID: id, Purpose: step.Kind, Stage: step.Stage, Error: step.Error}
			var set knowledge.EvidenceSet
			var err error
			if step.Kind == "local_lookup" {
				var preflight PreflightResult
				err = json.Unmarshal([]byte(step.ResultJSON), &preflight)
				set = preflight.Evidence
				retrieval.Error = preflight.RetrievalError
			} else {
				err = json.Unmarshal([]byte(step.ResultJSON), &set)
			}
			if err == nil && step.Stage != "reserved" {
				retrieval.Metrics = &set.Metrics
			} else if err != nil {
				retrieval.Error = "decode retrieval: " + err.Error()
			}
			out.Retrievals = append(out.Retrievals, retrieval)
		}
	}
	accounting := aiapp.RunAccounting{AsOf: out.AsOf, IndexAttempts: []knowledge.IndexAttempt{}, IndexWork: []knowledge.IndexWork{}, IndexSettled: true}
	if r.ChildRunID != "" {
		if s.Crawler == nil {
			return out, fmt.Errorf("child crawler accounting unavailable")
		}
		run, err := s.Crawler.Data.Run(ctx, r.ChildRunID)
		if err != nil {
			return out, err
		}
		var input aiapp.CreateRunRequest
		if err = json.Unmarshal([]byte(run.InputJSON), &input); err != nil {
			return out, err
		}
		out.ChildActionMode = input.ActionMode
		out.ChildOperations, err = s.Crawler.Data.Operations(ctx, run.ID)
		if err != nil {
			return out, err
		}
		accounting, err = s.Crawler.Accounting(ctx, run)
		if err != nil {
			return out, err
		}
		out.ModelCalls = append(out.ModelCalls, aiapp.CrawlerModelCalls(out.ChildOperations)...)
		for _, op := range out.ChildOperations {
			if op.Kind == "discover_sources" {
				out.SearchCalls = append(out.SearchCalls, fmt.Sprintf("crawler:%s/operation:%d", run.ID, op.ID))
			}
		}
	}
	// Native API/Browser tasks deliberately have no crawler Run. Their durable
	// request keys are minted by commitNative, and index work records TaskID.
	if err := s.DB.WithContext(ctx).Model(&task.CrawlTask{}).Where("request_key LIKE ?", "information-"+r.ID+"-%").Order("id").Pluck("id", &out.NativeTaskIDs).Error; err != nil {
		return out, err
	}
	// Metadata indexing can have TaskID=0. Attribute its actual triggering
	// request explicitly; retain the task join for receipts predating this field.
	query := s.DB.WithContext(ctx).Where("origin_request_id = ?", r.ID)
	if len(out.NativeTaskIDs) > 0 {
		query = query.Or("origin_task_id IN ?", out.NativeTaskIDs)
	}
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
