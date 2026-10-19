package information

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/agentstatus"
	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/knowledge"
)

type routeProgress struct {
	SemanticProfile   string               `json:"semantic_profile,omitempty"`
	SemanticCheck     *RouteSemanticResult `json:"semantic_check,omitempty"`
	Proposal          string               `json:"proposal"`
	Evidence          string               `json:"evidence"`
	RepeatedBehavior  int                  `json:"repeated_behavior"`
	NoNewEvidence     int                  `json:"no_new_evidence"`
	SemanticRepeated  int                  `json:"semantic_repeated"`
	CorrectionVisible bool                 `json:"correction_visible"`
	Warning           string               `json:"warning,omitempty"`
	SemanticStatus    string               `json:"semantic_status"`
	SemanticError     string               `json:"semantic_error,omitempty"`
	Texts             []string             `json:"texts,omitempty"`
}

func advanceRouteProgress(old routeProgress, proposal, evidence string, correction, semanticRepeat bool) (routeProgress, bool) {
	next := old
	next.Warning = ""
	if old.Evidence != "" && old.Evidence == evidence {
		next.NoNewEvidence++
	} else {
		next.NoNewEvidence = 0
		next.RepeatedBehavior = 0
	}
	if old.Proposal != "" && old.Proposal == proposal && old.Evidence == evidence {
		next.RepeatedBehavior++
	} else {
		next.RepeatedBehavior = 0
	}
	if semanticRepeat {
		next.SemanticRepeated++
	} else {
		next.SemanticRepeated = 0
	}
	next.Proposal, next.Evidence = proposal, evidence
	next.CorrectionVisible = correction
	if next.RepeatedBehavior > 0 || semanticRepeat {
		next.Warning = "This decision is repeating without new delivery evidence. Read the exact saved failure and offered schema; identify the missing field/parameter/source capability, make a real supported correction, and validate it. Changing descriptions, IDs or rereading the same observation is not a correction. A changed business plan remains eligible for execution."
	}
	stop := old.CorrectionVisible && correction && next.RepeatedBehavior >= 2 && next.NoNewEvidence >= 2 && (next.SemanticRepeated >= 2 || next.NoNewEvidence >= 2)
	return next, stop
}

func routeEvidenceStamp(r Request) string {
	var state routeContext
	_ = json.Unmarshal([]byte(r.ContextJSON), &state)
	rows := []any{}
	for _, row := range state.Records {
		rows = append(rows, map[string]any{"fields": row.Fields, "source": row.Provenance.SourceID, "url": row.Provenance.URL, "version": row.Provenance.DataVersion})
	}
	refs := []any{}
	if state.Lookup != nil {
		for _, e := range state.Lookup.Evidence.Entries {
			refs = append(refs, []string{e.DocID, e.Revision, e.ChunkID})
		}
	}
	if state.AnswerEvidence != nil {
		for _, e := range state.AnswerEvidence.Entries {
			refs = append(refs, []string{e.DocID, e.Revision, e.ChunkID})
		}
	}
	var asset any
	if state.Asset != nil && state.Asset.ProofJSON != "" {
		asset = []string{state.Asset.ID, state.Asset.Version}
	}
	return agentdata.Hash(map[string]any{"records": rows, "published_asset": asset, "source_structure": state.Observation, "evidence": refs})
}

func routeBehavior(r Request, name string, message aiapp.Message, schema any) string {
	if len(message.ToolCalls) != 1 || message.ToolCalls[0].Function.Name != name {
		return agentdata.Hash([]string{name, "invalid_tool_envelope"})
	}
	var args map[string]any
	if json.Unmarshal([]byte(message.ToolCalls[0].Function.Arguments), &args) != nil {
		return agentdata.Hash([]string{name, "invalid_argument_json"})
	}
	var declared struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	_ = json.Unmarshal(mustMarshal(schema), &declared)
	for key := range args {
		if _, ok := declared.Properties[key]; !ok || key == "reason" || key == "description" || key == "purpose" {
			delete(args, key)
		}
	}
	var state routeContext
	_ = json.Unmarshal([]byte(r.ContextJSON), &state)
	if id, ok := args["asset_id"].(string); ok {
		resolved := false
		if state.Lookup != nil {
			for _, asset := range state.Lookup.Assets {
				if asset.ID == id {
					args["asset_id"] = []string{asset.SourceID, asset.Version}
					resolved = true
					break
				}
			}
		}
		if !resolved {
			args["asset_id"] = "unresolved_asset"
		}
	}
	if id, ok := args["source_operation_id"].(float64); ok {
		resolved := false
		if state.LuaFailure != nil {
			for _, diagnostic := range state.LuaFailure.Diagnostics {
				if diagnostic.OperationID == uint64(id) && diagnostic.Receipt != nil {
					args["source_operation_id"] = []any{diagnostic.Receipt.URL, diagnostic.Receipt.Status}
					resolved = true
					break
				}
			}
		}
		if !resolved {
			args["source_operation_id"] = "unresolved_source_operation"
		}
	}
	return agentdata.Hash([]any{name, args})
}

