package aiapp

import (
	"encoding/json"
	"go-lua-crawler/internal/agentdata"
	"sort"
)

type TaskReview struct {
	Required         bool   `json:"required"`
	Reason           string `json:"reason,omitempty"`
	UnassessedRounds int    `json:"unassessed_rounds"`
	FactsChanged     bool   `json:"facts_changed"`
	StalledReviews   int    `json:"stalled_reviews"`
	RecoveryCycle    bool   `json:"recovery_cycle"`
	DeliveryRequired bool   `json:"delivery_required"`
}

// Newly consumed persisted pages may be necessary before an assessment can
// improve. They pause the stall counter, never reset it or satisfy a requirement.
// Stable source identity/content deduplicate repeated reads with new call IDs.
func observedReadSignature(ops []agentdata.Operation) string {
	reads := []string{}
	seen := map[string]bool{}
	for _, op := range ops {
		if op.Stage != "succeeded" || (op.Kind != "read_observation" && op.Kind != "read_source_content" && op.Kind != "read_evidence") {
			continue
		}
		var receipt ToolResult
		var data map[string]any
		if json.Unmarshal([]byte(op.ResultJSON), &receipt) != nil || json.Unmarshal(receipt.Data, &data) != nil {
			continue
		}
		text, ok := data["text"].(string)
		if !ok || text == "" {
			continue
		}
		identity := map[string]any{"text": text}
		for _, key := range []string{"receipt_tool_call_id", "inspection_call_id", "pointer", "doc_id", "revision", "generation", "chunk_id"} {
			if value, ok := data[key]; ok {
				identity[key] = value
			}
		}
		key := agentdata.Hash(identity)
		if !seen[key] {
			reads = append(reads, key)
			seen[key] = true
		}
	}
	sort.Strings(reads)
	return agentdata.Hash(reads)
}

// Failures require assessment, but do not by themselves improve delivery.
// Successful code capabilities and supported requirement changes do count.
func deliveryProgressSignature(state LoopState, p TaskProgress) string {
	material := state
	material.Operations = nil
	material.TaskResults = nil
	for _, child := range state.TaskResults {
		if child.Task.Status == "succeeded" {
			material.TaskResults = append(material.TaskResults, child)
		}
	}
	capabilities := []string{}
	seen := map[string]bool{}
	for _, op := range state.Operations {
		if op.Stage != "succeeded" || (op.Kind != "validate_program" && op.Kind != "register_source") {
			continue
		}
		var receipt ToolResult
		var artifact struct {
			Hash string `json:"artifact_hash"`
		}
		if json.Unmarshal([]byte(op.ResultJSON), &receipt) == nil && json.Unmarshal(receipt.Data, &artifact) == nil && artifact.Hash != "" && !seen[artifact.Hash] {
			capabilities = append(capabilities, artifact.Hash)
			seen[artifact.Hash] = true
		}
	}
	sort.Strings(capabilities)
	assessments := []map[string]any{}
	for _, req := range p.Requirements {
		citations := []string{}
		seenCitations := map[string]bool{}
		for _, id := range req.Citations {
			if !seenCitations[id] {
				citations = append(citations, id)
				seenCitations[id] = true
			}
		}
		sort.Strings(citations)
		assessments = append(assessments, map[string]any{"id": req.ID, "status": req.Status, "citations": citations})
	}
	sort.Slice(assessments, func(i, j int) bool { return assessments[i]["id"].(string) < assessments[j]["id"].(string) })
	return agentdata.Hash(map[string]any{"facts": workingFactsSignature(material), "capabilities": capabilities, "requirements": assessments})
}

func successfulModelRounds(ops []agentdata.Operation) int {
	n := 0
	for _, op := range ops {
		if op.Kind == "model" && op.Stage == "succeeded" {
			n++
		}
	}
	return n
}

// Execution outcomes and published evidence belong to the host. Reads, query
// spelling, bookkeeping timestamps and checkpoint notes do not change them.
func workingFactsSignature(state LoopState) string {
	children := []map[string]any{}
	for _, child := range state.TaskResults {
		if child.Task.Status.IsTerminal() {
			children = append(children, map[string]any{"task_id": child.Task.ID, "status": child.Task.Status, "failure_code": child.Task.FailureCode, "records": child.Records})
		}
	}
	sort.Slice(children, func(i, j int) bool { return children[i]["task_id"].(uint64) < children[j]["task_id"].(uint64) })
	evidence := []string{}
	for _, e := range state.EvidenceCatalog {
		evidence = append(evidence, agentdata.Hash(e))
	}
	sort.Strings(evidence)
	outcomes := []map[string]any{}
	seen := map[string]bool{}
	for _, op := range state.Operations {
		// Source/trial failures must be digested before expanding acquisition.
		if op.Stage != "failed" || (op.Kind != "discover_sources" && op.Kind != "inspect_source" && op.Kind != "validate_program") {
			continue
		}
		var receipt ToolResult
		var rejection struct {
			Review *TaskReview `json:"task_review"`
		}
		if json.Unmarshal([]byte(op.ResultJSON), &receipt) == nil && json.Unmarshal(receipt.Data, &rejection) == nil && rejection.Review != nil {
			continue // A host rejection never reached a source/provider.
		}
		if receipt.Failure != nil && (receipt.Failure.Kind == "cooldown" || receipt.Failure.Kind == "batch_stopped") {
			continue
		} // No source/provider attempt was dispatched.
		key := op.Kind + ":" + op.InputHash
		if !seen[key] {
			outcomes = append(outcomes, map[string]any{"kind": op.Kind, "input": op.InputHash})
			seen[key] = true
		}
	}
	sort.Slice(outcomes, func(i, j int) bool { return agentdata.Hash(outcomes[i]) < agentdata.Hash(outcomes[j]) })
	var records any
	if state.Result != nil && (len(state.Result.Records) > 0 || len(state.Result.Conflicts) > 0 || len(state.Result.ReuseIssues) > 0) {
		records = map[string]any{"records": state.Result.Records, "conflicts": state.Result.Conflicts, "reuse_issues": state.Result.ReuseIssues}
	}
	return agentdata.Hash(map[string]any{"children": children, "evidence": evidence, "outcomes": outcomes, "result": records})
}

