package aiapp

import (
	"encoding/json"
	"go-lua-crawler/internal/acquisition"
)

// inspectResponse builds the readable projection of a fetched page.
//
// A page's selector catalog is published here, not only from a later
// read_data(view=dom). Measured across 19 real runs, every detail-page
// inspection returned no catalog at all (13 runs had detail pages; all 13 had
// catalog=0), so a model that needed the page's own structure had to spend a
// whole extra round asking for it — and six of those runs never asked, because
// nothing on the page said the catalog existed. Publishing it with the fetch
// costs one computation the host already performs and removes that round.
func inspectResponse(source acquisition.Response, stats acquisition.Stats, specs ...*TaskSpec) sourceInspection {
	out := sourceInspection{Source: source, ActualStats: stats, TotalRunes: len([]rune(source.Body)), JSONStructure: inspectJSON(source.Body, specs...)}
	if out.JSONStructure == nil {
		out.Structure = inspectDOM(source.Body)
		var err error
		out.Information, err = buildHTMLInformation(source.Body, source.Receipt.URL, "", specs...)
		if err != nil {
			out.ViewError = err.Error()
		}
		out.SelectorMenu = mustCatalogJSON(source.Body)
	}
	return out
}

// An observed schema/sample is navigation metadata, never a delivered record.
// Full bytes remain in the original successful GET receipt.
type JSONInspection struct {
	Root       string                 `json:"root"`
	Count      int                    `json:"count,omitempty"`
	Fields     []JSONFieldObservation `json:"fields"`
	FieldTypes map[string]string      `json:"field_types"`
}
type JSONFieldObservation struct {
	Name     string                 `json:"name"`
	Type     string                 `json:"type"`
	Sample   any                    `json:"sample,omitempty"`
	Count    int                    `json:"count,omitempty"`
	Children []JSONFieldObservation `json:"children,omitempty"`
}

func inspectJSON(body string, specs ...*TaskSpec) *JSONInspection {
	return inspectJSONWithSampleLimit(body, 0, specs...)
}

func inspectJSONWithSampleLimit(body string, sampleLimit int, specs ...*TaskSpec) *JSONInspection {
	var value any
	if json.Unmarshal([]byte(body), &value) != nil {
		return nil
	}
	out := &JSONInspection{}
	switch v := value.(type) {
	case []any:
		out.Root = "array"
		out.Count = len(v)
		if len(v) > 0 {
			remaining := 64
			out.Fields = jsonFields(v[0], 0, &remaining, projectionFields(specs...), sampleLimit)
		}
	case map[string]any:
		out.Root = "object"
		out.Count = len(v)
		remaining := 64
		out.Fields = jsonFields(v, 0, &remaining, projectionFields(specs...), sampleLimit)
	default:
		return nil
	}
	out.FieldTypes = map[string]string{}
	for _, f := range out.Fields {
		out.FieldTypes[f.Name] = f.Type
	}
	return out
}
func jsonFields(value any, depth int, remaining *int, priority map[string]bool, sampleLimit int) []JSONFieldObservation {
	if *remaining <= 0 {
		return nil
	}
	object, ok := value.(map[string]any)
	if !ok {
		*remaining--
		return []JSONFieldObservation{jsonField("$", value, depth, remaining, priority, sampleLimit)}
	}
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sortProjectionKeys(keys, priority)
	out := []JSONFieldObservation{}
	count := min(32, len(keys), *remaining)
	*remaining -= count // Reserve siblings before expanding any nested metadata.
	for _, key := range keys[:count] {
		out = append(out, jsonField(key, object[key], depth, remaining, priority, sampleLimit))
	}
	return out
}
func jsonField(name string, value any, depth int, remaining *int, priority map[string]bool, sampleLimit int) JSONFieldObservation {
	out := JSONFieldObservation{Name: name}
	switch v := value.(type) {
	case nil:
		out.Type = "null"
	case bool:
		out.Type = "boolean"
		out.Sample = v
	case float64:
		out.Type = "number"
		out.Sample = v
	case string:
		out.Type = "string"
		sample := []rune(v)
		limit := 80
		if depth > 0 {
			limit = 40
		}
		if sampleLimit > 0 {
			limit = sampleLimit
		}
		if len(sample) > limit {
			sample = sample[:limit]
		}
		out.Sample = string(sample)
	case []any:
		out.Type = "array"
		out.Count = len(v)
		if depth < 3 && len(v) > 0 {
			out.Children = jsonFields(v[0], depth+1, remaining, priority, sampleLimit)
		}
	case map[string]any:
		out.Type = "object"
		out.Count = len(v)
		if depth < 3 {
			out.Children = jsonFields(v, depth+1, remaining, priority, sampleLimit)
		}
	}
	return out
}
