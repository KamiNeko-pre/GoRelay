package aiapp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/knowledge"
)

// fixedEmbedder returns caller-chosen vectors keyed by a substring of the text,
// so a test can decide exactly which feedback is "the same meaning" without a
// real model. Anything unmatched gets a distinct orthogonal vector.
type fixedEmbedder struct {
	groups  [][]string  // each group maps to one shared direction
	vectors [][]float32 // direction per group
	calls   int
	fail    bool
	lastLen int
}

func (e *fixedEmbedder) EmbedTexts(_ context.Context, texts []string) ([][]float32, error) {
	e.calls++
	e.lastLen = len(texts)
	if e.fail {
		return nil, errors.New("embedding unavailable")
	}
	out := make([][]float32, len(texts))
	for i, text := range texts {
		assigned := -1
		for g, group := range e.groups {
			for _, marker := range group {
				if strings.Contains(text, marker) {
					assigned = g
					break
				}
			}
			if assigned >= 0 {
				break
			}
		}
		if assigned >= 0 && assigned < len(e.vectors) {
			out[i] = e.vectors[assigned]
			continue
		}
		// Unique direction for ungrouped text: orthogonal to everything else.
		v := make([]float32, 64)
		v[i%64] = 1
		v[(i+31)%64] = 1
		out[i] = v
	}
	return out, nil
}

func serviceWithEmbedder(e *fixedEmbedder) *Service {
	return &Service{Retriever: &knowledge.Retriever{Embed: e}}
}

// modelRound appends one completed model round proposing the given tool calls.
func modelRound(id string, calls ...ToolCall) agentdata.Operation {
	payload := map[string]any{"message": map[string]any{"tool_calls": calls}}
	raw, _ := json.Marshal(payload)
	return agentdata.Operation{Kind: "model", Stage: "succeeded", Key: "model:" + id, ResultJSON: string(raw)}
}

// failedTool appends a failed tool reply carrying a structured failure.
func failedTool(callID, name, code, field, message string) agentdata.Operation {
	res := ToolResult{
		CallID: callID, Name: name, Error: message,
		Failure: &FailureInfo{
			Kind: "tool", Message: message, Category: "validation", Code: code,
			ExecutionStatus: "not_started", Retryable: false,
			Details: map[string]any{"field": field},
		},
	}
	raw, _ := json.Marshal(res)
	return agentdata.Operation{Kind: name, Stage: "failed", Key: "tool:" + callID, ResultJSON: string(raw)}
}

// The case this feature exists for: a different call whose feedback means the
// same thing. Structural identity sees two different messages; the vectors see
// one wall.
func TestSemanticRepeatDetectsRewordedFeedback(t *testing.T) {
	e := &fixedEmbedder{
		groups:  [][]string{{"selector matched nothing", "no element matches selector"}},
		vectors: [][]float32{{1, 0, 0, 0}},
	}
	s := serviceWithEmbedder(e)
	state := LoopState{
		Budget: BudgetSnapshot{LoopVersion: 5},
		Operations: []agentdata.Operation{
			modelRound("m1"),
			failedTool("c1", "read_data", "TOOL_EXECUTION_ERROR", "selector", "selector matched nothing on this page"),
			modelRound("m2"),
			failedTool("c2", "read_data", "TOOL_EXECUTION_ERROR", "selector", "no element matches selector here"),
		},
	}
	repeat, round := s.semanticRepeat(context.Background(), state)
	if !repeat {
		t.Fatal("reworded repeat of the same failure was not detected")
	}
	if round == "" {
		t.Fatal("repeat was reported without naming the matching round")
	}
	if e.calls != 1 {
		t.Fatalf("expected one batched embedding request, got %d", e.calls)
	}
}

// A genuinely different diagnosis must NOT be treated as a repeat, or a
// productive run would be cut short.
func TestSemanticRepeatIgnoresNewDiagnosis(t *testing.T) {
	e := &fixedEmbedder{
		groups: [][]string{
			{"selector matched nothing"},
			{"required parameter detail_limit missing"},
		},
		vectors: [][]float32{{1, 0, 0, 0}, {0, 1, 0, 0}},
	}
	s := serviceWithEmbedder(e)
	state := LoopState{
		Budget: BudgetSnapshot{LoopVersion: 5},
		Operations: []agentdata.Operation{
			modelRound("m1"),
			failedTool("c1", "read_data", "TOOL_EXECUTION_ERROR", "selector", "selector matched nothing"),
			modelRound("m2"),
			failedTool("c2", "submit_acquisition", "ARGUMENT_JSON_ERROR", "detail_limit", "required parameter detail_limit missing"),
		},
	}
	if repeat, _ := s.semanticRepeat(context.Background(), state); repeat {
		t.Fatal("a new diagnosis was misreported as a repeat")
	}
}

// Without an embedder the check must decline, never claim a repeat: a missing
// capability must not stop a healthy run.
func TestSemanticRepeatDeclinesWithoutEmbedder(t *testing.T) {
	s := &Service{}
	state := LoopState{
		Budget: BudgetSnapshot{LoopVersion: 5},
		Operations: []agentdata.Operation{
			modelRound("m1"), failedTool("c1", "read_data", "E", "f", "same"),
			modelRound("m2"), failedTool("c2", "read_data", "E", "f", "same"),
		},
	}
	if repeat, _ := s.semanticRepeat(context.Background(), state); repeat {
		t.Fatal("no embedder configured but a repeat was claimed")
	}
}

