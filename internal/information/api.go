package information

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/engine"
)

// APIPlan describes public GET pagination and source-relative JSON locators.
// It contains no literals for requested facts and never executes model code.
type APIPlan struct {
	URL          string            `json:"url"`
	ItemsPointer string            `json:"items_pointer"`
	Fields       map[string]string `json:"fields"`
	NextPointer  string            `json:"next_pointer,omitempty"`
}

func pointer(value any, path string) (any, error) {
	if path == "" {
		return value, nil
	}
	if !strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("JSON pointer must be empty or start with /")
	}
	for _, segment := range strings.Split(path[1:], "/") {
		key := strings.ReplaceAll(strings.ReplaceAll(segment, "~1", "/"), "~0", "~")
		switch v := value.(type) {
		case map[string]any:
			var ok bool
			value, ok = v[key]
			if !ok {
				return nil, fmt.Errorf("JSON pointer %s has no member %q", path, key)
			}
		case []any:
			i, e := strconv.Atoi(key)
			if e != nil || i < 0 || i >= len(v) {
				return nil, fmt.Errorf("JSON pointer %s has no array item %q", path, key)
			}
			value = v[i]
		default:
			return nil, fmt.Errorf("JSON pointer %s crosses a scalar", path)
		}
	}
	return value, nil
}

func executeAPI(ctx context.Context, plan APIPlan, fetch acquisition.FetchFunc) (result engine.ProgramResult, cause error) {
	started := time.Now()
	result.Collection = map[string]any{"items": []any{}}
	result.HTTPUsageKnown = true
	defer func() { result.DurationMS = time.Since(started).Milliseconds() }()
	items := []any{}
	visited := map[string]bool{}
	next := plan.URL
	if len(plan.Fields) == 0 {
		return result, fmt.Errorf("fields must map each requested field to a source JSON pointer")
	}
	for next != "" {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if visited[next] {
			return result, fmt.Errorf("API pagination revisited an already fetched URL; inspect next_pointer and terminal-page semantics")
		}
		visited[next] = true
		response, err := fetch(ctx, http.MethodGet, next)
		result.Stats.Requests++
		result.Stats.Bytes += response.Receipt.Bytes
		result.Requests = append(result.Requests, response.Receipt)
		if err != nil {
			return result, err
		}
		if response.Receipt.Status < 200 || response.Receipt.Status >= 300 {
			return result, fmt.Errorf("API HTTP status %d", response.Receipt.Status)
		}
		result.SourceEvidence = append(result.SourceEvidence, response)
		var value any
		if err := json.Unmarshal([]byte(response.Body), &value); err != nil {
			return result, fmt.Errorf("API response is not JSON: %w; inspect an actual API endpoint before proposing fields", err)
		}
		selected, err := pointer(value, plan.ItemsPointer)
		if err != nil {
			return result, err
		}
		rows, isArray := selected.([]any)
		if !isArray {
			rows = []any{selected}
		}
		for index, row := range rows {
			prefix := plan.ItemsPointer
			if isArray {
				prefix += "/" + strconv.Itoa(index)
			}
			item := map[string]any{"source_url": response.Receipt.URL}
			locators := map[string]any{}
			for name, path := range plan.Fields {
				if name == "source_url" {
					continue
				}
				v, err := pointer(row, path)
				if err != nil {
					return result, fmt.Errorf("field %s: %w", name, err)
				}
				item[name] = v
				locators[name] = map[string]any{"json_pointer": prefix + path}
			}
			item["_evidence"] = locators
			items = append(items, item)
		}
		next = ""
		if plan.NextPointer != "" {
			v, err := pointer(value, plan.NextPointer)
			if err != nil {
				return result, fmt.Errorf("next_pointer: %w", err)
			}
			if v != nil && v != "" {
				ref, ok := v.(string)
				if !ok {
					return result, fmt.Errorf("next_pointer must select the observed next URL string or null")
				}
				base, _ := url.Parse(response.Receipt.URL)
				relative, err := url.Parse(ref)
				if err != nil {
					return result, err
				}
				next = base.ResolveReference(relative).String()
			}
		}
	}
	result.Collection["items"] = items
	return result, nil
}
