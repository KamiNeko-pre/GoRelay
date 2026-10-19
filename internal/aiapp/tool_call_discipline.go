package aiapp

import (
	"encoding/json"
	"fmt"
	"go-lua-crawler/internal/agentdata"
	"sort"
	"strings"
)

// Tool-call discipline.
//
// Most agent loops do not look like one call repeated verbatim; they look like
// the same tool being asked again with cosmetically different arguments, or one
// tool failing repeatedly while the model keeps retrying it unchanged. Both are
// detectable from the operation ledger alone, with no embedding call, no entropy
// model and no new infrastructure:
//
//   - argument novelty: the same tool re-called with arguments that are nearly
//     the same string as an earlier call produced the same everything, so the
//     caller is told which earlier call it resembles rather than being silently
//     throttled.
//   - consecutive failures: a tool that failed N times in a row without an
//     intervening success is not going to succeed on the N+1th identical try;
//     the caller is told to change approach or report a gap.
//
// Both are advisory facts injected into the next request. They never rewrite a
// call, never block a legitimate retry after a real change, and never count a
// different tool as a repeat.

const (
	// toolArgumentRepeatLimit is how many near-identical arguments for one tool
	// are tolerated before the ledger calls it a repeat. Two allows a legitimate
	// "fix the selector and try again" while catching the third identical ask.
	toolArgumentRepeatLimit = 3
	// toolConsecutiveFailureLimit is how many consecutive failures of one tool
	// are tolerated before the caller must change approach or stop.
	toolConsecutiveFailureLimit = 2
	// toolArgumentSimilarity is the fraction of shared prefix tokens above which
	// two argument strings are treated as the same attempt. A changed page
	// offset, selector or program body moves the string well below this.
	toolArgumentSimilarity = 0.9
)

// toolCallFacts reports repeat and consecutive-failure observations for the next
// request. The map is empty when the run looks healthy.
func toolCallFacts(ops []agentdata.Operation) map[string]any {
	repeats := repeatedToolArguments(ops)
	failures := consecutiveToolFailures(ops)
	if len(repeats) == 0 && len(failures) == 0 {
		return nil
	}
	out := map[string]any{}
	if len(repeats) > 0 {
		out["repeated_tool_arguments"] = repeats
		out["repeated_tool_arguments_hint"] = "These calls carry arguments nearly identical to an earlier call of the same tool, so they can only return the same result. Change the selector, page, program or tool, or report the gap instead of asking again."
	}
	if len(failures) > 0 {
		out["tool_consecutive_failures"] = failures
		out["tool_consecutive_failures_hint"] = "These tools failed repeatedly with no success in between. Retrying the same way will fail the same way; change approach or deliver what is already supported."
	}
	return out
}

