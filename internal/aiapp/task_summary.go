package aiapp

import (
	"context"
	"encoding/json"
	"fmt"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/knowledge"
	"strings"
)

type finishProposal struct {
	Status       string              `json:"status"`
	Reason       string              `json:"reason"`
	Summary      string              `json:"summary,omitempty"`
	Citations    []string            `json:"citations,omitempty"`
	Gaps         []string            `json:"gaps,omitempty"`
	Limitations  []string            `json:"limitations,omitempty"`
	Answer       *RAGAnswer          `json:"answer,omitempty"` // Host-bound receipt; never accepted as model input.
	Requirements []RequirementResult `json:"requirements,omitempty"`
}

func summaryRequired(r agentdata.Run) bool {
	var input CreateRunRequest
	_ = json.Unmarshal([]byte(r.InputJSON), &input)
	return input.RequireSummary
}

func (s *Service) finishProposal(ctx context.Context, r agentdata.Run, call ToolCall) (ToolResult, error) {
	var a struct {
		Status       string              `json:"status"`
		Reason       string              `json:"reason"`
		Summary      string              `json:"summary,omitempty"`
		Citations    []string            `json:"citations,omitempty"`
		Gaps         []string            `json:"gaps,omitempty"`
		Limitations  []string            `json:"limitations,omitempty"`
		Requirements []RequirementResult `json:"requirements,omitempty"`
	}
	if err := decodeArgs(call.Function.Arguments, &a); err != nil {
		return ToolResult{}, err
	}
	if a.Status != "completed" && a.Status != "partial" && a.Status != "failed" {
		return ToolResult{}, fmt.Errorf("invalid terminal proposal")
	}
	if a.Status == "completed" && len(a.Gaps) > 0 {
		return ToolResult{}, fmt.Errorf("completion has declared gaps; deliver partial or acquire missing evidence")
	}
	var spec TaskSpec
	if r.SpecJSON != "" {
		if err := json.Unmarshal([]byte(r.SpecJSON), &spec); err != nil {
			return ToolResult{}, err
		}
	}
	if err := validateRequirementDelivery(spec, a.Status, a.Requirements, a.Citations); err != nil {
		return ToolResult{}, err
	}
	out := finishProposal{Status: a.Status, Reason: a.Reason, Summary: a.Summary, Citations: a.Citations, Gaps: a.Gaps, Limitations: a.Limitations, Requirements: a.Requirements}
	// The frozen request owns delivery requirements. Optional model prose must
	// not promote a structured extraction into a RAG task. Keep that prose in
	// the receipt, but do not publish it as a grounded Answer. Explicit citations
	// still enter the evidence validator, including for optional summaries.
	analysisTask := spec.RequireSummary || summaryRequired(r) || len(spec.Requirements) > 0
	if strings.TrimSpace(a.Summary) == "" && len(a.Citations) == 0 && analysisTask {
		if a.Status == "completed" && (spec.RequireSummary || summaryRequired(r)) {
			return ToolResult{}, fmt.Errorf("this task requires a cited summary before completion")
		}
	}
	if len(a.Citations) == 0 && !analysisTask {
		return toolData(call, out)
	}
	set, err := s.runEvidence(ctx, r.ID)
	if err != nil {
		return ToolResult{}, err
	}
	answer := RAGAnswer{Status: "answered", Answer: a.Summary, Citations: a.Citations, Evidence: knowledge.EvidenceSet{RequestID: r.ID, Entries: []knowledge.Evidence{}}}
	answer.SupportLevel = "references_only_analysis_unverified"
	if a.Status != "completed" {
		answer.Status = "partial"
	}
	// Bind only cited evidence, after checking the same current-generation rule as RAG.
	reader := knowledge.SetReader{Store: s.Knowledge, Set: set}
	for _, id := range a.Citations {
		ev, err := reader.ReadEvidence(ctx, id)
		if err != nil {
			// A rejected citation is usually a plausible-looking id the model
			// constructed (a tool call id, a URL, a locator) rather than one the
			// host issued. Saying only "unknown" invites another guess; name the
			// ids this run actually returned so the next proposal can cite them.
			// The original error stays wrapped, so callers matching
			// knowledge.ErrInvalidReference still classify it correctly.
			return ToolResult{}, fmt.Errorf("%w: citation %q is not an evidence id from this run. Use an exact id beginning ev_ as returned in tool receipts and the evidence catalog. Available ids: %s", err, truncateRunes(id, 120), availableEvidenceIDs(set))
		}
		answer.Evidence.Entries = append(answer.Evidence.Entries, ev)
	}
	if err := validateRAGAnswer(ctx, s.Knowledge, answer); err != nil {
		return ToolResult{}, err
	}
	if spec.RequireExtractiveSummary && (a.Status == "completed" || strings.TrimSpace(a.Summary) != "") {
		if err := checkExtractiveSummary(&answer); err != nil {
			return ToolResult{}, err
		}
	}
	out.Answer = &answer
	return toolData(call, out)
}

