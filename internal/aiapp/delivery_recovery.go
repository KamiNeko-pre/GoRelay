package aiapp

import (
	"context"
	"encoding/json"
	"go-lua-crawler/internal/agentdata"
	"sort"
	"time"
)

type DeliveryRecovery struct {
	Phase    string `json:"phase,omitempty"`
	Attempts int    `json:"attempts"`
	// Legacy fields remain decodable for saved runs; neither is a loop stop signal.
	IdleModels           int64  `json:"idle_models,omitempty"`
	LastModel            uint64 `json:"last_model,omitempty"`
	Baseline             string `json:"baseline,omitempty"`
	Reason               string `json:"reason,omitempty"`
	ReadBaseline         string `json:"read_baseline,omitempty"`
	SemanticRepeatRounds int64  `json:"semantic_repeat_rounds"`
	NoEvidenceRounds     int64  `json:"no_evidence_rounds"`
	BehaviorRepeated     bool   `json:"behavior_repeated"`
	NewEvidence          bool   `json:"new_evidence"`
}

// One convergence owner replaces the V5 exact-cycle stop and 20/40/80 review
// paths. Only a model request that actually saw correction feedback consumes
// a correction attempt, and only after all of its tool replies exist.
//
// Only consecutive signals from settled decisions that actually saw correction
// count. They authorize conclusion only with unchanged behavior and no new
// evidence; an absence of delivered records alone never authorizes it.
const deliveryIdleRoundLimit = 2

func advanceDeliveryRecovery(prior DeliveryRecovery, state LoopState, host HostProgress, semanticIdle bool, semanticMatch string) DeliveryRecovery {
	if state.Budget.LoopVersion < 5 {
		return prior
	}
	var last agentdata.Operation
	var response ChatResponse
	for _, op := range state.Operations {
		if op.Kind == "model" && op.Stage != "reserved" {
			last = op
			_ = json.Unmarshal([]byte(op.ResultJSON), &response)
		}
	}
	settled := true
	for _, call := range response.Message.ToolCalls {
		found := false
		for _, op := range state.Operations {
			if op.Key == "tool:"+call.ID && (op.Stage == "failed" || op.Stage == "succeeded") {
				found = true
				break
			}
		}
		if !found {
			settled = false
			break
		}
	}
	if prior.Phase == "closed" {
		return prior
	}
	// Reserve correction and conclusion inside the existing call ceiling. A
	// default 12-call window cannot wait for the 20-round stagnation threshold.
	// Timeboxed unlimited experiments still use evidence/semantic liveness.
	if settled && last.ID != 0 && !state.Budget.Unlimited && state.Budget.PromptPolicy == nil && host.NoProgressModelRounds > 0 {
		remaining := state.Budget.Limits.ModelCalls - state.Budget.workSpent().ModelCalls
		if remaining <= 1 && prior.Phase != "concluding" {
			prior.Phase, prior.LastModel, prior.Reason = "concluding", last.ID, "one model call remains in the existing work window; conclude from saved results"
			return prior
		}
		if prior.Phase == "" && remaining <= deliveryIdleRoundLimit+1 {
			return DeliveryRecovery{Phase: "correcting", LastModel: last.ID, Baseline: deliveryInformationSignature(state), ReadBaseline: agentdata.Hash(consumedReadRanges(state)), Reason: "the existing work window is near exhaustion without substantive output; use the reserved correction calls"}
		}
	}
	if prior.Phase != "concluding" && !state.Budget.WorkDeadline.IsZero() && !time.Now().Before(state.Budget.WorkDeadline) {
		prior.Phase = "concluding"
		prior.LastModel = last.ID
		prior.Reason = "work deadline reached; reserve one bounded conclusion round"
		return prior
	}
	if prior.Phase == "concluding" {
		if settled && last.ID > prior.LastModel && response.DeliveryRecoveryPhase == "concluding" {
			prior.Phase = "closed"
			prior.LastModel = last.ID
		}
		return prior
	}
	signature := deliveryInformationSignature(state)
	if prior.Phase == "correcting" {
		if !settled {
			return prior
		}
		// A round that changed the delivery signature produced new material, so
		// it is progress and the run returns to normal exploration. This is the
		// "new valid information restores exploration" rule: the correction
		// window is defined by information, not by a fixed count of tries.
		if signature != prior.Baseline {
			return DeliveryRecovery{}
		}
		if last.Stage == "succeeded" && last.ID > prior.LastModel && response.DeliveryRecoveryPhase == "correcting" {
			prior.LastModel = last.ID
			prior.Attempts++
			prior.NewEvidence = newCorrectionEvidence(state, last.ID)
			if prior.NewEvidence {
				prior.NoEvidenceRounds = 0
			} else {
				prior.NoEvidenceRounds++
			}
			if semanticIdle {
				prior.SemanticRepeatRounds++
			} else {
				// Missing/failed embedding checks cannot extend a repeat streak.
				prior.SemanticRepeatRounds = 0
			}
			prior.BehaviorRepeated = repeatedCorrectionBehavior(state, last.ID)
			if (prior.SemanticRepeatRounds >= deliveryIdleRoundLimit || prior.NoEvidenceRounds >= deliveryIdleRoundLimit) && prior.BehaviorRepeated && !prior.NewEvidence {
				prior.Phase = "concluding"
				prior.Reason = "after visible correction, consecutive semantic repetition or absent evidence persisted with unchanged behavior and no new evidence"
			}
		}
		return prior
	}
	reason := ""
	if repeatedReadInformation(state.Operations) != nil {
		reason = "reads returned already observed information despite changed paging/batches"
	}
	if _, n := repeatedObservationCycle(state.Operations); n >= 2 {
		reason = "the same observation cycle repeated"
	}
	if repeatedToolRejection(state) != nil {
		reason = "a tool proposal repeated the same diagnosed failure"
	}
	if semanticIdle {
		// A reworded repeat: different calls, same meaning. The structural checks
		// above cannot see this, which is exactly why it stalled the run.
		reason = "this round's feedback means the same as " + semanticMatch
	}
	if host.NoProgressModelRounds >= hostProgressLimit(state.Budget) {
		reason = "no accepted records, supported delivery fields or useful capability advanced the frozen task"
	}
	if settled && reason != "" {
		prior.Reason = reason
		return DeliveryRecovery{Phase: "correcting", Baseline: signature, ReadBaseline: agentdata.Hash(consumedReadRanges(state)), LastModel: last.ID, Reason: reason}
	}
	return prior
}

