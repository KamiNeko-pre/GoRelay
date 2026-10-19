package aiapp

import (
	"encoding/json"
	"strings"
	"testing"

	"go-lua-crawler/internal/agentdata"
)

// modelCall builds a ledger row for one assistant turn that proposed a tool.
func modelCall(id, tool, args string) agentdata.Operation {
	payload := map[string]any{
		"message": map[string]any{
			"tool_calls": []any{map[string]any{
				"id":   id,
				"type": "function",
				"function": map[string]any{
					"name":      tool,
					"arguments": args,
				},
			}},
		},
	}
	raw, _ := json.Marshal(payload)
	return agentdata.Operation{Kind: "model", Stage: "succeeded", Key: "model:" + id, ResultJSON: string(raw)}
}

// toolReply builds a ledger row for one executed tool call.
func toolReply(callID, name, stage, errText string) agentdata.Operation {
	out := ToolResult{CallID: callID, Name: name, Error: errText}
	raw, _ := json.Marshal(out)
	return agentdata.Operation{Kind: name, Stage: stage, Key: "tool:" + callID, ResultJSON: string(raw)}
}

// A third near-identical call of the same tool must be reported, because it can
// only return what the earlier calls returned.
func TestArgumentRepeatIsReported(t *testing.T) {
	same := `{"handle":"call_1","view":"dom","selector":"ul li a"}`
	ops := []agentdata.Operation{
		modelCall("c1", "read_data", same),
		toolReply("c1", "read_data", "succeeded", ""),
		modelCall("c2", "read_data", same),
		toolReply("c2", "read_data", "succeeded", ""),
		modelCall("c3", "read_data", same),
		toolReply("c3", "read_data", "succeeded", ""),
	}
	facts := toolCallFacts(ops)
	repeats, ok := facts["repeated_tool_arguments"].([]map[string]any)
	if !ok || len(repeats) == 0 {
		t.Fatalf("third identical call was not reported: %s", describeToolCallFacts(facts))
	}
	if repeats[0]["tool"] != "read_data" {
		t.Fatalf("wrong tool reported: %v", repeats[0])
	}
}

// A legitimate correction must not be reported: the selector changed, so the
// call is a different attempt even though the tool is the same.
func TestChangedArgumentsAreNotReported(t *testing.T) {
	ops := []agentdata.Operation{
		modelCall("c1", "read_data", `{"handle":"call_1","view":"dom","selector":"ul li a"}`),
		toolReply("c1", "read_data", "failed", "selector matches no elements"),
		modelCall("c2", "read_data", `{"handle":"call_1","view":"dom","selector":"li p a"}`),
		toolReply("c2", "read_data", "succeeded", ""),
		modelCall("c3", "read_data", `{"handle":"call_1","view":"dom","selector":"li p"}`),
		toolReply("c3", "read_data", "succeeded", ""),
	}
	facts := toolCallFacts(ops)
	if _, ok := facts["repeated_tool_arguments"]; ok {
		t.Fatalf("a corrected selector was reported as a repeat: %s", describeToolCallFacts(facts))
	}
}

// Normal paging changes the offset, which must not look like a repeat.
func TestLegitimatePagingIsNotReported(t *testing.T) {
	ops := []agentdata.Operation{}
	for i, off := range []int{0, 1200, 2400, 3600} {
		id := "p" + string(rune('1'+i))
		ops = append(ops,
			modelCall(id, "read_data", `{"handle":"call_1","view":"content","offset":`+itoa(off)+`}`),
			toolReply(id, "read_data", "succeeded", ""),
		)
	}
	facts := toolCallFacts(ops)
	if _, ok := facts["repeated_tool_arguments"]; ok {
		t.Fatalf("paging was reported as a repeat: %s", describeToolCallFacts(facts))
	}
}