// repeatedToolArguments lists tools whose most recent calls repeat earlier
// arguments for the same tool.
func repeatedToolArguments(ops []agentdata.Operation) []map[string]any {
	type call struct {
		tool string
		args string
		id   string
	}
	byTool := map[string][]call{}
	var order []string
	for _, op := range ops {
		if op.Kind != "model" || op.Stage != "succeeded" {
			continue
		}
		var payload struct {
			Message struct {
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(op.ResultJSON), &payload) != nil {
			continue
		}
		for _, tc := range payload.Message.ToolCalls {
			name := tc.Function.Name
			if name == "" {
				continue
			}
			if _, seen := byTool[name]; !seen {
				order = append(order, name)
			}
			byTool[name] = append(byTool[name], call{tool: name, args: canonicalToolInput(FunctionCall{Name: name, Arguments: tc.Function.Arguments}), id: tc.ID})
		}
	}
	out := []map[string]any{}
	for _, name := range order {
		calls := byTool[name]
		if len(calls) < toolArgumentRepeatLimit {
			continue
		}
		last := calls[len(calls)-1]
		similar := 0
		var firstSimilar string
		for i := len(calls) - 2; i >= 0; i-- {
			if argumentSimilarity(calls[i].args, last.args) >= toolArgumentSimilarity {
				similar++
				if firstSimilar == "" {
					firstSimilar = calls[i].id
				}
			}
		}
		if similar == 0 {
			continue
		}
		entry := map[string]any{
			"tool":            name,
			"similar_calls":   similar,
			"considered_limit": toolArgumentRepeatLimit,
		}
		if firstSimilar != "" {
			entry["resembles_call"] = firstSimilar
		}
		entry["arguments_excerpt"] = truncateRunes(last.args, 160)
		out = append(out, entry)
	}
	return out
}

// consecutiveToolFailures lists tools that failed repeatedly without a success
// in between, counting only the trailing run of failures per tool.
func consecutiveToolFailures(ops []agentdata.Operation) []map[string]any {
	type state struct {
		run      int
		lastErr  string
	}
	tools := map[string]*state{}
	var order []string
	for _, op := range ops {
		if op.Kind == "model" {
			continue
		}
		name := toolNameOfOperation(op)
		if name == "" {
			continue
		}
		if _, seen := tools[name]; !seen {
			tools[name] = &state{}
			order = append(order, name)
		}
		st := tools[name]
		if op.Stage == "succeeded" {
			st.run, st.lastErr = 0, ""
			continue
		}
		if op.Stage != "failed" {
			continue
		}
		st.run++
		var receipt ToolResult
		if json.Unmarshal([]byte(op.ResultJSON), &receipt) == nil {
			st.lastErr = receipt.Error
		}
	}
	out := []map[string]any{}
	for _, name := range order {
		st := tools[name]
		if st.run < toolConsecutiveFailureLimit {
			continue
		}
		out = append(out, map[string]any{
			"tool":             name,
			"consecutive_failures": st.run,
			"considered_limit": toolConsecutiveFailureLimit,
			"last_error":       truncateRunes(st.lastErr, 200),
		})
	}
	return out
}

// toolNameOfOperation recovers the tool name from a ledger row. The row key is
// "tool:<call id>" and the receipt carries the name; both are used so an
// unparseable receipt still counts.
func toolNameOfOperation(op agentdata.Operation) string {
	var receipt ToolResult
	if json.Unmarshal([]byte(op.ResultJSON), &receipt) == nil && receipt.Name != "" {
		return receipt.Name
	}
	if strings.HasPrefix(op.Key, "tool:") {
		return strings.TrimPrefix(op.Key, "tool:")
	}
	return ""
}

// argumentSimilarity compares two canonical argument strings by shared prefix
// words. It deliberately ignores JSON punctuation so a reordered identical call
// still counts as a repeat, while a changed offset, selector or program body
// does not.
func argumentSimilarity(a, b string) float64 {
	if a == "" || b == "" {
		return 0
	}
	ta := argumentTokens(a)
	tb := argumentTokens(b)
	if len(ta) == 0 || len(tb) == 0 {
		return 0
	}
	shared := 0
	for i := 0; i < len(ta) && i < len(tb); i++ {
		if ta[i] != tb[i] {
			break
		}
		shared++
	}
	denominator := len(ta)
	if len(tb) > denominator {
		denominator = len(tb)
	}
	return float64(shared) / float64(denominator)
}

// argumentTokens splits canonical arguments into comparable words, dropping the
// structural punctuation that changes between otherwise identical calls.
func argumentTokens(s string) []string {
	out := []string{}
	var b strings.Builder
	flush := func() {
		if b.Len() > 0 {
			out = append(out, b.String())
			b.Reset()
		}
	}
	for _, r := range s {
		switch r {
		case '{', '}', '[', ']', ',', '"', ':', ' ', '\t', '\n', '\r':
			flush()
		default:
			b.WriteRune(r)
		}
	}
	flush()
	return out
}

// describeToolCallFacts renders the facts for the host-context block. Kept
// separate so the caller decides the transport, not the wording.
func describeToolCallFacts(facts map[string]any) string {
	if len(facts) == 0 {
		return ""
	}
	keys := make([]string, 0, len(facts))
	for k := range facts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		v, _ := json.Marshal(facts[k])
		fmt.Fprintf(&b, "%s: %s\n", k, v)
	}
	return b.String()
}
