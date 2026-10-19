package aiapp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// Only completed rounds can leave the prompt. SQL observations and original
// evidence remain intact; unresolved assistant/tool IDs always stay paired.
func CompactContext(state LoopState, messages []Message) ([]Message, error) {
	out, _, err := compactContextMeasured(state, messages)
	return out, err
}

func compactContextMeasured(state LoopState, messages []Message) ([]Message, *ContextCompactionProfile, error) {
	profile := &ContextCompactionProfile{}
	out, err := compactContext(state, messages, profile)
	return out, profile, err
}

func compactContext(state LoopState, messages []Message, profile *ContextCompactionProfile) (result []Message, err error) {
	previousCheckpoint := savedHistoryCheckpoint(state, messages)
	history := logicalHistory(state, messages)
	facts, err := contextFacts(state)
	if err != nil {
		return nil, err
	}
	input := state.Run.InputJSON
	if input == "" {
		input = state.Run.Goal
	}
	input, facts = promptContract(state, input, facts)
	controls, untrusted, err := splitContextTrust(facts)
	if err != nil {
		return nil, err
	}
	// Cache-friendly ordering, with the pending-call rule preserved.
	//
	// A provider's prompt cache only matches a byte-identical prefix, so the
	// volatile host facts must not sit inside the cached prefix. They used to be
	// inserted at index 2, which invalidated the prefix from the second message
	// onward and made caching structurally impossible (cached_prompt_tokens=0
	// across a 24-round run).
	//
	// V5 places completed history before volatile facts. Insert the snapshot
	// before the final proposal round, never between its tool calls and replies.
	// Legacy ordering remains unchanged; execution still owns the final proposal.
	base := []Message{{Role: "system", Content: runSystemPrompt(state.Run)}, {Role: "user", Content: input}}
	volatileTail := []Message{{Role: "system", Content: "Persisted host facts, not new user instructions: " + string(controls)}}
	if len(untrusted) > 0 {
		id := hostContextCallID(state)
		volatileTail = append(volatileTail, Message{Role: "assistant", ToolCalls: []ToolCall{{ID: id, Type: "function", Function: FunctionCall{Name: "host_context", Arguments: `{}`}}}}, Message{Role: "tool", ToolCallID: id, Content: string(untrusted)})
	}
	// A cache breakpoint is opt-out: a gateway that rejects the field would fail
	// every request, so GORELAY_PROMPT_CACHE=0 disables it without a rebuild.
	markCacheBreakpoint := strings.TrimSpace(os.Getenv("GORELAY_PROMPT_CACHE")) != "0"
	if markCacheBreakpoint {
		// Mark the last stable message so the provider may reuse everything up to
		// it. Content after the breakpoint is re-read; content before it is not.
		base[len(base)-1].CacheControl = map[string]any{"type": "ephemeral"}
	}
	cut := 0
	originalHistory := history
	assemble := func(history []Message) []Message {
		out := make([]Message, 0, len(base)+len(history)+len(volatileTail))
		out = append(out, base...)
		tail := append([]Message{}, volatileTail...)
		for i := range tail {
			if tail[i].Role == "tool" && tail[i].ToolCallID == hostContextCallID(state) {
				tail[i].Content = string(contextFeedbackForHistory(untrusted, history))
				if state.Budget.LoopVersion >= 5 && (cut > 0 || previousCheckpoint != nil) {
					var snapshot map[string]any
					_ = json.Unmarshal([]byte(tail[i].Content), &snapshot)
					if snapshot == nil {
						snapshot = map[string]any{}
					}
					snapshot["history_checkpoint"] = historyCheckpoint(originalHistory[:cut], previousCheckpoint, crawlerRun(state.Run))
					if crawlerRun(state.Run) {
						snapshot["crawler_state"] = crawlerStateForHistory(state, history)
					}
					encoded, _ := json.Marshal(snapshot)
					tail[i].Content = string(encoded)
				}
			}
		}
		if state.Budget.LoopVersion >= 5 {
			boundary := len(history)
			for n := len(history) - 1; n >= 0; n-- {
				if history[n].Role == "assistant" {
					boundary = n
					break
				}
			}
			out = append(out, history[:boundary]...)
			out = append(out, tail...)
			out = append(out, history[boundary:]...)
			return out
		}
		out = append(out, tail...)
		out = append(out, history...)
		return out
	}
	out := assemble(history)
	beforeCompression := out
	profile.BeforeBytes = contextRequestBytes(state, out)
	profile.HistoryMessagesBefore = len(history)
	defer func() {
		if err == nil {
			finishContextMeasurement(state, beforeCompression, result, profile)
		}
	}()
	softLimit := 32 << 10
	measure := func(messages []Message) int { return contextRequestBytes(state, messages) }
	if state.Budget.LoopVersion >= 3 {
		// Static tools/contracts are not working history. Charging both to the
		// same soft window evicted a 17 KiB history after just a few reads.
		// Preserve a separate history allowance, within the unchanged hard cap.
		softLimit = min(64<<10, contextRequestBytes(state, base)+(32<<10))
	}
	profile.SoftLimit = softLimit
	target := softLimit
	if state.Budget.LoopVersion >= 5 && contextRequestBytes(state, out) > softLimit {
		target = min(softLimit, max(contextRequestBytes(state, base)+(16<<10), softLimit-(8<<10)))
	}
	profile.TargetBytes = target
	hardLimit := 64 << 10
	if state.Budget.LoopVersion < 2 {
		hardLimit = 32 << 10
	}
	if policy := state.Budget.PromptPolicy; policy != nil {
		measure = func(messages []Message) int { n, _ := estimateContextTokens(state, messages); return n }
		softLimit, target, hardLimit = policy.CompactAtTokens, policy.CompactAtTokens, policy.WindowTokens-policy.MaxOutputTokens
		if measure(out) > softLimit {
			target = policy.CompactToTokens
		}
		// Legacy byte diagnostics remain bytes, not token counts with old labels.
		profile.SoftLimit, profile.TargetBytes = 0, 0
	}
	if state.Budget.LoopVersion < 2 && contextRequestBytes(state, out) <= 32<<10 {
		return out, nil
	}
	tail := len(history)
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == "assistant" {
			tail = i
			break
		}
	}
	// V2 has explicit acknowledged checkpoints. V3 removed that model ceremony,
	// so a saved SQL receipt is not a replacement for model-visible feedback.
	// Keep recent sanitized rounds until actual window pressure; only then page
	// older completed rounds. Always retain pending proposals and their replies.
	for cut < tail && (state.Budget.LoopVersion == 2 || measure(out) > target) {
		next := tail
		for i := cut + 1; i <= tail; i++ {
			if history[i].Role == "assistant" {
				next = i
				break
			}
		}
		if !assessedHistoryRound(state, history[cut:next]) {
			break
		}
		cut = next
		profile.EvictedRounds++
		out = assemble(history[cut:])
	}
	// The hard cap matches what this protocol promised. A pre-V2 request was
	// specified at 32 KiB (see the entry check above), so letting it settle at
	// 64 KiB would silently double the budget the caller was told it had. V3+
	// carries a separate history allowance and keeps the larger ceiling.
	if measure(out) <= hardLimit {
		return out, nil
	}
	// Legacy requests retain their original protocol. New requests may discard
	// only checkpointed rounds; bound unread receipts while keeping their calls.
	currentStart := tail
	if state.Budget.LoopVersion >= 2 {
		currentStart = cut
	}
	current := append([]Message{}, history[currentStart:]...)
	out = assemble(current)
	if measure(out) <= hardLimit {
		return out, nil
	}
	if state.Budget.PromptPolicy != nil {
		return nil, fmt.Errorf("indivisible proposal or frozen contract exceeds the whole request budget: estimated input %d tokens, available input %d tokens; complete pending feedback was retained", measure(out), hardLimit)
	}
	// The owner of the whole request assigns bounded receipt views to this batch.
	// It preserves every proposed call and reply, with exact original read handles.
	for n := range current {
		if current[n].Role != "tool" {
			continue
		}
		var receipt ToolResult
		if json.Unmarshal([]byte(current[n].Content), &receipt) != nil || receipt.CallID == "" {
			continue
		}
		view := observationWithinBudget(receipt, 1024, state.Spec)
		raw, _ := json.Marshal(view)
		current[n].Content = string(raw)
	}
	out = assemble(current)
	if measure(out) > hardLimit {
		return nil, fmt.Errorf("indivisible proposal or frozen contract exceeds the whole request budget")
	}
	return out, nil
}

