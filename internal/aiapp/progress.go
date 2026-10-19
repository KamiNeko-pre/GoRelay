package aiapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/knowledge"
)

// These are model assessments, not host-verified semantic facts. The host owns
// requirement identity and current evidence validity; original receipts own text.
type TaskProgress struct {
	Version           int                 `json:"version"`
	Requirements      []RequirementResult `json:"requirements"`
	Notes             []string            `json:"notes"`
	NextPurpose       string              `json:"next_purpose"`
	ExploredPaths     []ExploredPath      `json:"explored_paths,omitempty"`
	ObservedThrough   uint64              `json:"observed_through,omitempty"`
	ModelRounds       int                 `json:"model_rounds,omitempty"`
	FactsSignature    string              `json:"facts_signature,omitempty"`
	DeliverySignature string              `json:"delivery_signature,omitempty"`
	ReadSignature     string              `json:"read_signature,omitempty"`
	StalledReviews    int                 `json:"stalled_reviews,omitempty"`
}

type ExploredPath struct {
	Method  string `json:"method"`
	Target  string `json:"target"`
	Outcome string `json:"outcome"`
	Reason  string `json:"reason"`
}

func progressOf(r agentdata.Run) (TaskProgress, error) {
	var p TaskProgress
	if r.ProgressJSON != "" {
		if err := json.Unmarshal([]byte(r.ProgressJSON), &p); err != nil {
			return p, err
		}
	}
	return p, nil
}

func (s *Service) updateProgress(ctx context.Context, r agentdata.Run, call ToolCall) (ToolResult, error) {
	var a struct {
		ExpectedVersion *int                `json:"expected_version,omitempty"`
		Requirements    []RequirementResult `json:"requirements"`
		Notes           []string            `json:"notes"`
		NextPurpose     string              `json:"next_purpose"`
		ExploredPaths   []ExploredPath      `json:"explored_paths,omitempty"`
	}
	if err := decodeArgs(call.Function.Arguments, &a); err != nil {
		return ToolResult{}, err
	}
	p, err := progressOf(r)
	if err != nil {
		return ToolResult{}, err
	}
	if a.ExpectedVersion != nil && *a.ExpectedVersion != p.Version {
		return ToolResult{}, fmt.Errorf("progress version conflict: current version is %d", p.Version)
	}
	spec, err := specOf(r)
	if err != nil {
		return ToolResult{}, err
	}
	ids := []string{}
	for _, req := range a.Requirements {
		ids = append(ids, req.Citations...)
	}
	if err = validateRequirementDelivery(spec, "partial", a.Requirements, ids); err != nil {
		return ToolResult{}, err
	}
	provided := map[string]RequirementResult{}
	for _, req := range a.Requirements {
		provided[req.ID] = req
	}
	assessments := make([]RequirementResult, 0, len(spec.Requirements))
	for _, req := range spec.Requirements {
		assessment, ok := provided[req.ID]
		if !ok {
			assessment = RequirementResult{ID: req.ID, Status: "missing", Explanation: "not yet assessed", Citations: []string{}}
		}
		assessments = append(assessments, assessment)
	}
	a.Requirements = assessments
	set, err := s.runEvidence(ctx, r.ID)
	if err != nil {
		return ToolResult{}, err
	}
	if err = knowledge.CheckReferences(ctx, knowledge.SetReader{Store: s.Knowledge, Set: set}, ids); err != nil {
		return ToolResult{}, err
	}
	previous := p
	p = TaskProgress{Version: p.Version + 1, Requirements: a.Requirements, Notes: a.Notes, NextPurpose: a.NextPurpose, ExploredPaths: a.ExploredPaths}
	state, err := s.state(ctx, r)
	if err != nil {
		return ToolResult{}, err
	}
	p.ModelRounds = successfulModelRounds(state.Operations)
	p.FactsSignature = workingFactsSignature(state)
	p.DeliverySignature = deliveryProgressSignature(state, p)
	p.ReadSignature = observedReadSignature(state.Operations)
	if previous.Version > 0 && previous.DeliverySignature == p.DeliverySignature {
		p.StalledReviews = previous.StalledReviews
		if p.ModelRounds > previous.ModelRounds && previous.ReadSignature == p.ReadSignature {
			p.StalledReviews++
		}
	}
	for _, op := range state.Operations {
		if (op.Stage == "succeeded" || op.Stage == "failed") && op.ID > p.ObservedThrough {
			p.ObservedThrough = op.ID
		}
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return ToolResult{}, err
	}
	if len(raw) > 12<<10 {
		return ToolResult{}, fmt.Errorf("progress is a concise checkpoint, limited to 12 KiB; keep source text in evidence receipts")
	}
	q := s.Data.DB.WithContext(ctx).Model(&agentdata.Run{}).Where("id = ? AND token = ? AND (progress_json = ? OR (progress_json IS NULL AND ? = ''))", r.ID, r.Token, r.ProgressJSON, r.ProgressJSON).Update("progress_json", string(raw))
	if q.Error != nil {
		return ToolResult{}, q.Error
	}
	if q.RowsAffected != 1 {
		return ToolResult{}, agentdata.ErrConflict
	}
	return toolData(call, p)
}

func (s *Service) currentProgress(ctx context.Context, r agentdata.Run) (TaskProgress, error) {
	p, err := progressOf(r)
	if err != nil {
		return p, err
	}
	if len(p.Requirements) == 0 {
		return p, nil
	}
	set, err := s.runEvidence(ctx, r.ID)
	if err != nil {
		return p, err
	}
	reader := knowledge.SetReader{Store: s.Knowledge, Set: set}
	for n := range p.Requirements {
		req := &p.Requirements[n]
		if err := knowledge.CheckReferences(ctx, reader, req.Citations); err != nil {
			if !errors.Is(err, knowledge.ErrInvalidReference) {
				return p, err
			}
			req.Status = "missing"
			req.Explanation = "Previously recorded support is no longer current; reassess this requirement. " + req.Explanation
			req.Citations = nil
		}
	}
	return p, nil
}

// Body-free inventory keeps references discoverable after context compaction.
func evidenceCatalog(set knowledge.EvidenceSet) []map[string]any {
	entries := []map[string]any{}
	seen := map[string]bool{}
	for _, e := range set.Entries {
		if seen[e.ID] {
			continue
		}
		seen[e.ID] = true
		entries = append(entries, map[string]any{"id": e.ID, "doc_id": e.DocID, "revision": e.Revision, "generation": e.Generation, "chunk_id": e.ChunkID, "title": e.Title, "url": e.URL, "record_key": e.RecordKey, "read_with": map[string]string{"evidence_id": e.ID}})
	}
	return entries
}
