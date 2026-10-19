package aiapp

import (
	"encoding/json"
	"go-lua-crawler/internal/agentdata"
	"sort"
	"strings"
)

// Feedback novelty.
//
// A loop is not "the same call twice"; it is "a round that carried no new
// information". The cheapest reliable signal of that is the feedback the round
// actually delivered back to the model, normalised so that rewording does not
// look like progress:
//
//   - a tool failure is reduced to its structured identity (category, code,
//     execution status, failing field) plus the failing arguments. "syntax
//     error at line 7" and "unexpected symbol near ')'" are the same diagnosis
//     for this purpose, and so are two different spellings of the same missing
//     parameter; a genuinely different failure has a different code or a
//     different failing field and therefore counts as new.
//   - a successful read is reduced to the identity of what it returned (handle,
//     view, and the content hash), which is what deliveryInformationSignature
//     already does.
//
// The result is a set of feedback identities. A round that adds none of them
// produced nothing the model did not already have. No embedding call, no token
// cost, and it is deterministic, which matters because a non-deterministic
// "similarity" judgement would make the loop bound itself unverifiable.

// feedbackIdentity is one normalised piece of feedback a round delivered.
func feedbackIdentities(ops []agentdata.Operation) []string {
	seen := map[string]bool{}
	for _, op := range ops {
		if op.Kind == "model" {
			continue
		}
		if op.Stage != "failed" && op.Stage != "succeeded" {
			continue
		}
		var out ToolResult
		if json.Unmarshal([]byte(op.ResultJSON), &out) != nil {
			continue
		}
		tool := out.Name
		if tool == "" {
			tool = op.Kind
		}
		if op.Stage == "failed" {
			seen[toolFailureIdentity(tool, out)] = true
			continue
		}
		if identity := toolSuccessIdentity(tool, out); identity != "" {
			seen[identity] = true
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// toolFailureIdentity reduces a failure to what actually distinguishes it from
// another failure of the same tool. Two errors that share a code and a failing
// field are the same diagnosis no matter how the provider worded it.
func toolFailureIdentity(tool string, out ToolResult) string {
	f := out.Failure
	if f == nil {
		// A failure without a structured contract still has usable identity: the
		// tool plus the normalised message text. Normalisation removes the parts
		// that change between identical diagnostics (line numbers, ids).
		return "fail|" + tool + "|ungoverned|" + normalizedMessage(out.Error)
	}
	parts := []string{"fail", tool, f.Category, f.Code, f.ExecutionStatus}
	if field := failureField(f); field != "" {
		parts = append(parts, "field="+field)
	}
	// The failing arguments are part of the identity: the same code with a
	// different proposed argument is a new attempt with new information, not a
	// repeat. Call ids and offsets are excluded so paging is not mistaken for a
	// new diagnosis.
	if out.Name != "" {
		parts = append(parts, "args="+normalizedArguments(out.Error))
	}
	return strings.Join(parts, "|")
}

// failureField extracts the field a validation error blames, from the
// structured details first and the message text second.
func failureField(f *FailureInfo) string {
	if f == nil {
		return ""
	}
	if f.Details != nil {
		var d struct {
			Field  string `json:"field"`
			Name   string `json:"name"`
			Param  string `json:"parameter"`
			Column string `json:"column"`
			Of     string `json:"of"`
		}
		if raw, err := json.Marshal(f.Details); err == nil {
			if json.Unmarshal(raw, &d) == nil {
				for _, candidate := range []string{d.Field, d.Name, d.Param, d.Of, d.Column} {
					if candidate != "" {
						return candidate
					}
				}
			}
		}
	}
	// Fall back to the wording the host already uses for parameter failures.
	for _, marker := range []string{"missing required parameter ", "unknown parameter ", "parameter "} {
		if i := strings.Index(f.Message, marker); i >= 0 {
			rest := strings.TrimSpace(f.Message[i+len(marker):])
			end := strings.IndexAny(rest, " ;,.")
			if end < 0 {
				end = len(rest)
			}
			if end > 0 {
				return rest[:end]
			}
		}
	}
	return ""
}

// toolSuccessIdentity identifies what a successful tool call returned. Two
// successful reads of the same content are the same information.
func toolSuccessIdentity(tool string, out ToolResult) string {
	// Reader progress has one owner: consumedReadRanges resolves views and
	// receipt chains to the immutable source. Counting handles here a second
	// time lets a re-read reset recovery despite adding no source information.
	if sourceReaderName(tool) || tool == "read_evidence" {
		return ""
	}
	if len(out.Data) == 0 {
		return "ok|" + tool
	}
	var data map[string]any
	if json.Unmarshal(out.Data, &data) != nil {
		return "ok|" + tool + "|" + agentdata.Hash([]any{string(out.Data)})
	}
	parts := []string{"ok", tool}
	for _, key := range []string{"handle", "view_version", "selector", "pointer", "block_id", "offset", "total_runes", "total_items"} {
		if v := data[key]; v != nil {
			parts = append(parts, key+"="+toComparable(v))
		}
	}
	// The material itself: whichever content keys this receipt carries.
	material := []string{}
	for _, key := range []string{"text", "value", "items", "records", "structure", "selector_catalog"} {
		if v := data[key]; v != nil {
			material = append(material, key+"="+agentdata.Hash([]any{v}))
		}
	}
	if len(material) > 0 {
		sort.Strings(material)
		parts = append(parts, material...)
	}
	return strings.Join(parts, "|")
}

// normalizedMessage strips the parts of a diagnostic that change between runs of
// the same failure: addresses, ids, and exact numeric offsets.
func normalizedMessage(s string) string {
	var b strings.Builder
	for _, token := range strings.Fields(s) {
		if isVolatileToken(token) {
			continue
		}
		b.WriteString(strings.ToLower(token))
		b.WriteByte(' ')
	}
	return strings.TrimSpace(b.String())
}

// normalizedArguments keeps the semantic shape of a failure message while
// dropping volatile coordinates, so that paging does not look like a new error.
func normalizedArguments(s string) string {
	return normalizedMessage(s)
}

func isVolatileToken(t string) bool {
	if t == "" {
		return true
	}
	digits, letters := 0, 0
	for _, r := range t {
		switch {
		case r >= '0' && r <= '9':
			digits++
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
			letters++
		}
	}
	// A token that is mostly digits is a coordinate: line number, offset, id.
	if digits > 0 && letters == 0 {
		return true
	}
	// Hex-looking ids.
	if strings.HasPrefix(strings.ToLower(t), "call_") || strings.HasPrefix(strings.ToLower(t), "0x") {
		return true
	}
	return false
}

func toComparable(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return trimFloat(t)
	case bool:
		return map[bool]string{true: "true", false: "false"}[t]
	default:
		raw, _ := json.Marshal(v)
		return agentdata.Hash([]any{string(raw)})
	}
}

// trimFloat renders a JSON number without a trailing ".0", so that an offset of
// 1200 and 1200.0 compare equal.
func trimFloat(f float64) string {
	raw, _ := json.Marshal(f)
	return strings.TrimSuffix(string(raw), ".0")
}