// V5 uses substantive output only. The remainder is legacy preparation
// accounting for old persisted runs; it does not control new crawler delivery.
//
// Two sources of new information are folded in, because a round can advance the
// task either by acquiring material or by receiving a *different diagnosis*:
//
//   - material: page bodies, read slices, evidence. Re-reading the same bytes is
//     not progress.
//   - feedback: normalised tool-failure and tool-success identities. A failure
//     whose code and failing field are unchanged is the same information even if
//     the provider reworded the message, so it cannot renew a stalled run; a
//     failure that names a different field, or a success that returned different
//     content, is genuinely new.
//
// This is legacy accounting. V5 checks semantic novelty every settled round
// separately from accepted delivery and correction evidence.
func deliveryInformationSignature(state LoopState) string {
	if state.Budget.LoopVersion >= 5 {
		return hostProgressSignature(state, false)
	}
	material := map[string]bool{}
	for _, op := range state.Operations {
		if op.Stage != "succeeded" {
			continue
		}
		var out ToolResult
		if json.Unmarshal([]byte(op.ResultJSON), &out) != nil || out.Error != "" {
			continue
		}
		if op.Kind == "inspect_source" {
			var page sourceInspection
			if json.Unmarshal(out.Data, &page) == nil && page.Source.Receipt.Status >= 200 && page.Source.Receipt.Status < 300 && page.Source.Body != "" {
				material[agentdata.Hash([]any{page.Source.Receipt.URL, page.Source.Body})] = true
			}
		}
	}
	keys := make([]string, 0, len(material))
	for key := range material {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	// Feedback identities participate in the signature so that a round delivering
	// a new diagnosis counts as new information, while a reworded repeat of the
	// same diagnosis does not.
	return agentdata.Hash([]any{hostProgressSignature(state, false), keys, consumedReadRanges(state), feedbackIdentities(state.Operations)})
}

func lastSettledModel(state LoopState) (agentdata.Operation, bool) {
	var last agentdata.Operation
	var response ChatResponse
	for _, op := range state.Operations {
		if op.Kind == "model" && op.Stage != "reserved" {
			last = op
			json.Unmarshal([]byte(op.ResultJSON), &response)
		}
	}
	if last.ID == 0 {
		return last, false
	}
	for _, call := range response.Message.ToolCalls {
		found := false
		for _, op := range state.Operations {
			if op.Key == "tool:"+call.ID && (op.Stage == "succeeded" || op.Stage == "failed") {
				found = true
				break
			}
		}
		if !found {
			return last, false
		}
	}
	return last, true
}

func conclusionToolAllowed(name string) bool {
	switch name {
	case "read_data", "read_observation", "read_evidence", "read_task_context", "get_task_results", "evaluate_result", "finish_task":
		return true
	}
	return false
}

// Outside conclusion, a slow source/model cannot consume the reserved final
// minute. Receipt persistence still uses the Run's final deadline.
func deliveryRequestContext(ctx context.Context, r agentdata.Run) (context.Context, context.CancelFunc) {
	var budget BudgetSnapshot
	var host HostProgress
	_ = json.Unmarshal([]byte(r.BudgetJSON), &budget)
	_ = json.Unmarshal([]byte(r.HostProgressJSON), &host)
	deadline := r.Deadline
	if host.Recovery.Phase != "concluding" && host.Recovery.Phase != "closed" && !budget.WorkDeadline.IsZero() && (deadline.IsZero() || budget.WorkDeadline.Before(deadline)) {
		deadline = budget.WorkDeadline
	}
	if deadline.IsZero() {
		return context.WithCancel(ctx)
	}
	return context.WithDeadline(ctx, deadline)
}
