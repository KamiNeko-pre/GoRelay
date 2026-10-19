package aiapp

import (
	"encoding/json"
	"fmt"
	"sort"
)

// JSON members/array entries are structural nodes, just like DOM matches.
// Samples expose short fields; long text has metadata only and no text cursor.
func crawlerJSONNodes(value any, pointer string, offset, limit int) ([]map[string]any, int) {
	type entry struct {
		path   string
		value  any
		member bool
	}
	entries := []entry{}
	switch v := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			entries = append(entries, entry{pointer + "/" + pointerKey(key), v[key], true})
		}
	case []any:
		for i, item := range v {
			entries = append(entries, entry{fmt.Sprintf("%s/%d", pointer, i), item, false})
		}
	default:
		entries = append(entries, entry{pointer, value, false})
	}
	if limit == 0 {
		limit = 64
	}
	start := min(offset, len(entries))
	end := min(start+limit, len(entries))
	nodes := []map[string]any{}
	for _, e := range entries[start:end] {
		sample, truncated := crawlerJSONSample(e.value, 2)
		node := map[string]any{"json_pointer": e.path, "sample": sample, "sample_truncated": truncated}
		if e.member {
			node["key_locator"] = map[string]any{"json_key_pointer": e.path}
		}
		nodes = append(nodes, node)
	}
	return nodes, len(entries)
}

func crawlerJSONSample(value any, depth int) (any, bool) {
	switch v := value.(type) {
	case string:
		if len([]rune(v)) > 180 {
			return map[string]any{"type": "string", "total_runes": len([]rune(v)), "model_readable": false}, true
		}
	case map[string]any:
		if depth == 0 {
			return map[string]any{"type": "object", "count": len(v)}, true
		}
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		out := map[string]any{}
		truncated := len(keys) > 16
		for _, key := range keys[:min(16, len(keys))] {
			sample, cut := crawlerJSONSample(v[key], depth-1)
			out[key] = sample
			truncated = truncated || cut
		}
		return out, truncated
	case []any:
		if depth == 0 {
			return map[string]any{"type": "array", "count": len(v)}, true
		}
		out := []any{}
		truncated := len(v) > 16
		for _, item := range v[:min(16, len(v))] {
			sample, cut := crawlerJSONSample(item, depth-1)
			out = append(out, sample)
			truncated = truncated || cut
		}
		return out, truncated
	}
	return value, false
}

func crawlerJSONSourceNodes(body string) []map[string]any {
	var value any
	if json.Unmarshal([]byte(body), &value) != nil {
		return nil
	}
	nodes, _ := crawlerJSONNodes(value, "", 0, 64)
	return nodes
}
