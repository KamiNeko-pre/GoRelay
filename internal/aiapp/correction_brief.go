package aiapp

import (
	"encoding/json"
	"fmt"
	"go-lua-crawler/internal/agentdata"
	"sort"
	"strings"
)

// Correction brief.
//
// A correction that only says "change approach" is a mechanism, not information:
// the model already believed it was making progress, so it has no reason to
// change anything. Two things must arrive together with the correction:
//
//  1. the direction is wrong, stated plainly, with the evidence that shows it;
//  2. something the model does not already have — a concrete unmet field, a
//     saved page it never used, a failure whose fix it has not tried.
//
// This file builds that brief from the ledger. Everything in it is derived from
// persisted state, so it can never invent a fact, and it is bounded so a long
// run cannot turn the brief into a second copy of the history.

// correctionBrief is the structured payload injected when a run is corrected.
type correctionBrief struct {
	Phase string `json:"phase"`
	// Direction names what the model has been doing and why it cannot work.
	Direction string `json:"direction_is_wrong"`
	// Evidence is the observed fact that shows the direction is wrong.
	Evidence []string `json:"evidence"`
	// Unmet is what the frozen contract still requires.
	Unmet []string `json:"still_required"`
	// Unused is material already saved that no delivered record accounts for.
	Unused []string `json:"saved_but_unused,omitempty"`
	// Unfixed is the failing field and what the host expects, when known.
	Unfixed []string `json:"failing_and_expected,omitempty"`
	// Next is the concrete action to take this round.
	Next        string `json:"do_this_next"`
	Methodology []any  `json:"methodology"`
}

// buildCorrectionBrief assembles the brief for the current round.
func buildCorrectionBrief(state LoopState, phase, reason string) map[string]any {
	brief := correctionBrief{Phase: phase}

	// 1. The direction, named from what the ledger shows the run doing.
	brief.Direction, brief.Evidence = diagnoseDirection(state, reason)

	// 2. What the contract still requires, named by field so the model can act.
	brief.Unmet = unmetRequirements(state)

	// 3. Material already saved that no delivered record accounts for. This is
	// new information in the strict sense: the model may have lost track of what
	// it already fetched, and a saved page is enough to submit a record.
	brief.Unused = savedButUnused(state)

	// 4. The failing field and the expected shape, when the host knows it.
	brief.Unfixed = failingAndExpected(state)

	// 5. The next concrete action, chosen from the highest-value unmet item.
	brief.Next = nextActionFor(state, brief)
	brief.Methodology = crawlerRecoveryMethodology(state, phase)

	raw, err := json.Marshal(brief)
	if err != nil {
		return map[string]any{"phase": phase, "reason": reason}
	}
	var out map[string]any
	if json.Unmarshal(raw, &out) != nil {
		return map[string]any{"phase": phase, "reason": reason}
	}
	out["reason"] = reason
	r := state.HostProgress.Recovery
	out["loop_stop_condition"] = map[string]any{
		"semantic_repeat_rounds": r.SemanticRepeatRounds, "no_evidence_rounds": r.NoEvidenceRounds,
		"behavior_repeated": r.BehaviorRepeated, "new_evidence": r.NewEvidence,
		"required_consecutive_rounds": deliveryIdleRoundLimit,
		"rule":                        "After correction is visible, conclude for looping only when two consecutive semantic repeats OR two consecutive rounds without new evidence coincide with unchanged looping behavior and no new evidence. New observed source structure or a new actionable diagnosis permits applying that feedback; it does not count as delivered records. The task deadline remains independent.",
	}
	return out
}

// diagnoseDirection states plainly what the run is doing that cannot work, with
// the observation that proves it.
func diagnoseDirection(state LoopState, reason string) (string, []string) {
	evidence := []string{}

	// Repeating a call whose result cannot change.
	if repeat := repeatedToolRejection(state); repeat != nil {
		tool, _ := repeat["tool"].(string)
		count := intField(repeat, "rejections")
		evidence = append(evidence, fmt.Sprintf("%s was refused %d times with the same diagnosed defect", tool, count))
		if args, _ := repeat["arguments"].(string); args != "" {
			evidence = append(evidence, "the refused arguments were: "+truncateRunes(args, 200))
		}
		return "Re-sending the same rejected call. The host has already diagnosed it; repeating it cannot produce a different answer.", evidence
	}

	// Re-reading material already read.
	if repeat := repeatedReadInformation(state.Operations); repeat != nil {
		if pages := intField(repeat, "repeated"); pages > 0 {
			evidence = append(evidence, fmt.Sprintf("%d read(s) returned content already in the ledger", pages))
		}
		return "Re-reading information the run already holds without advancing accepted records or a useful capability.", evidence
	}

	// An unchanged call/observation cycle.
	if period, count := repeatedObservationCycle(state.Operations); count >= 2 {
		evidence = append(evidence, fmt.Sprintf("a %d-step call pattern repeated %d times with unchanged results", period, count))
		return "Repeating an unchanged cycle of calls. Every iteration returns what the previous one returned.", evidence
	}
	if probe := latestProbeRepair(state); probe != nil {
		return "The successful probe is incomplete. Repair the missing fields against its observed source structures, then validate acquisition before publication and submission.", []string{fmt.Sprintf("probe %s has row-level missing-field diagnostics; it produced no accepted records", probe["probe_call_id"])}
	}

	// Rounds with no material, no records and no capability.
	if state.HostProgress.NoProgressModelRounds > 0 {
		evidence = append(evidence, fmt.Sprintf("%d consecutive rounds produced no accepted records, useful validated or published capability, or supported delivery fields; successful reads do not reset this count", state.HostProgress.NoProgressModelRounds))
	}
	if pages := acquiredPageCount(state); pages > 0 && deliveredRecordCount(state) == 0 {
		evidence = append(evidence, fmt.Sprintf("%d page(s) were fetched successfully but no record has been submitted", pages))
		return "Inspecting and reading without ever submitting. Material that is not submitted cannot satisfy the contract.", evidence
	}
	if len(evidence) == 0 {
		evidence = append(evidence, reason)
	}
	return "The run is not producing information the contract can use.", evidence
}