func workingProgress(state LoopState) TaskProgress {
	p := state.Progress
	if p.Version == 0 && state.Run.ProgressJSON != "" {
		_ = json.Unmarshal([]byte(state.Run.ProgressJSON), &p)
	}
	return p
}

func taskReview(state LoopState) TaskReview {
	if state.Budget.LoopVersion >= 4 {
		if state.HostProgress.Recovery.Phase == "concluding" || state.HostProgress.Recovery.Phase == "closed" {
			return TaskReview{Required: true, DeliveryRequired: true, Reason: "bounded recovery exhausted; only read saved evidence and deliver supported results with gaps"}
		}
		if failures := unresolvedToolFailures(state); len(failures) > 0 {
			return TaskReview{Required: true, Reason: "unresolved tool diagnostics are available; correct the reported cause using the persisted failure receipt"}
		}
		// Work-window enforcement and host progress replace the former total
		// call countdown. A necessary acquisition must not be reserved out.
		return TaskReview{}
	}
	if state.Budget.LoopVersion < 2 || state.Spec == nil {
		return TaskReview{}
	}
	if state.Budget.LoopVersion >= 3 {
		// V3 removed update_task_progress. Never advertise an obligation the
		// model cannot discharge; durable host progress owns convergence.
		remaining := state.Budget.Limits.ModelCalls - state.Budget.Spent.ModelCalls
		if len(state.Messages) > 0 {
			last := state.Messages[len(state.Messages)-1]
			if last.Role == "assistant" && len(last.ToolCalls) > 0 {
				remaining++ // The snapshot that admitted this proposal.
			}
		}
		if !state.Budget.Unlimited && remaining <= 1 {
			return TaskReview{Required: true, DeliveryRequired: true, Reason: "remaining model call is reserved for cited delivery; further acquisition is closed"}
		}
		return TaskReview{}
	}
	p := workingProgress(state)
	completedRounds := successfulModelRounds(state.Operations)
	remainingDecisions := state.Budget.Limits.ModelCalls - state.Budget.Spent.ModelCalls
	// A fresh proposal was made using the previous snapshot. Let that round
	// receive feedback before the next decision's periodic review becomes due.
	if len(state.Messages) > 0 {
		last := state.Messages[len(state.Messages)-1]
		if last.Role == "assistant" && len(last.ToolCalls) > 0 {
			completedRounds--
			remainingDecisions++ // Admission uses the snapshot that produced this proposal.
		}
	}
	r := TaskReview{UnassessedRounds: max(0, completedRounds-p.ModelRounds), StalledReviews: p.StalledReviews, RecoveryCycle: p.StalledReviews == 2}
	baseline := p.FactsSignature
	if baseline == "" {
		baseline = workingFactsSignature(LoopState{})
	}
	r.FactsChanged = baseline != workingFactsSignature(state)
	if r.FactsChanged {
		r.Required = true
		r.Reason = "execution or evidence changed; digest its effect on frozen requirements in update_task_progress before further acquisition"
	}
	if r.UnassessedRounds >= 3 {
		r.Required = true
		r.Reason = "three model rounds without a checkpoint; assess evidence, explored paths and the remaining purpose before further acquisition"
	}
	if p.StalledReviews >= 3 {
		r.Required = true
		r.Reason = "checkpoint recovery cycle produced no delivery progress; deliver accurate partial/failed output"
	}
	if !state.Budget.Unlimited && remainingDecisions <= 1 {
		r.DeliveryRequired = true
		r.Required = true
		r.Reason = "remaining model call is reserved for cited delivery; further acquisition is closed"
	}
	return r
}

func expandsAcquisition(name string) bool {
	switch name {
	case "discover_sources", "inspect_source", "validate_program", "submit_acquisition", "submit_extraction":
		return true
	}
	return false
}

// Checkpoints acknowledge original SQL operations, never projected previews.
func assessedHistoryRound(state LoopState, messages []Message) bool {
	if state.Budget.LoopVersion < 2 {
		return true
	}
	p := workingProgress(state)
	if p.Version == 0 && state.Budget.LoopVersion < 3 {
		return false
	}
	ids := map[string]uint64{}
	for _, op := range state.Operations {
		if (op.Stage == "succeeded" || op.Stage == "failed") && len(op.Key) > 5 && op.Key[:5] == "tool:" {
			ids[op.Key[5:]] = op.ID
		}
	}
	for _, m := range messages {
		for _, call := range m.ToolCalls {
			if ids[call.ID] == 0 || (state.Budget.LoopVersion < 3 && ids[call.ID] > p.ObservedThrough) {
				return false
			}
		}
		if m.Role == "tool" && (ids[m.ToolCallID] == 0 || (state.Budget.LoopVersion < 3 && ids[m.ToolCallID] > p.ObservedThrough)) {
			return false
		}
	}
	return true
}
