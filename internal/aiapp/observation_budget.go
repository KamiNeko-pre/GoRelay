package aiapp

import "encoding/json"

// Layered read budgets.
//
// One flat ceiling produced every truncation defect on record: guidance fields
// (the selector menu, the region index, structure samples) competed with the
// paged text for the same bytes, so keeping the menu dropped the text and
// keeping the text dropped the menu, and an over-budget first read rejected the
// whole receipt. The budgets are now split by kind:
//
//	core   observationBytes (observation.go)  paged text plus its continuation
//	                                          state; the pager halves the text
//	                                          when this is exceeded
//	nav    navigationBytes                    one-shot guidance that a caller
//	                                          needs to choose its next read;
//	                                          trimmed within its own budget,
//	                                          never charged against the text
//	envelope envelopeAllowance                fixed receipt metadata
const (
	navigationBytes   = 4 << 10
	envelopeAllowance = 512
)

// continuationKeys bind one read to its next call. They survive every bound:
// losing the cursor is what produced the six-identical-reads loop.
var continuationKeys = map[string]bool{
	"handle": true, "next_read": true, "next_cursor": true,
	"unread_runes": true, "continue_with": true,
	"offset": true, "total_runes": true, "view_version": true,
}

// navigationKeys are single-shot guidance. They answer "what should I read
// next", never "what does the page say", so they live in their own budget.
var navigationKeys = map[string]bool{
	"selector_menu": true, "regions": true, "regions_note": true,
	"structure": true, "structure_note": true, "structure_more": true,
	"narrow_with": true, "next_step": true, "pointer_hint": true,
	"available_pointers": true,
}

// splitReadPayload separates guidance from bulk content so each can be bounded
// against its own ceiling instead of stealing from the other.
func splitReadPayload(data map[string]any) (core, nav map[string]any) {
	core = map[string]any{}
	nav = map[string]any{}
	for k, v := range data {
		switch {
		case navigationKeys[k]:
			nav[k] = v
		default:
			core[k] = v
		}
	}
	return core, nav
}

// boundNavigation trims guidance inside its own budget. Structure entries are
// the bulk: their markup samples shrink first (the selector is the decision
// fact), then the entry count. A caller that loses a sample keeps the selector
// it was choosing between; a caller that loses text keeps its continuation.
func boundNavigation(nav map[string]any) map[string]any {
	if len(nav) == 0 {
		return nav
	}
	raw, _ := json.Marshal(nav)
	if len(raw) <= navigationBytes {
		return nav
	}
	// Shrink markup samples inside structure entries before dropping entries.
	if entries, ok := nav["structure"].([]any); ok {
		for i, e := range entries {
			if item, ok := e.(map[string]any); ok {
				if s, ok := item["html_sample"].(string); ok && len(s) > 160 {
					item["html_sample"] = s[:160]
					item["html_sample_truncated"] = true
				}
			}
			entries[i] = e
		}
		nav["structure"] = entries
		if raw, _ = json.Marshal(nav); len(raw) <= navigationBytes {
			return nav
		}
		for len(entries) > 1 {
			entries = entries[:len(entries)/2]
			nav["structure"] = entries
			nav["structure_more"] = true
			if raw, _ = json.Marshal(nav); len(raw) <= navigationBytes {
				return nav
			}
		}
		delete(nav, "structure")
	}
	if raw, _ = json.Marshal(nav); len(raw) <= navigationBytes {
		return nav
	}
	delete(nav, "regions")
	return nav
}

// coreOverBudget reports whether the non-guidance payload, plus the fixed
// receipt envelope, exceeds the core ceiling.
func coreOverBudget(data map[string]any) bool {
	core, _ := splitReadPayload(data)
	raw, err := json.Marshal(core)
	if err != nil {
		return true
	}
	return len(raw)+envelopeAllowance > observationBytes
}