// The user message owns the original goal. Frozen fields live once in the host
// snapshot; canonical SQL inputs and standalone context readers remain intact.
func promptContract(state LoopState, input string, facts []byte) (string, []byte) {
	if state.Budget.LoopVersion < 2 {
		return input, facts
	}
	var request map[string]json.RawMessage
	var goal string
	if json.Unmarshal([]byte(input), &request) == nil {
		_ = json.Unmarshal(request["goal"], &goal)
	} else if input == state.Run.Goal {
		goal = input
	}
	if goal == "" || goal != state.Run.Goal {
		return input, facts
	}
	var snapshot map[string]json.RawMessage
	if json.Unmarshal(facts, &snapshot) != nil {
		return input, facts
	}
	delete(snapshot, "original_goal")
	if state.Spec != nil {
		canonical, _ := json.Marshal(state.Spec)
		if sameContractJSON(request["spec"], canonical) {
			delete(request, "spec")
		} else {
			var submitted map[string]json.RawMessage
			var submittedGoal string
			if json.Unmarshal(request["spec"], &submitted) == nil {
				_ = json.Unmarshal(submitted["goal"], &submittedGoal)
				if submittedGoal == goal {
					delete(submitted, "goal")
					request["spec"], _ = json.Marshal(submitted)
				}
			}
		}
		if state.Spec.Goal == goal {
			var frozen map[string]json.RawMessage
			_ = json.Unmarshal(snapshot["frozen_spec"], &frozen)
			delete(frozen, "goal")
			snapshot["frozen_spec"], _ = json.Marshal(frozen)
		}
	}
	if request != nil {
		encoded, _ := json.Marshal(request)
		input = string(encoded)
	}
	encoded, _ := json.Marshal(snapshot)
	return input, encoded
}