// unmetRequirements names each frozen requirement that is not yet satisfied.
func unmetRequirements(state LoopState) []string {
	out := []string{}
	if state.Spec == nil {
		if state.Result == nil || len(state.Result.Records) == 0 {
			out = append(out, "no record has been submitted")
		}
		return out
	}
	records := deliveredRecordCount(state)
	if state.Spec.MinRecords > 0 && records < state.Spec.MinRecords {
		out = append(out, fmt.Sprintf("%d record(s) required, %d submitted", state.Spec.MinRecords, records))
	} else if state.Spec.MinRecords == 0 && records == 0 {
		out = append(out, "at least one record is required")
	}
	// Which required fields are missing across delivered records.
	missing := missingRequiredFields(state)
	if len(missing) > 0 {
		out = append(out, "required fields with no supported value yet: "+strings.Join(missing, ", "))
	}
	if state.Spec.RequireSummary && (state.Result == nil || state.Result.Answer == nil || state.Result.Answer.Status != "answered") {
		out = append(out, "a cited summary answering the goal is required and has not been delivered")
	}
	if state.Spec.RequirePublishedCapability && (state.Result == nil || state.Result.ReusableCapabilities == 0) {
		out = append(out, "a published, reusable acquisition capability is required and none is registered")
	}
	if len(out) == 0 && state.Result != nil && !state.Result.Coverage.Satisfied {
		out = append(out, "coverage is not satisfied")
	}
	return out
}

// missingRequiredFields lists required fields absent from every delivered record.
func missingRequiredFields(state LoopState) []string {
	if state.Spec == nil {
		return nil
	}
	present := map[string]bool{}
	if state.Result != nil {
		for _, rec := range state.Result.Records {
			for name, value := range rec.Fields {
				if strings.TrimSpace(fmt.Sprint(value)) != "" {
					present[name] = true
				}
			}
		}
	}
	out := []string{}
	for _, req := range state.Spec.RequiredFields {
		if !present[req.Name] {
			out = append(out, req.Name)
		}
	}
	sort.Strings(out)
	return out
}

// savedButUnused names pages the run fetched successfully that no delivered
// record cites. The model may have lost track of them; they are enough to
// submit a record without any new fetch.
func savedButUnused(state LoopState) []string {
	delivered := map[string]bool{}
	if state.Result != nil {
		for _, rec := range state.Result.Records {
			for _, src := range rec.Sources {
				if src.URL != "" {
					delivered[src.URL] = true
				}
			}
		}
	}
	out := []string{}
	seen := map[string]bool{}
	for _, op := range state.Operations {
		if op.Kind != "inspect_source" || op.Stage != "succeeded" {
			continue
		}
		var res ToolResult
		if json.Unmarshal([]byte(op.ResultJSON), &res) != nil {
			continue
		}
		var page sourceInspection
		if json.Unmarshal(res.Data, &page) != nil {
			continue
		}
		url := page.Source.Receipt.URL
		if url == "" || delivered[url] || seen[url] {
			continue
		}
		if page.Source.Receipt.Status < 200 || page.Source.Receipt.Status >= 300 || page.Source.Body == "" {
			continue
		}
		seen[url] = true
		out = append(out, fmt.Sprintf("%s (inspection handle=%s; inspect saved selectors with read_data view=dom, then extract through field locators or Lua)", url, res.CallID))
		if len(out) >= 5 {
			break
		}
	}
	return out
}

