package aiapp

import "encoding/json"

// One representation owner assigns independent budgets to source prose,
// machine locators and control metadata. A locator is either intact or absent;
// it never turns into a prose preview. SQL keeps the full original.
const (
	observationContentBytes   = 6 << 10
	observationStructureBytes = 4 << 10
	observationControlBytes   = 2 << 10
	readerObservationBytes    = observationContentBytes + observationStructureBytes + observationControlBytes
)

func observationLimit(out ToolResult) int {
	if probeFeedback(out.Name) {
		return probeObservationBytes
	}
	if out.Name == "inspect_source" || sourceReaderName(out.Name) {
		return readerObservationBytes
	}
	return observationBytes
}

func fitObservationText(text string, budget int) string {
	runes := []rune(text)
	lo, hi := 0, len(runes)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		raw, _ := json.Marshal(string(runes[:mid]))
		if len(raw) <= budget {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return string(runes[:lo])
}

func locatedInspectionObservation(out ToolResult) (ToolResult, bool) {
	var original sourceInspection
	if json.Unmarshal(out.Data, &original) != nil || original.Source.Receipt.URL == "" {
		return out, false
	}
	catalog := crawlerCatalog(original.Source.Body, sourceSelectorCatalog(original.Source.Body))
	all := completeSourceSelectorCatalog(original.Source.Body)
	data := map[string]any{"handle": out.CallID, "source": map[string]any{"receipt": original.Source.Receipt}, "view_version": "dom-structure-v1", "selector_catalog": catalog, "selector_menu": catalog, "selector_catalog_complete": len(catalog) == len(all), "total_selectors": len(all), "read_structure_with": map[string]any{"handle": out.CallID, "view": "dom"}, "read_with": map[string]any{"handle": out.CallID, "view": "dom"}, "next_step": "Choose observed selectors and row links; extract complete fields with Lua or submit_extraction. Article text is stored by the host and is not model-readable."}
	if original.JSONStructure != nil {
		data["json_structure"] = crawlerJSONInspection(original.Source.Body)
		nodes := crawlerJSONSourceNodes(original.Source.Body)
		data["nodes"], data["total_nodes"] = nodes, original.JSONStructure.Count
		data["view_version"] = "json-structure-v1"
		data["read_structure_with"] = dataReadArgs{Handle: out.CallID, View: "json"}
		data["read_with"] = dataReadArgs{Handle: out.CallID, View: "json"}
		data["next_step"] = "Use the observed json_pointer and complete short field samples to write Lua/json_decode and field evidence. Open a container pointer for its members; next_read pages JSON members or array entries only. Long text is host-only and has no text cursor."
		if len(nodes) < original.JSONStructure.Count {
			data["next_read"] = dataReadArgs{Handle: out.CallID, View: "json", ViewVersion: "json-structure-v1", Offset: len(nodes), Limit: 64}
		}
		delete(data, "selector_catalog")
		delete(data, "selector_menu")
		delete(data, "selector_catalog_complete")
		delete(data, "total_selectors")
	}
	if len(catalog) < len(all) && original.JSONStructure == nil {
		data["next_read"] = dataReadArgs{Handle: out.CallID, View: "dom", ViewVersion: "dom-structure-v1", Offset: len(catalog)}
	}
	out.Data, _ = json.Marshal(data)
	return out, true
}

// Memory may omit bulk, but it must not re-project an already projected JSON
// tree. These are source/receipt references, not a second copy of the data.
func shortReaderFeedback(out ToolResult) ToolResult {
	if out.Error != "" {
		return observationWithLimit(out, observationControlBytes)
	}
	var data map[string]json.RawMessage
	if json.Unmarshal(out.Data, &data) != nil {
		return out
	}
	short := map[string]json.RawMessage{}
	for _, key := range []string{"handle", "receipt_handle", "view_version", "offset", "total_runes", "next_read", "read_with", "next_step", "next_action", "block_id", "selector", "total_items"} {
		if raw := data[key]; raw != nil {
			short[key] = raw
		}
	}
	var text string
	if json.Unmarshal(data["text"], &text) == nil && text != "" {
		short["text"], _ = json.Marshal(fitObservationText(text, 700))
	}
	out.Data, _ = json.Marshal(short)
	if len(out.Data) > observationControlBytes {
		out.Data, _ = json.Marshal(map[string]any{"receipt_handle": out.CallID, "read_with": map[string]any{"handle": out.CallID, "view": "json"}})
	}
	return out
}