func sameContractJSON(a, b []byte) bool {
	var left, right any
	decode := func(raw []byte, dst *any) error {
		d := json.NewDecoder(bytes.NewReader(raw))
		d.UseNumber()
		return d.Decode(dst)
	}
	return decode(a, &left) == nil && decode(b, &right) == nil && reflect.DeepEqual(left, right)
}

func logicalHistory(state LoopState, messages []Message) []Message {
	out := []Message{}
	proposals := map[string]ToolCall{}
	for _, message := range messages {
		for _, call := range message.ToolCalls {
			proposals[call.ID] = call
		}
	}
	for _, m := range messages {
		if m.Role == "tool" && m.ToolCallID == hostContextCallID(state) {
			continue
		}
		if m.Role == "assistant" && len(m.ToolCalls) == 1 && m.ToolCalls[0].ID == hostContextCallID(state) && m.ToolCalls[0].Function.Name == "host_context" {
			continue
		}
		if m.Role == "user" {
			continue
		} // Run.InputJSON owns the original user contract.
		if m.Role == "system" && (m.Content == runSystemPrompt(state.Run) || strings.HasPrefix(m.Content, "Current host task facts") || strings.HasPrefix(m.Content, "Persisted host facts")) {
			continue
		}
		if m.Role == "tool" {
			var receipt ToolResult
			if json.Unmarshal([]byte(m.Content), &receipt) == nil {
				if crawlerRun(state.Run) && probeFeedback(receipt.Name) {
					// Rebuild an older compact pointer from its canonical receipt;
					// re-projecting a pointer cannot recover the lost next_action.
					for _, op := range state.Operations {
						if op.RunID == state.Run.ID && op.Key == "tool:"+receipt.CallID && op.Kind == receipt.Name && (op.Stage == "succeeded" || op.Stage == "failed") {
							var full ToolResult
							if json.Unmarshal([]byte(op.ResultJSON), &full) == nil {
								receipt = full
							}
							break
						}
					}
				}
				if receipt.Name == "inspect_source" {
					for _, op := range state.Operations {
						if op.RunID == state.Run.ID && op.Key == "tool:"+receipt.CallID && op.Kind == "inspect_source" && op.Stage == "succeeded" {
							var full ToolResult
							if json.Unmarshal([]byte(op.ResultJSON), &full) == nil {
								receipt = full
							}
							break
						}
					}
					r := state.Run
					if r.BudgetJSON == "" {
						raw, _ := json.Marshal(state.Budget)
						r.BudgetJSON = string(raw)
					}
					view := modelObservationForRun(r, receipt, state.Spec)
					raw, _ := json.Marshal(view)
					m.Content = string(raw)
				} else {
					view := historyReaderView(state, receipt, proposals[m.ToolCallID])
					if crawlerRun(state.Run) {
						view = modelObservationForRun(state.Run, view, state.Spec)
					}
					raw, _ := json.Marshal(view)
					m.Content = string(raw)
				}
			}
		}
		out = append(out, m)
	}
	return out
}