// failingAndExpected describes the failing field and what the host expects,
// using the structured failure contract rather than the wording.
func failingAndExpected(state LoopState) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, res := range unresolvedToolFailures(state) {
		if res.Failure == nil || res.ReplayOf != "" || len(out) >= 4 {
			continue
		}
		f := res.Failure
		field := failureField(f)
		key := res.Name + "|" + f.Code + "|" + field
		if seen[key] {
			continue
		}
		seen[key] = true
		line := fmt.Sprintf("%s failed with %s", res.Name, f.Code)
		var batch struct {
			Fields      []string `json:"fields"`
			Unsupported []struct {
				Field string `json:"field"`
			} `json:"unsupported_fields"`
		}
		detail, _ := json.Marshal(f.Details)
		json.Unmarshal(detail, &batch)
		if len(batch.Fields) == 0 {
			unique := map[string]bool{}
			for _, issue := range batch.Unsupported {
				if issue.Field != "" && !unique[issue.Field] {
					batch.Fields = append(batch.Fields, issue.Field)
					unique[issue.Field] = true
				}
			}
			sort.Strings(batch.Fields)
		}
		if len(batch.Fields) > 0 {
			line += " on fields " + strings.Join(batch.Fields, ", ")
		} else if field != "" {
			line += fmt.Sprintf(" on field %q", field)
		}
		if f.Recovery != nil && f.Recovery.Instruction != "" {
			line += "; host guidance: " + truncateRunes(f.Recovery.Instruction, 160)
		} else if f.Message != "" {
			line += "; reported: " + truncateRunes(f.Message, 160)
		}
		out = append(out, line)
	}
	return out
}

// nextActionFor picks the single highest-value action for this round, in the
// order that unblocks the contract fastest.
func nextActionFor(state LoopState, brief correctionBrief) string {
	if brief.Phase == "concluding" {
		return "Call finish_task now with supported results and explicit gaps. Do not start more acquisition or inspection."
	}
	if len(brief.Unfixed) > 0 {
		return "Follow the unresolved diagnostic's recovery strategy below. Change arguments or code only when that diagnostic identifies a correctable defect; dependency or policy failures require their own recovery or honest partial/failed delivery."
	}
	if latestProbeRepair(state) != nil {
		return "Apply the saved probe's field_diagnostics to the named missing fields and observed layouts. Preserve working navigation, add field evidence, validate mode=acquisition, and follow the returned publication/submission steps."
	}
	if len(brief.Unused) > 0 {
		return "Submit a record from a page already saved (listed above) using submit_extraction, or validate_program and register_source for a reusable capability, before fetching anything new."
	}
	if len(brief.Unmet) > 0 {
		return "Address the unmet requirement named above with one concrete tool call that changes the observed state."
	}
	return "Deliver what is supported now with finish_task, naming the remaining gaps explicitly."
}

// Reuse exact host-produced continuations rather than inventing selectors,
// identifiers or values. A plan is not new evidence and does not renew progress.
func crawlerRecoveryMethodology(state LoopState, phase string) []any {
	if phase == "concluding" {
		return []any{map[string]any{"step": 1, "tool": "finish_task", "instruction": "Report supported delivery and remaining gaps; use completed only if frozen coverage is satisfied."}}
	}
	steps := []any{}
	for _, out := range unresolvedToolFailures(state) {
		if out.ReplayOf == "" && out.Failure != nil && out.Failure.Recovery != nil {
			steps = append(steps, map[string]any{"step": 1, "failed_call_id": out.CallID, "cause": out.Failure.Message, "recovery": out.Failure.Recovery})
			break
		}
	}
	summary := crawlerStateSummary(state)
	if probe := latestProbeRepair(state); probe != nil {
		steps = append(steps, map[string]any{"step": len(steps) + 1, "probe_repair": probe, "instruction": "Use the concrete missing-field/source-structure feedback to repair the saved probe. A successful probe is preparation, never a publishable or delivered candidate."})
	}
	steps = append(steps, map[string]any{"step": len(steps) + 1, "saved_work": summary, "instruction": "Reuse a candidate only for its supported purpose: composition is temporary and cannot satisfy required publication. For a reusable capability use kind=adapter, mode=acquisition, register_source, then submit_acquisition. Do not repeat identical validation arguments; a diagnosed kind or schema mismatch permits reusing the same code with corrected arguments. If already published, use the published source instead of registering again."})
	steps = append(steps, map[string]any{"step": len(steps) + 1, "instruction": "If no usable candidate exists: use a saved inspection handle and observed row links to inspect one representative detail; map missing fields to DOM selectors or JSON paths. Read only missing structure, never article text. Generate Lua that follows observed pagination and detail links, deduplicates stable record keys, and extracts required fields with their real locators. Validate mode=acquisition, follow returned publication/submission steps, then check child results and finish. If the source or available tools cannot supply a field, state that gap instead of repeating reads."})
	return steps
}

func acquiredPageCount(state LoopState) int {
	n := 0
	for _, op := range state.Operations {
		if op.Kind == "inspect_source" && op.Stage == "succeeded" {
			n++
		}
	}
	return n
}

func deliveredRecordCount(state LoopState) int {
	if state.Result == nil {
		return 0
	}
	return len(state.Result.Records)
}

func intField(m map[string]any, key string) int {
	switch v := m[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}

// ensure agentdata stays referenced for the ledger types used above.
var _ = agentdata.Operation{}