func cosineSimilarity(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, x, y float64
	for i := range a {
		aa, bb := float64(a[i]), float64(b[i])
		dot += aa * bb
		x += aa * aa
		y += bb * bb
	}
	if x == 0 || y == 0 {
		return 0
	}
	return dot / math.Sqrt(x*y)
}

func (s *Service) trackRouteProposal(ctx context.Context, r *Request, name string, schema any, message aiapp.Message, modelSteps ...uint64) error {
	var old routeProgress
	_ = json.Unmarshal([]byte(r.RouteProgressJSON), &old)
	text := aiapp.DecisionSemanticText(message)
	history := append([]string{}, old.Texts...)
	if old.SemanticProfile != "decision-intent-v1" {
		history = nil
	}
	if len(history) > 11 {
		history = history[len(history)-11:]
	}
	batch := append(history, text)
	var modelStep uint64
	if len(modelSteps) > 0 {
		modelStep = modelSteps[0]
	}
	check, err := s.routeSemanticCheck(ctx, *r, batch, modelStep)
	if err != nil {
		return err
	}
	next, stop := advanceRouteProgress(old, routeBehavior(*r, name, message, schema), routeEvidenceStamp(*r), r.FeedbackJSON != "", check.Repeated)
	next.Texts, next.SemanticStatus, next.SemanticError = batch, check.Status, check.Error
	next.SemanticProfile, next.SemanticCheck = "decision-intent-v1", &check
	r.RouteProgressJSON = string(mustMarshal(next))
	if stop {
		return agentstatus.Wrap(agentstatus.AgentExhausted, fmt.Errorf("routing repeated the same business decision after visible correction with %d no-evidence rounds", next.NoNewEvidence), next.Warning, "Preserve the model reasoning and exact tool receipts; this is Agent behavior, not an operator repair incident.")
	}
	return nil
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

func (s *Service) routeSemanticCheck(ctx context.Context, r Request, batch []string, modelStep uint64) (RouteSemanticResult, error) {
	result := RouteSemanticResult{Status: "not_configured", ModelStepID: modelStep, BatchSize: len(batch)}
	if s.Crawler == nil || s.Crawler.LoopEmbedding == nil {
		return result, nil
	}
	step, fresh, err := s.reserveStep(ctx, r, "route_semantic", map[string]any{"profile": "decision-intent-v1", "model_step_id": modelStep, "texts": batch})
	if err != nil {
		return result, err
	}
	if !fresh {
		if step.Stage == "reserved" {
			result.Status, result.Error = "outcome_unknown", "prior embedding outcome is not durably known"
			return result, nil
		}
		if err := json.Unmarshal([]byte(step.ResultJSON), &result); err != nil {
			return result, err
		}
		return result, nil
	}
	started := time.Now()
	var vectors [][]float32
	if measured, ok := s.Crawler.LoopEmbedding.(knowledge.MeasuredEmbedder); ok {
		measurement, cause := measured.EmbedMeasured(ctx, batch)
		vectors, err = measurement.Vectors, cause
		result.Model, result.RequestID, result.Usage = measurement.Model, measurement.RequestID, measurement.Usage
	} else {
		vectors, err = s.Crawler.LoopEmbedding.EmbedTexts(ctx, batch)
	}
	result.DurationMS = time.Since(started).Milliseconds()
	if err == nil && len(vectors) != len(batch) {
		err = fmt.Errorf("embedding batch count mismatch")
	}
	if err != nil {
		result.Status, result.Error = "unavailable", err.Error()
	} else {
		result.Status = "checked"
		for i := 0; i < len(vectors)-1; i++ {
			similarity := cosineSimilarity(vectors[i], vectors[len(vectors)-1])
			if similarity > result.Similarity {
				result.Similarity = similarity
			}
			result.Repeated = result.Repeated || similarity >= 0.92
		}
	}
	return result, s.finishStep(context.WithoutCancel(ctx), step, result, err)
}