func hostContextCallID(state LoopState) string { return "host-context-" + state.Run.ID }

// Provenance validity is not instruction trust. Keep retrieved metadata,
// execution errors and model-written notes as tool data even after compaction.
func splitContextTrust(facts []byte) ([]byte, []byte, error) {
	var controls map[string]json.RawMessage
	if err := json.Unmarshal(facts, &controls); err != nil {
		return nil, nil, err
	}
	untrusted := map[string]json.RawMessage{}
	if raw, ok := controls["delivery_recovery"]; ok {
		// The phase is a host control. The brief embeds historical source/error
		// text and must retain tool-data trust after compaction.
		var phase struct {
			Phase string `json:"phase"`
		}
		if err := json.Unmarshal(raw, &phase); err != nil {
			return nil, nil, err
		}
		untrusted["delivery_recovery"] = raw
		controls["delivery_recovery"], _ = json.Marshal(phase)
	}
	for _, key := range []string{"progress", "conflicts", "reuse_issues", "recent_tool_feedback", "unresolved_tool_failures", "acquired_pages", "model_failure", "repeated_rejection"} {
		if raw, ok := controls[key]; ok {
			untrusted[key] = raw
			delete(controls, key)
		}
	}
	for key, keep := range map[string][]string{"children": {"task_id", "status", "records", "failure_code", "artifact_hash"}, "evidence_catalog": {"id", "read_with"}} {
		if raw, ok := controls[key]; ok {
			untrusted[key] = raw
			var rows []map[string]json.RawMessage
			if err := json.Unmarshal(raw, &rows); err != nil {
				return nil, nil, err
			}
			safe := []map[string]json.RawMessage{}
			for _, row := range rows {
				entry := map[string]json.RawMessage{}
				for _, field := range keep {
					if value, ok := row[field]; ok {
						entry[field] = value
					}
				}
				safe = append(safe, entry)
			}
			controls[key], _ = json.Marshal(safe)
		}
	}
	controlJSON, _ := json.Marshal(controls)
	if len(untrusted) == 0 {
		return controlJSON, nil, nil
	}
	dataJSON, _ := json.Marshal(untrusted)
	return controlJSON, dataJSON, nil
}

func contextRequestBytes(state LoopState, messages []Message) int {
	raw, _ := json.Marshal(ChatRequest{Messages: messages, Tools: runToolDefinitionsForState(state), MaxCompletionTokens: completionTokens(state)})
	return len(raw) + 256 // transport-owned model name and framing reserve.
}