// Two consecutive failures of one tool must be reported so the caller changes
// approach instead of retrying a third identical time.
func TestConsecutiveFailuresAreReported(t *testing.T) {
	ops := []agentdata.Operation{
		modelCall("f1", "validate_program", `{"code":"return true,{items=}"}`),
		toolReply("f1", "validate_program", "failed", "syntax error"),
		modelCall("f2", "validate_program", `{"code":"return true,{items=}"}`),
		toolReply("f2", "validate_program", "failed", "syntax error"),
	}
	facts := toolCallFacts(ops)
	rows, ok := facts["tool_consecutive_failures"].([]map[string]any)
	if !ok || len(rows) == 0 {
		t.Fatalf("consecutive failures were not reported: %s", describeToolCallFacts(facts))
	}
	if rows[0]["tool"] != "validate_program" {
		t.Fatalf("wrong tool: %v", rows[0])
	}
}

// A success breaks the run, so a later single failure is not reported.
func TestSuccessResetsFailureRun(t *testing.T) {
	ops := []agentdata.Operation{
		modelCall("f1", "read_data", `{}`),
		toolReply("f1", "read_data", "failed", "transient"),
		modelCall("f2", "read_data", `{}`),
		toolReply("f2", "read_data", "succeeded", ""),
		modelCall("f3", "read_data", `{}`),
		toolReply("f3", "read_data", "failed", "transient"),
	}
	facts := toolCallFacts(ops)
	if _, ok := facts["tool_consecutive_failures"]; ok {
		t.Fatalf("a success should reset the failure run: %s", describeToolCallFacts(facts))
	}
}

// A healthy ledger produces no facts at all, so the request does not grow.
func TestHealthyLedgerProducesNoFacts(t *testing.T) {
	ops := []agentdata.Operation{
		modelCall("h1", "inspect_source", `{"url":"https://example.test/"}`),
		toolReply("h1", "inspect_source", "succeeded", ""),
		modelCall("h2", "read_data", `{"handle":"call_1","view":"content"}`),
		toolReply("h2", "read_data", "succeeded", ""),
	}
	if facts := toolCallFacts(ops); len(facts) != 0 {
		t.Fatalf("healthy ledger produced facts: %s", describeToolCallFacts(facts))
	}
}

// Different tools doing different things are never each other's repeat.
func TestDifferentToolsAreNotCompared(t *testing.T) {
	ops := []agentdata.Operation{
		modelCall("d1", "read_data", `{"handle":"h","view":"content"}`),
		toolReply("d1", "read_data", "succeeded", ""),
		modelCall("d2", "inspect_source", `{"url":"https://example.test/"}`),
		toolReply("d2", "inspect_source", "succeeded", ""),
		modelCall("d3", "read_data", `{"handle":"h","view":"content"}`),
		toolReply("d3", "read_data", "succeeded", ""),
	}
	facts := toolCallFacts(ops)
	if _, ok := facts["repeated_tool_arguments"]; ok {
		t.Fatalf("two calls of one tool were treated as a repeat: %s", describeToolCallFacts(facts))
	}
}

// The similarity helper must tolerate JSON punctuation differences and reject
// a real argument change.
func TestArgumentSimilarityBoundary(t *testing.T) {
	base := `{"handle":"call_1","view":"dom","selector":"ul li a"}`
	spaced := "{\n  \"handle\" : \"call_1\",\n  \"view\": \"dom\",\n  \"selector\": \"ul li a\"\n}"
	if got := argumentSimilarity(base, spaced); got < toolArgumentSimilarity {
		t.Fatalf("punctuation-only difference scored %.2f, want >= %.2f", got, toolArgumentSimilarity)
	}
	changed := `{"handle":"call_1","view":"dom","selector":"div.v_news_content"}`
	if got := argumentSimilarity(base, changed); got >= toolArgumentSimilarity {
		t.Fatalf("a changed selector scored %.2f, want < %.2f", got, toolArgumentSimilarity)
	}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b strings.Builder
	digits := []byte{}
	for v > 0 {
		digits = append(digits, byte('0'+v%10))
		v /= 10
	}
	if neg {
		b.WriteByte('-')
	}
	for i := len(digits) - 1; i >= 0; i-- {
		b.WriteByte(digits[i])
	}
	return b.String()
}