// The durable finish receipt owns the summary, so lease/process recovery cannot lose it.
// Current views recheck references; Run.ResultJSON remains an immutable historical delivery.
func (s *Service) attachTaskSummary(ctx context.Context, r agentdata.Run, result *TaskResult, ops []agentdata.Operation) error {
	for n := len(ops) - 1; n >= 0; n-- {
		op := ops[n]
		if op.Kind != "finish_task" || op.Stage != "succeeded" {
			continue
		}
		var receipt ToolResult
		var finish finishProposal
		if err := json.Unmarshal([]byte(op.ResultJSON), &receipt); err != nil {
			return err
		}
		if err := json.Unmarshal(receipt.Data, &finish); err != nil {
			return err
		}
		if finish.Answer != nil {
			if err := validateRAGAnswer(ctx, s.Knowledge, *finish.Answer); err != nil {
				result.Gaps = append(result.Gaps, "summary evidence is no longer current: "+err.Error())
				return nil
			}
			result.Answer = finish.Answer
			result.Summary = finish.Answer.Answer
		}
		result.Gaps = append(result.Gaps, finish.Gaps...)
		result.Limitations = append(result.Limitations, finish.Limitations...)
		result.Requirements = finish.Requirements
		return nil
	}
	return nil
}

// These are model assessments backed by current evidence, not independent
// semantic truth. Host checks prevent silently dropping frozen requirements.
func validateRequirementDelivery(spec TaskSpec, status string, results []RequirementResult, citations []string) error {
	known := map[string]bool{}
	for _, r := range spec.Requirements {
		known[r.ID] = true
	}
	allowedEvidence := map[string]bool{}
	for _, id := range citations {
		allowedEvidence[id] = true
	}
	seen := map[string]bool{}
	for _, result := range results {
		if !known[result.ID] || seen[result.ID] || strings.TrimSpace(result.Explanation) == "" {
			return fmt.Errorf("requirement result must reference one frozen requirement with explanation")
		}
		seen[result.ID] = true
		if result.Status != "satisfied" && result.Status != "missing" {
			return fmt.Errorf("requirement status must be satisfied or missing")
		}
		if result.Status == "satisfied" && len(result.Citations) == 0 {
			return fmt.Errorf("satisfied requirement needs supporting evidence")
		}
		for _, id := range result.Citations {
			if !allowedEvidence[id] {
				return fmt.Errorf("requirement evidence must be included in delivery citations")
			}
		}
		if status == "completed" && result.Status != "satisfied" {
			return fmt.Errorf("completion has an unsatisfied requirement")
		}
	}
	if status == "completed" && len(seen) != len(known) {
		return fmt.Errorf("completion must assess every frozen requirement")
	}
	return nil
}

// availableEvidenceIDs lists the evidence ids this run may cite, bounded so the
// rejection message stays inside the observation budget. It exists because a
// model that cannot see the ids can only guess them.
func availableEvidenceIDs(set knowledge.EvidenceSet) string {
	if len(set.Entries) == 0 {
		return "(none yet; acquire a source before citing)"
	}
	ids := make([]string, 0, len(set.Entries))
	for _, e := range set.Entries {
		if e.ID == "" {
			continue
		}
		ids = append(ids, e.ID)
		if len(ids) >= 8 {
			break
		}
	}
	if len(ids) == 0 {
		return "(none yet; acquire a source before citing)"
	}
	out := strings.Join(ids, ", ")
	if len(set.Entries) > len(ids) {
		out += fmt.Sprintf(" (and %d more)", len(set.Entries)-len(ids))
	}
	return out
}