// An embedding outage must also decline rather than stop the run.
func TestSemanticRepeatDeclinesOnEmbeddingFailure(t *testing.T) {
	e := &fixedEmbedder{fail: true}
	s := serviceWithEmbedder(e)
	state := LoopState{
		Budget: BudgetSnapshot{LoopVersion: 5},
		Operations: []agentdata.Operation{
			modelRound("m1"), failedTool("c1", "read_data", "E", "f", "same"),
			modelRound("m2"), failedTool("c2", "read_data", "E", "f", "same"),
		},
	}
	if repeat, _ := s.semanticRepeat(context.Background(), state); repeat {
		t.Fatal("embedding failure was reported as a repeat")
	}
}

// A single round cannot be compared against anything.
func TestSemanticRepeatNeedsHistory(t *testing.T) {
	e := &fixedEmbedder{groups: [][]string{{"x"}}, vectors: [][]float32{{1, 0, 0, 0}}}
	s := serviceWithEmbedder(e)
	state := LoopState{
		Budget:     BudgetSnapshot{LoopVersion: 5},
		Operations: []agentdata.Operation{modelRound("m1"), failedTool("c1", "read_data", "E", "f", "x")},
	}
	if repeat, _ := s.semanticRepeat(context.Background(), state); repeat {
		t.Fatal("a repeat was claimed from a single round")
	}
	if e.calls != 0 {
		t.Fatalf("no comparison was possible but the embedder was called %d times", e.calls)
	}
}

// The comparison must be batched: one request covers the new round plus history.
func TestSemanticRepeatUsesOneBatchedRequest(t *testing.T) {
	e := &fixedEmbedder{
		groups:  [][]string{{"selector matched nothing"}},
		vectors: [][]float32{{1, 0, 0, 0}},
	}
	s := serviceWithEmbedder(e)
	ops := []agentdata.Operation{}
	for i := 0; i < 5; i++ {
		id := "m" + string(rune('1'+i))
		ops = append(ops,
			modelRound(id),
			failedTool("c"+id, "read_data", "TOOL_EXECUTION_ERROR", "selector", "selector matched nothing"),
		)
	}
	state := LoopState{Budget: BudgetSnapshot{LoopVersion: 5}, Operations: ops}
	if repeat, _ := s.semanticRepeat(context.Background(), state); !repeat {
		t.Fatal("five identical failures were not detected")
	}
	if e.calls != 1 {
		t.Fatalf("expected 1 batched request, got %d", e.calls)
	}
	// 1 new + 4 history
	if e.lastLen != 5 {
		t.Fatalf("expected 5 texts in the batch, got %d", e.lastLen)
	}
	if base := os.Getenv("GORELAY_TEST_EMBED_BASE_URL"); base != "" {
		provider, err := NewModelProvider(ProviderConfig{BaseURL: base, Model: "bge-small-zh-v1.5-f16", APIKey: "local-bge"})
		if err != nil {
			t.Fatal(err)
		}
		s.LoopEmbedding = EmbeddingAdapter{Provider: provider}
		check := s.assessSemanticRepeat(context.Background(), state)
		if check.Status != "checked" || check.BatchSize != 5 || check.Usage == nil || check.Usage.TotalTokens <= 0 || check.Model == "" {
			t.Fatalf("real semantic measurement missing: %+v", check)
		}
		raw, _ := json.Marshal(check)
		t.Logf("real semantic receipt: %s", raw)
	}
}

// Cosine must be a real measure, not a placeholder.
func TestCosineMeasuresSimilarity(t *testing.T) {
	if got := cosine([]float32{1, 0}, []float32{1, 0}); got < 0.999 {
		t.Fatalf("identical vectors scored %.3f", got)
	}
	if got := cosine([]float32{1, 0}, []float32{0, 1}); got > 0.001 {
		t.Fatalf("orthogonal vectors scored %.3f", got)
	}
	if got := cosine([]float32{1, 0}, []float32{-1, 0}); got > -0.999 {
		t.Fatalf("opposite vectors scored %.3f", got)
	}
	if got := cosine([]float32{1, 0}, []float32{1, 0, 0}); got != 0 {
		t.Fatalf("mismatched lengths scored %.3f, want 0", got)
	}
}

// The threshold must be high enough that a merely related diagnosis is not a
// repeat. This pins the configured value to a documented, testable number.
func TestSemanticThresholdIsHigh(t *testing.T) {
	if semanticRepeatSimilarity < 0.85 {
		t.Fatalf("threshold %.2f is low enough to misjudge related feedback as a repeat", semanticRepeatSimilarity)
	}
	if semanticRepeatSimilarity > 0.99 {
		t.Fatalf("threshold %.2f would never fire on real paraphrases", semanticRepeatSimilarity)
	}
}