func contextFacts(state LoopState) ([]byte, error) {
	observations := []map[string]any{}
	for _, op := range state.Operations {
		if op.RunID != "" && op.RunID != state.Run.ID {
			continue
		}
		if op.Stage == "succeeded" || op.Stage == "failed" || op.Stage == "unknown_external" {
			entry := map[string]any{"operation_key": op.Key, "kind": op.Kind, "stage": op.Stage}
			if strings.HasPrefix(op.Key, "tool:") && (op.Stage == "succeeded" || op.Stage == "failed") && op.ResultJSON != "" {
				entry["read_arguments"] = map[string]any{"tool_call_id": strings.TrimPrefix(op.Key, "tool:"), "pointer": "", "offset": 0, "limit": 1}
			}
			observations = append(observations, entry)
		}
	}
	if len(observations) > 8 {
		observations = observations[len(observations)-8:]
	}
	progress := state.Progress
	if progress.Version == 0 && state.Run.ProgressJSON != "" {
		var e error
		progress, e = progressOf(state.Run)
		if e != nil {
			return nil, e
		}
	}
	facts := map[string]any{"original_goal": state.Run.Goal, "frozen_spec": state.Spec, "progress": progress, "children": loopTaskFacts(state), "recent_observations": observations, "recover_facts_with": "read_task_context/get_task_results/list_sources/read_evidence/read_source_content/read_observation"}
	if state.Budget.LoopVersion >= 3 {
		facts["host_progress"] = state.HostProgress
		facts["recent_tool_feedback"] = recentToolFeedback(state)
		failures := unresolvedToolFailures(state)
		if crawlerRun(state.Run) {
			for i, failure := range failures {
				failures[i] = modelObservationForRun(state.Run, failure, state.Spec)
			}
		}
		facts["unresolved_tool_failures"] = failures
		pages := acquiredPageReferences(state)
		facts["acquired_pages"] = pages
		facts["recover_facts_with"] = "get_task_results/list_sources/read_data"
		// Delivery gate: the host knows observed pages are persisted but no
		// record has been submitted yet. This objective signal prevents the
		// repeated failure mode of reading everything and never delivering.
		if len(pages) > 0 && (state.Result == nil || len(state.Result.Records) == 0) {
			facts["delivery_pending"] = "Observed source pages are persisted but no records are submitted. Submit extracted records with submit_extraction, or validate_program, register_source and submit_acquisition for a reusable capability, then finish_task. Inspecting or reading more pages without submitting cannot complete the task."
		}
		for _, row := range observations {
			if args, ok := row["read_arguments"].(map[string]any); ok {
				row["read_arguments"] = map[string]any{"handle": args["tool_call_id"]}
			}
		}
	}
	if cooldowns := toolCooldowns(state.Operations, time.Now()); len(cooldowns) > 0 {
		facts["provider_cooldowns"] = cooldowns
	}
	if state.Budget.LoopVersion >= 5 {
		if failure := latestModelFailure(state.Operations); failure != nil && failure.Kind == "rejected_proposal" {
			facts["model_failure"] = map[string]any{"kind": failure.Kind, "message": failure.Message, "code": failure.Code, "execution_status": failure.ExecutionStatus, "details": failure.Details, "recovery": failure.Recovery, "tools_dispatched": false}
			facts["proposal_recovery"] = "The previous model response was received but rejected before any tool execution. Return one assistant decision with named function calls matching the available tool schemas. Existing results and receipts are preserved; do not repeat already completed work. Repeated rejected envelopes terminate after bounded correction attempts."
		}
	}
	if state.Budget.LoopVersion >= 2 {
		facts["task_review"] = taskReview(state)
	}
	if state.Result != nil {
		facts["coverage"] = state.Result.Coverage
		if state.Result.CapabilityRequired {
			facts["capability_delivery"] = map[string]any{"required": true, "published_and_used": state.Result.ReusableCapabilities}
		}
		facts["conflicts"] = state.Result.Conflicts
		facts["reuse_issues"] = state.Result.ReuseIssues
	}
	if state.Budget.PromptPolicy != nil {
		facts["run_budget"] = map[string]any{"mode": "task_deadline_and_spend", "deadline": state.Run.Deadline, "prompt_policy": state.Budget.PromptPolicy, "program_limits": state.Budget.ProgramLimits, "total_spent": state.Budget.Spent, "policy": "Healthy model/tool calls have no count ceiling. The task deadline, explicit authorized spend and listed isolated execution resource budgets remain binding. Input compaction uses the frozen token policy. Repeated decisions without delivery progress receive correction independently of costs. Probes, acquisition validation and formal execution use the same frozen program_limits; partition a larger acquisition into different URL/params if its real IO/output exceeds those resources."}
	} else if state.Budget.LoopVersion >= 4 {
		facts["run_budget"] = map[string]any{"mode": "host-progress-renewed", "total_spent": state.Budget.Spent, "progress_base": state.Budget.ProgressBase, "limits": state.Budget.Limits, "unlimited_spend": state.Budget.Unlimited, "policy": "Verified new work renews the work window. Repeated ineffective decisions terminate. Lifetime cost accounting and cancellation remain binding."}
		window := explorationReviewWindow(state)
		if state.Budget.LoopVersion == 4 && state.HostProgress.ExplorationRounds >= window {
			facts["exploration_recovery"] = map[string]any{"mode": "autonomous", "rounds_without_delivery": state.HostProgress.ExplorationRounds, "review_window": window, "instruction": "Preparation has not advanced accepted records or useful capability. Reuse persisted observations and successful work; identify the missing fields or failed strategy, then choose a different source/tool or deliver supported results. Necessary paging of already fetched data remains allowed. New pages, notes and IDs do not reset this review. If recovery cannot meet the frozen contract, return honest partial/failed delivery with gaps; human approval is not required to recover."}
		}
	} else if state.Budget.Unlimited {
		facts["run_budget"] = "unlimited experiment; attempts still accounted; cancellation and per-operation constraints remain"
	} else {
		facts["budget"] = state.Budget
	}
	if len(state.EvidenceCatalog) > 0 {
		facts["evidence_catalog"] = state.EvidenceCatalog[:min(4, len(state.EvidenceCatalog))]
		facts["evidence_count"] = len(state.EvidenceCatalog)
		if state.Budget.LoopVersion >= 5 {
			// Reading status is a host receipt fact, not a model-authored note.
			// It lets a restored planner move on after an unhelpful document.
			reading := map[string]bool{}
			for _, op := range state.Operations {
				if op.RunID != state.Run.ID || op.Kind != "read_data" || op.Stage != "succeeded" {
					continue
				}
				var out ToolResult
				var data struct {
					Handle  string `json:"handle"`
					Pointer string `json:"pointer"`
					Offset  int    `json:"offset"`
					Total   int    `json:"total_runes"`
					Text    string `json:"text"`
				}
				if json.Unmarshal([]byte(op.ResultJSON), &out) == nil && out.Error == "" && json.Unmarshal(out.Data, &data) == nil && strings.HasPrefix(data.Handle, "ev_") && data.Pointer == "/text" && data.Total > 0 {
					reading[data.Handle] = reading[data.Handle] || data.Offset+len([]rune(data.Text)) >= data.Total
				}
			}
			facts["evidence_reading"] = reading
		}
		facts["read_all_evidence_with"] = "read_task_context"
		if state.Budget.LoopVersion >= 3 {
			catalog := make([]map[string]any, 0, min(4, len(state.EvidenceCatalog)))
			for _, entry := range state.EvidenceCatalog[:min(4, len(state.EvidenceCatalog))] {
				copy := map[string]any{}
				for k, v := range entry {
					copy[k] = v
				}
				copy["read_with"] = map[string]any{"handle": entry["id"]}
				catalog = append(catalog, copy)
			}
			facts["evidence_catalog"] = catalog
			delete(facts, "read_all_evidence_with")
		}
	}
	if state.Budget.LoopVersion < 2 && repeatedObservationRounds(state.Operations) >= 3 {
		facts["convergence_feedback"] = "The same unchanged observation batch repeated three times. Read persisted progress, address a different unmet requirement, or deliver an accurate result. Repeating the batch again ends as stalled partial/failed, not success."
	}
	// A tool that keeps failing the same way is not learning. Every rejection is
	// already delivered as the tool reply, but a model that re-proposes the same
	// rejected payload gains nothing from seeing the same sentence again. Naming
	// the repeat count and the exact rejected argument turns a loop into a
	// decision: fix the named defect, change tool, or deliver what is supported.
	if repeat := repeatedToolRejection(state); repeat != nil {
		facts["repeated_rejection"] = repeat
	}
	if repeat := repeatedReadInformation(state.Operations); repeat != nil {
		facts["read_information_recovery"] = repeat
	}
	if period, count := repeatedObservationCycle(state.Operations); state.Budget.LoopVersion < 5 && count >= 2 {
		facts["loop_recovery"] = map[string]any{"period": period, "repeats": count, "instruction": "The same call pattern returned unchanged results. Use saved results; correct the reported cause, choose another strategy or deliver supported partial results with gaps. Repeating this unchanged cycle again terminates the run. New page offsets, corrected inputs and changed results remain allowed."}
	}
	if state.Budget.LoopVersion >= 5 && state.HostProgress.Recovery.Phase != "" {
		// The brief, not a bare instruction: it names the wrong direction, the
		// evidence for it, what the contract still needs, saved material the
		// model never used, and the one action to take next. A correction that
		// only says "change approach" gives the model nothing it did not have.
		facts["delivery_recovery"] = buildCorrectionBrief(state, state.HostProgress.Recovery.Phase, state.HostProgress.Recovery.Reason)
	}
	return json.Marshal(facts)
}

