package aiapp

import (
	"context"
	"encoding/json"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/knowledge"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Compare every settled model decision, including successful tool rounds.
// Reasoning and observed outcomes identify repeated methods; typed failures
// distinguish different diagnoses. Similarity can request correction, while
// accepted output owns actual progress. Low similarity alone cannot renew it.
//
// The embedder is a local model, so the only cost is latency (measured ~34 ms for
// a batch, vs seconds for a model round), and the comparison is batched: one
// call carries this round's texts plus the recent history's, so a whole
// comparison is a single request.
//
// The threshold is a configured starting value, not a proven optimum. It is
// deliberately high: misjudging a genuinely new diagnosis as a repeat would cut
// a productive run short, which is worse than letting a repeat through for one
// more round.

const (
	// semanticRepeatSimilarity: at or above this cosine similarity, two feedback
	// texts carry the same information for the purpose of loop detection.
	semanticRepeatSimilarity = 0.92
	// semanticHistoryWindow is how many earlier feedback texts a round is
	// compared against. The loop is a recent phenomenon; comparing against the
	// whole run would make any recurring-but-legitimate observation look like a
	// repeat.
	semanticHistoryWindow = 12
	// semanticTextLimit bounds each embedded string so one long receipt cannot
	// dominate the request.
	semanticTextLimit = 800
)

// semanticRepeat reports whether this round's feedback carries the same meaning
// as an earlier round's. It returns false whenever the check cannot run
// (no embedder configured, embedding failed, not enough history), because a
// missing check must never be read as "no new information" — that would stop a
// healthy run on an infrastructure hiccup.
//
// Structural identity takes precedence over the vector comparison. If the two
// rounds already differ in their structured failure identity — a different code,
// a different execution status, or a different blamed field — the difference is
// real regardless of how close the wording sits, and the round counts as new.
// Measured example: "field=detail_limit" and "field=list_url" embed at 0.94,
// above the threshold, yet they are different problems that need different
// fixes.
type SemanticAssessment struct {
	Model        string                    `json:"model,omitempty"`
	RequestID    string                    `json:"provider_request_id,omitempty"`
	Usage        *knowledge.EmbeddingUsage `json:"usage"`
	Status       string                    `json:"status"`
	Repeated     bool                      `json:"repeated"`
	MatchedRound string                    `json:"matched_round,omitempty"`
	Similarity   float64                   `json:"similarity"`
	Threshold    float64                   `json:"threshold"`
	BatchSize    int                       `json:"batch_size"`
	DurationMS   int64                     `json:"duration_ms"`
	Error        string                    `json:"error,omitempty"`
}

func (s *Service) semanticRepeat(ctx context.Context, state LoopState) (bool, string) {
	check := s.assessSemanticRepeat(ctx, state)
	return check.Repeated, check.MatchedRound
}

func (s *Service) assessSemanticRepeat(ctx context.Context, state LoopState) (check SemanticAssessment) {
	started := time.Now()
	check.Threshold = semanticRepeatSimilarity
	defer func() { check.DurationMS = time.Since(started).Milliseconds() }()
	embed := s.embedder()
	if embed == nil {
		check.Status = "not_configured"
		return
	}
	texts := feedbackTexts(state.Operations)
	if len(texts) < 2 {
		check.Status = "insufficient_history"
		return
	}
	// Compare the newest round against the ones before it.
	newest := texts[len(texts)-1]
	start := len(texts) - 1 - semanticHistoryWindow
	if start < 0 {
		start = 0
	}
	history := texts[start : len(texts)-1]
	if len(history) == 0 {
		check.Status = "insufficient_history"
		return
	}
	newestKey := structuralFeedbackKey(state.Operations, len(texts))
	batch := append([]string{newest.text}, func() []string {
		out := make([]string, 0, len(history))
		for _, h := range history {
			out = append(out, h.text)
		}
		return out
	}()...)
	var vectors [][]float32
	var err error
	if measured, ok := embed.(knowledge.MeasuredEmbedder); ok {
		result, cause := measured.EmbedMeasured(ctx, batch)
		vectors, err = result.Vectors, cause
		check.Model, check.RequestID, check.Usage = result.Model, result.RequestID, result.Usage
	} else {
		vectors, err = embed.EmbedTexts(ctx, batch)
	}
	check.BatchSize = len(batch)
	if err != nil || len(vectors) != len(batch) {
		check.Status = "error"
		check.Error = "embedding vector count does not match decision batch"
		if err != nil {
			check.Error = err.Error()
		}
		return
	}
	check.Status = "checked"
	target := vectors[0]
	historyStart := len(texts) - len(history)
	for i := 1; i < len(vectors); i++ {
		similarity := cosine(target, vectors[i])
		if similarity < semanticRepeatSimilarity {
			if similarity > check.Similarity {
				check.Similarity = similarity
			}
			continue
		}
		// The wording matches. Before calling it a repeat, require that the
		// structured diagnosis also matches: a different failing field is a
		// different problem however similar the sentence looks.
		if key := structuralFeedbackKey(state.Operations, historyStart+i-1); key != newestKey {
			continue
		}
		check.Repeated, check.MatchedRound, check.Similarity = true, history[i-1].round, similarity
		return
	}
	return
}

// structuralFeedbackKey is the ordered set of *structural* failure identities
// for the round at the given 1-based feedback index.
//
// Only the fields that name a distinct problem are included: category, code,
// execution status and the blamed field. Message text is deliberately excluded,
// because wording is exactly what the vector comparison exists to see through.
// Including it would make two differently-worded reports of one problem look
// structurally different and veto the very match this feature is for.
func structuralFeedbackKey(ops []agentdata.Operation, feedbackIndex int) string {
	rounds := structuralFailureRounds(ops)
	if feedbackIndex <= 0 || feedbackIndex > len(rounds) {
		return ""
	}
	keys := append([]string{}, rounds[feedbackIndex-1]...)
	sort.Strings(keys)
	return strings.Join(keys, "|")
}

// structuralFailureRounds returns the structural identity of each round's
// failures, aligned by index with feedbackTexts.
func structuralFailureRounds(ops []agentdata.Operation) [][]string {
	out := [][]string{}
	current := []string{}
	active := false
	flush := func() {
		if active {
			out = append(out, append([]string{}, current...))
		}
		current = nil
	}
	for _, op := range ops {
		if op.Kind == "model" {
			if op.Stage == "reserved" {
				continue
			}
			flush()
			active = true
			continue
		}
		if !active || op.Stage != "failed" {
			continue
		}
		var res ToolResult
		if json.Unmarshal([]byte(op.ResultJSON), &res) != nil {
			continue
		}
		name := res.Name
		if name == "" {
			name = op.Kind
		}
		current = append(current, structuralFailureKey(name, res))
	}
	flush()
	return out
}

// Mechanical diagnoses are distinct from action identity. Wording, receipt IDs
// and timestamps cannot manufacture novelty; actual host-checked locators can
// show that the model applied a genuinely different repair.
func fieldFailureIdentities(f *FailureInfo, actions bool) []string {
	if f == nil {
		return nil
	}
	raw, _ := json.Marshal(f.Details)
	var details struct {
		Field  string       `json:"field"`
		Actual FieldSupport `json:"actual_support"`
		Fields []struct {
			Field  string       `json:"field"`
			Actual FieldSupport `json:"actual_support"`
		} `json:"unsupported_fields"`
	}
	if json.Unmarshal(raw, &details) != nil {
		return nil
	}
	keys := map[string]bool{}
	add := func(field string, support FieldSupport) {
		if support.DiagnosticCode == "" {
			return
		}
		parts := []any{field, support.DiagnosticCode}
		if actions {
			var loc *fieldLocator
			if support.ActualLocator != nil {
				copy := *support.ActualLocator
				copy.SourceURL = ""
				loc = &copy
			}
			parts = append(parts, support.SourceDigest, loc)
		}
		keys[agentdata.Hash(parts)] = true
	}
	if len(details.Fields) > 0 {
		for _, field := range details.Fields {
			add(field.Field, field.Actual)
		}
	} else {
		add(details.Field, details.Actual)
	}
	out := []string{}
	for key := range keys {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// structuralFailureKey names the problem without its wording.
func structuralFailureKey(tool string, res ToolResult) string {
	f := res.Failure
	if f == nil {
		// No structured contract: the tool name is all that is reliably known.
		return "fail|" + tool + "|ungoverned"
	}
	parts := []string{"fail", tool, f.Category, f.Code, f.ExecutionStatus}
	if field := failureField(f); field != "" {
		parts = append(parts, "field="+field)
	}
	parts = append(parts, fieldFailureIdentities(f, false)...)
	return strings.Join(parts, "|")
}

// embedder exposes the configured local embedding transport, if any.
func (s *Service) embedder() interface {
	EmbedTexts(context.Context, []string) ([][]float32, error)
} {
	if s == nil {
		return nil
	}
	if s.LoopEmbedding != nil {
		return s.LoopEmbedding
	}
	if s.Retriever == nil {
		return nil
	}
	return s.Retriever.Embed
}

type feedbackText struct {
	round string
	text  string
}

// DecisionSemanticText is an embedding projection, not a replacement for the
// model's full reasoning or context. It excludes large code/argument payloads.
func DecisionSemanticText(message Message) string {
	parts := []string{}
	thought := message.ReasoningContent
	if thought == "" {
		thought = message.Content
	}
	if thought != "" {
		parts = append(parts, "decision: "+truncateRunes(thought, semanticTextLimit/2))
	}
	for _, call := range message.ToolCalls {
		parts = append(parts, "action: "+call.Function.Name)
	}
	return truncateRunes(strings.Join(parts, " ; "), semanticTextLimit)
}

// feedbackTexts aligns every completed model decision with both successful and
// failed tool feedback. Reasoning/content explains the intended strategy;
// bounded success metadata and typed failures explain what actually happened.
// Similarity diagnoses repeated method, never proves delivery progress.
func feedbackTexts(ops []agentdata.Operation) []feedbackText {
	out := []feedbackText{}
	roundIndex := 0
	current := []string{}
	active := false
	flush := func() {
		if active {
			out = append(out, feedbackText{round: "round-" + strconv.Itoa(roundIndex), text: truncateRunes(strings.Join(current, " ; "), semanticTextLimit)})
		}
		current = nil
	}
	for _, op := range ops {
		if op.Kind == "model" {
			if op.Stage == "reserved" {
				continue
			}
			flush()
			active = true
			roundIndex++
			var response ChatResponse
			if json.Unmarshal([]byte(op.ResultJSON), &response) == nil {
				if text := DecisionSemanticText(response.Message); text != "" {
					current = append(current, text)
				}
			}
			continue
		}
		if !active || (op.Stage != "failed" && op.Stage != "succeeded") {
			continue
		}
		var res ToolResult
		if json.Unmarshal([]byte(op.ResultJSON), &res) != nil {
			continue
		}
		name := res.Name
		if name == "" {
			name = op.Kind
		}
		if op.Stage == "failed" {
			current = append(current, name+" failed: "+failureTextForSemantics(res))
		} else {
			current = append(current, name+" succeeded: "+successTextForSemantics(res))
		}
	}
	flush()
	return out
}

// failureTextForSemantics is the string embedded for a failure. It keeps the
// wording, because wording is what similarity compares, and prefixes the
// structured code so that the same code with a reworded message still lands
// close in vector space.
func failureTextForSemantics(res ToolResult) string {
	parts := []string{}
	if f := res.Failure; f != nil {
		for _, p := range []string{f.Category, f.Code, f.ExecutionStatus} {
			if p != "" {
				parts = append(parts, p)
			}
		}
		if field := failureField(f); field != "" {
			parts = append(parts, "field="+field)
		}
		if f.Message != "" {
			parts = append(parts, f.Message)
		}
	}
	if res.Error != "" {
		parts = append(parts, res.Error)
	}
	return truncateRunes(strings.Join(parts, " "), semanticTextLimit)
}

// successTextForSemantics describes a successful reply without its bulk: the
// selector, pointer, view, handle and a content hash say what was returned.
func successTextForSemantics(res ToolResult) string {
	parts := []string{}
	var data map[string]any
	if json.Unmarshal(res.Data, &data) == nil {
		for _, key := range []string{"view_version", "selector", "pointer", "block_id", "offset", "total_runes", "total_items", "count"} {
			if v := data[key]; v != nil {
				parts = append(parts, key+"="+toComparable(v))
			}
		}
		for _, key := range []string{"text", "value", "items", "records", "selector_catalog", "structure"} {
			if v := data[key]; v != nil {
				parts = append(parts, key+"#"+agentdata.Hash([]any{v})[:12])
			}
		}
	}
	if len(parts) == 0 {
		parts = append(parts, agentdata.Hash([]any{string(res.Data)})[:12])
	}
	return truncateRunes(strings.Join(parts, " "), semanticTextLimit)
}

// cosine similarity of two equal-length vectors.
func cosine(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