// Measured against the local model, "field=detail_limit" and "field=list_url"
// embed at 0.94 — above the threshold — yet they are different problems needing
// different fixes. Structural identity must veto the vector match.
func TestSemanticRepeatVetoedByDifferentFailingField(t *testing.T) {
	// One shared direction: the test asserts that similarity alone would match,
	// and that the structured veto still prevents the repeat.
	e := &fixedEmbedder{
		groups:  [][]string{{"missing required parameter"}},
		vectors: [][]float32{{1, 0, 0, 0}},
	}
	s := serviceWithEmbedder(e)
	state := LoopState{
		Budget: BudgetSnapshot{LoopVersion: 5},
		Operations: []agentdata.Operation{
			modelRound("m1"),
			failedTool("c1", "submit_acquisition", "ARGUMENT_JSON_ERROR", "detail_limit",
				"missing required parameter detail_limit; supply params as {detail_limit}"),
			modelRound("m2"),
			failedTool("c2", "submit_acquisition", "ARGUMENT_JSON_ERROR", "list_url",
				"missing required parameter list_url; supply params as {list_url}"),
		},
	}
	if repeat, round := s.semanticRepeat(context.Background(), state); repeat {
		t.Fatalf("a different failing field was called a repeat of %s", round)
	}
}

// The same code, status and field is a repeat even when the wording differs.
func TestSemanticRepeatRequiresMatchingStructuredIdentity(t *testing.T) {
	e := &fixedEmbedder{
		groups:  [][]string{{"matched nothing", "did not match any"}},
		vectors: [][]float32{{1, 0, 0, 0}},
	}
	s := serviceWithEmbedder(e)
	state := LoopState{
		Budget: BudgetSnapshot{LoopVersion: 5},
		Operations: []agentdata.Operation{
			modelRound("m1"),
			failedTool("c1", "read_data", "TOOL_EXECUTION_ERROR", "selector", "selector matched nothing"),
			modelRound("m2"),
			failedTool("c2", "read_data", "TOOL_EXECUTION_ERROR", "selector", "the selector did not match any node"),
		},
	}
	if repeat, _ := s.semanticRepeat(context.Background(), state); !repeat {
		t.Fatal("same code, status and field was not recognised as a repeat")
	}
}

// A different execution status means the second attempt actually ran, so it
// cannot be the same wall as the first.
func TestSemanticRepeatVetoedByExecutionStatus(t *testing.T) {
	e := &fixedEmbedder{
		groups:  [][]string{{"failed to reach the source"}},
		vectors: [][]float32{{1, 0, 0, 0}},
	}
	s := serviceWithEmbedder(e)
	mk := func(callID, status string) agentdata.Operation {
		res := ToolResult{CallID: callID, Name: "inspect_source", Error: "failed to reach the source",
			Failure: &FailureInfo{Kind: "tool", Message: "failed to reach the source", Category: "network",
				Code: "PROVIDER_TIMEOUT", ExecutionStatus: status}}
		raw, _ := json.Marshal(res)
		return agentdata.Operation{Kind: "inspect_source", Stage: "failed", Key: "tool:" + callID, ResultJSON: string(raw)}
	}
	state := LoopState{
		Budget: BudgetSnapshot{LoopVersion: 5},
		Operations: []agentdata.Operation{
			modelRound("m1"), mk("c1", "not_started"),
			modelRound("m2"), mk("c2", "unknown"),
		},
	}
	if repeat, _ := s.semanticRepeat(context.Background(), state); repeat {
		t.Fatal("a different execution status was treated as the same failure")
	}
}

// Feedback text must carry the structured code and the failing field, because
// those are what make two differently-worded failures comparable.
func TestFeedbackTextCarriesStructuredIdentity(t *testing.T) {
	res := ToolResult{
		Name: "submit_acquisition",
		Failure: &FailureInfo{
			Message:  "missing required parameter detail_limit; supply params as {detail_limit}",
			Category: "validation", Code: "ARGUMENT_JSON_ERROR", ExecutionStatus: "not_started",
			Details: map[string]any{"field": "detail_limit"},
		},
	}
	text := failureTextForSemantics(res)
	for _, want := range []string{"validation", "ARGUMENT_JSON_ERROR", "not_started", "detail_limit"} {
		if !strings.Contains(text, want) {
			t.Fatalf("feedback text lost %q: %s", want, text)
		}
	}
}

// Success text must describe what came back without embedding the whole body.
func TestSuccessTextIsBoundedAndDescriptive(t *testing.T) {
	body := strings.Repeat("very long page body ", 5000)
	res := ToolResult{Name: "read_data", Data: mustJSON(map[string]any{
		"selector": "ul li a", "offset": 1200, "total_runes": 40000, "text": body,
	})}
	text := successTextForSemantics(res)
	if len(text) > semanticTextLimit {
		t.Fatalf("success text is %d runes, over the %d limit", len(text), semanticTextLimit)
	}
	if strings.Contains(text, body) {
		t.Fatal("success text embedded the whole body")
	}
	for _, want := range []string{"selector", "offset"} {
		if !strings.Contains(text, want) {
			t.Fatalf("success text lost %q: %s", want, text)
		}
	}
}