// repeatedToolRejection reports the most frequent identical tool rejection when
// one tool and error has already been refused more than once in this run. The
// rejected arguments are included because the fix is usually a specific field,
// not a different intention.
func repeatedToolRejection(state LoopState) map[string]any {
	type key struct{ name, err, inputHash string }
	activeCalls := map[string]bool{}
	for _, failure := range unresolvedToolFailures(state) {
		activeCalls[failure.CallID] = true
	}
	active := map[key]bool{}
	for _, op := range state.Operations {
		var reply ToolResult
		if json.Unmarshal([]byte(op.ResultJSON), &reply) == nil && activeCalls[reply.CallID] {
			active[key{reply.Name, reply.Error, op.InputHash}] = true
		}
	}
	counts := map[key]int{}
	lastArgs := map[key]string{}
	for _, op := range state.Operations {
		if op.RunID != state.Run.ID || !strings.HasPrefix(op.Key, "tool:") || (op.Stage != "failed" && op.Stage != "succeeded") {
			continue
		}
		var receipt ToolResult
		if json.Unmarshal([]byte(op.ResultJSON), &receipt) != nil {
			continue
		}
		if op.Stage == "succeeded" && receipt.Error == "" {
			for old := range counts {
				if old.name == op.Kind && old.inputHash == op.InputHash {
					delete(counts, old)
					delete(lastArgs, old)
				}
			}
			continue
		}
		if receipt.Error == "" {
			continue
		}
		k := key{receipt.Name, receipt.Error, op.InputHash}
		if !active[k] {
			continue
		}
		if op.InputHash == "" {
			continue
		} // An unknown proposal identity cannot prove an exact repeat.
		counts[k]++
		// Keep the rejected payload so the model can see exactly what was refused.
		for _, message := range state.Messages {
			for _, call := range message.ToolCalls {
				if call.ID == receipt.CallID {
					lastArgs[k] = call.Function.Arguments
				}
			}
		}
	}
	var worst key
	worstCount := 0
	for k, n := range counts {
		if n > worstCount {
			worst, worstCount = k, n
		}
	}
	if worstCount < 2 {
		return nil
	}
	out := map[string]any{
		"tool":        worst.name,
		"error":       worst.err,
		"times":       worstCount,
		"instruction": "These exact arguments previously returned the same error " + strconv.Itoa(worstCount) + " times. Review the current cause and observed source/state before retrying; correct the arguments or strategy when necessary. Historical errors do not prove that a changed source or recovered service will fail.",
	}
	if args := lastArgs[worst]; args != "" {
		out["rejected_arguments"] = truncateRunes(args, 600)
	}
	return out
}
