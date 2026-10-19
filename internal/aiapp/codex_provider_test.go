package aiapp

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestCodexReasoningConfiguration(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, effort := range []string{"", "max", "high", "invalid"} {
		t.Run(effort, func(t *testing.T) {
			t.Setenv("GORELAY_CODEX_REASONING_EFFORT", effort)
			p, err := NewCodexProvider(binary, "gpt-6-luna", t.TempDir())
			if effort == "invalid" {
				if err == nil {
					t.Fatal("invalid effort accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := effort
			if want == "" {
				want = "low"
			}
			if p.ReasoningEffort != want || p.Model != "gpt-6-luna" {
				t.Fatalf("wrong model settings: %+v", p)
			}
		})
	}
}

func TestCodexTerminalHTTPFailurePreservesRetrySemantics(t *testing.T) {
	for _, tc := range []struct {
		message string
		status  int
		retry   bool
	}{
		{"workspace routing discovery unauthorized (401)", 401, false},
		{"request forbidden (403)", 403, false},
		{"request rate limited (429)", 429, true},
		{"gateway response failed (502)", 502, true},
	} {
		for _, search := range []bool{false, true} {
			raw := `{"type":"turn.failed","error":{"message":"` + tc.message + `"}}`
			_, _, _, err := parseCodexTurn(strings.NewReader(raw), search)
			var remote *ProviderError
			if !errors.As(err, &remote) || remote.Status != tc.status || remote.Retryable != tc.retry {
				t.Errorf("%s search=%t: transport facts lost: %v", tc.message, search, err)
			}
		}
	}
	// Intermediate reconnect warnings can recover; model/source text is not an error.
	raw := `{"type":"error","message":"Reconnecting... (401)"}
{"type":"item.completed","item":{"type":"agent_message","text":"source returned (403)"}}
{"type":"turn.completed"}`
	if _, _, _, err := parseCodexTurn(strings.NewReader(raw), false); err != nil {
		t.Fatal("warning or source text stopped a completed turn:", err)
	}
	_, _, _, err := parseCodexTurn(strings.NewReader(`{"type":"turn.failed","error":{"message":"connection reset by peer"}}`), false)
	var remote *ProviderError
	if err == nil || errors.As(err, &remote) {
		t.Fatal("unknown network outcome assigned an invented HTTP status:", err)
	}
}

func TestCodexTypedRefusalSurvivesProcessExit(t *testing.T) {
	process := errors.New("Codex process exit 1")
	for _, parsed := range []error{&ProviderError{Status: 401}, &ProviderError{Status: 403}, ErrProviderQuota} {
		if codexResultError(parsed, process) != parsed {
			t.Fatal("provider refusal overwritten by generic process failure")
		}
	}
	if codexResultError(errors.New("incomplete event stream"), process) != process {
		t.Fatal("unknown transport failure lost its process evidence")
	}
}

func TestCodexInferenceParsesActionsAndRealUsage(t *testing.T) {
	raw := `{"type":"thread.started","thread_id":"request-one"}
{"type":"item.completed","item":{"type":"agent_message","text":"{\"content\":\"Inspect missing source\",\"tool_calls\":[{\"name\":\"inspect_source\",\"arguments\":\"{\\\"url\\\":\\\"https://example.com\\\"}\"}]}"}}
{"type":"turn.completed","usage":{"input_tokens":120,"cached_input_tokens":80,"output_tokens":14}}`
	out, err := parseCodexInference(strings.NewReader(raw), ChatRequest{Tools: []ToolDefinition{{Function: FunctionDefinition{Name: "inspect_source"}}}}, "test-model")
	if err != nil || len(out.Message.ToolCalls) != 1 || out.Message.ToolCalls[0].Function.Name != "inspect_source" || out.RequestID != "request-one" || out.Usage == nil || out.Usage.TotalTokens != 134 || out.Usage.CachedPromptTokens != 80 {
		t.Fatalf("bad inference/usage: %+v %v", out, err)
	}
}

func TestCodexInferenceRejectsExecutedToolsAndInvalidProposals(t *testing.T) {
	for _, raw := range []string{
		`{"type":"item.started","item":{"type":"command_execution","command":"whoami"}}`,
		`{"type":"item.completed","item":{"type":"mcp_tool_call"}}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"{\"content\":\"\",\"tool_calls\":[{\"name\":\"invented\",\"arguments\":\"{}\"}]}"}}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"{\"content\":\"\",\"tool_calls\":[{\"name\":\"inspect_source\",\"arguments\":\"not json\"}]}"}}`,
	} {
		if _, err := parseCodexInference(strings.NewReader(raw), ChatRequest{Tools: []ToolDefinition{{Function: FunctionDefinition{Name: "inspect_source"}}}}, "test-model"); err == nil {
			t.Fatalf("non-inference accepted: %s", raw)
		}
	}
	// A legitimate plain JSON answer works without tools or reported usage.
	raw := `{"type":"item.completed","item":{"type":"agent_message","text":"{\"content\":\"answer\",\"tool_calls\":[]}"}}
{"type":"turn.completed"}`
	got, err := parseCodexInference(strings.NewReader(raw), ChatRequest{}, "test-model")
	if err != nil || got.Message.Content != "answer" || got.Usage != nil {
		t.Fatalf("plain answer/unknown usage: %+v %v", got, err)
	}
}

func TestCodexRejectedExecutionKeepsReportedUsage(t *testing.T) {
	for _, kind := range []string{"mcp_tool_call", "web_search", "command_execution"} {
		t.Run(kind, func(t *testing.T) {
			raw := `{"type":"item.completed","item":{"type":"` + kind + `"}}
{"type":"item.completed","item":{"type":"agent_message","text":"{\"content\":\"ignored\",\"tool_calls\":[]}"}}
{"type":"turn.completed","usage":{"input_tokens":120,"cached_input_tokens":80,"output_tokens":14}}`
			out, err := parseCodexInference(strings.NewReader(raw), ChatRequest{}, "test-model")
			if err == nil || out.Usage == nil || out.Usage.TotalTokens != 134 || out.Usage.CachedPromptTokens != 80 || out.Message.Content != "" {
				t.Fatalf("rejected %s lost billing or became accepted: %+v %v", kind, out, err)
			}
		})
	}
	out, err := parseCodexInference(strings.NewReader(`{"type":"item.started","item":{"type":"command_execution"}}`), ChatRequest{}, "test-model")
	if err == nil || out.Usage != nil {
		t.Fatal("invented usage for an incomplete rejected turn")
	}
	out, _, searches, err := parseCodexTurn(strings.NewReader(`{"type":"item.completed","item":{"type":"web_search"}}
{"type":"turn.completed","usage":{"input_tokens":9,"output_tokens":3}}`), true)
	if err != nil || searches != 1 || out.Usage == nil || out.Usage.TotalTokens != 12 {
		t.Fatal("legitimate search was rejected or lost cost", err)
	}
}

func TestCodexQuotaFailureIsTypedAndSourceTextCannotTriggerIt(t *testing.T) {
	for _, msg := range []string{"You've hit your usage limit. Try again later.", "You’ve hit your usage limit. Try again at 6:04 PM.", "Your workspace is out of credits. Add credits to continue."} {
		raw := `{"type":"turn.failed","error":{"message":"` + msg + `"}}`
		_, _, _, err := parseCodexTurn(strings.NewReader(raw), false)
		if err == nil || !strings.Contains(err.Error(), "provider quota exhausted") {
			t.Fatalf("quota left as generic retryable failure: %v", err)
		}
	}
	_, _, _, err := parseCodexTurn(strings.NewReader(`{"type":"turn.failed","error":{"message":"connection reset by peer"}}`), false)
	if err == nil || strings.Contains(err.Error(), "quota exhausted") {
		t.Fatal("network failure misclassified")
	}
	raw := `{"type":"item.completed","item":{"type":"agent_message","text":"source says You've hit your usage limit"}}
{"type":"turn.completed"}`
	if _, _, _, err := parseCodexTurn(strings.NewReader(raw), false); err != nil {
		t.Fatal("ordinary model text treated as provider error", err)
	}
}

func TestCodexStructuredBillingCodeIsTerminalWithoutMatchingMessage(t *testing.T) {
	for _, code := range []string{"insufficient_quota", "insufficient_credits"} {
		_, _, _, err := parseCodexTurn(strings.NewReader(`{"type":"turn.failed","error":{"code":"`+code+`","message":"provider billing rejected"}}`), false)
		if err == nil || !strings.Contains(err.Error(), "provider quota exhausted") {
			t.Fatal(code, err)
		}
	}
	_, _, _, err := parseCodexTurn(strings.NewReader(`{"type":"item.completed","item":{"type":"agent_message","text":"Your workspace is out of credits. Add credits to continue."}}
{"type":"turn.completed"}`), false)
	if err != nil {
		t.Fatal("source/model text triggered billing classification", err)
	}
}
