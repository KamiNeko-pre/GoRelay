package information

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
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
	Method       string            `json:"method,omitempty"`
	Headers      map[string]string `json:"headers,omitempty"`
	Body         string            `json:"body,omitempty"`
	ItemsPointer string            `json:"items_pointer"`
	Fields       map[string]string `json:"fields"`
	NextPointer  string            `json:"next_pointer,omitempty"`
}

func normalizeJSONPointer(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	if !strings.HasPrefix(raw, "/") {
		if strings.ContainsAny(raw, "\r\n\t ") {
			return "", fmt.Errorf("JSON pointer %q must be empty or start with /", raw)
		}
		return "/" + raw, nil
	}
	return raw, nil
}

func joinJSONPointer(prefix, suffix string) string {
	normPrefix, err1 := normalizeJSONPointer(prefix)
	normSuffix, err2 := normalizeJSONPointer(suffix)
	if err1 != nil {
		normPrefix = prefix
	}
	if err2 != nil {
		normSuffix = suffix
	}
	if normSuffix == "" {
		return normPrefix
	}
	return normPrefix + normSuffix
}

func normalizeAPIPlan(plan APIPlan) (APIPlan, error) {
	plan.URL = strings.TrimSpace(plan.URL)
	var err error
	if plan.ItemsPointer, err = normalizeJSONPointer(plan.ItemsPointer); err != nil {
		return plan, err
	}
	if plan.NextPointer, err = normalizeJSONPointer(plan.NextPointer); err != nil {
		return plan, err
	}
	if plan.Fields != nil {
		normFields := make(map[string]string, len(plan.Fields))
		for k, v := range plan.Fields {
			np, err := normalizeJSONPointer(v)
			if err != nil {
				return plan, fmt.Errorf("field %s: %w", k, err)
			}
			normFields[k] = np
		}
		plan.Fields = normFields
	}
	return plan, nil
}

func pointer(value any, path string) (any, error) {
	path, err := normalizeJSONPointer(path)
	if err != nil {
		return nil, err
	}
	if path == "" {
		return value, nil
	}
	for _, segment := range strings.Split(path[1:], "/") {
		key := strings.ReplaceAll(strings.ReplaceAll(segment, "~1", "/"), "~0", "~")
		switch v := value.(type) {
		case map[string]any:
			var ok bool
			value, ok = v[key]
			if !ok {
				keys := make([]string, 0, len(v))
				for k := range v {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				if len(keys) > 16 {
					keys = append(keys[:16], "...")
				}
				return nil, fmt.Errorf("JSON pointer %s has no member %q (available keys: [%s])", path, key, strings.Join(keys, ", "))
			}
		case []any:
			i, e := strconv.Atoi(key)
			if e != nil || i < 0 || i >= len(v) {
				return nil, fmt.Errorf("JSON pointer %s crosses array (len=%d) with invalid index %q; put array path in items_pointer and extract fields from each item", path, len(v), key)
			}
			value = v[i]
		default:
			return nil, fmt.Errorf("JSON pointer %s crosses a scalar (%T)", path, v)
		}
	}
	return value, nil
}

// apiItemEvidence is created by the native extractor, not supplied by the model.
type apiItemEvidence struct {
	ResponseIndex int               `json:"response_index"`
	Pointers      map[string]string `json:"pointers"`
}

func extractAPIItems(value any, plan APIPlan, sourceURL string, responseIndex int) ([]any, []error, string, error) {
	selected, err := pointer(value, plan.ItemsPointer)
	if err != nil {
		return nil, nil, "", err
	}
	rows, isArray := selected.([]any)
	if !isArray {
		rows = []any{selected}
	}
	items := make([]any, 0, len(rows))
	var fieldErrors []error
	fieldNames := make([]string, 0, len(plan.Fields))
	for name := range plan.Fields {
		fieldNames = append(fieldNames, name)
	}
	sort.Strings(fieldNames)
	for index, row := range rows {
		prefix := plan.ItemsPointer
		if isArray {
			prefix += "/" + strconv.Itoa(index)
		}
		item := map[string]any{"source_url": sourceURL}
		locators := map[string]string{}
		for _, name := range fieldNames {
			if name == "source_url" {
				continue
			}
			path := plan.Fields[name]
			v, err := pointer(row, path)
			if err != nil {
				fieldErrors = append(fieldErrors, fmt.Errorf("response %s item %d field %s: %w", sourceURL, index, name, err))
				continue
			}
			item[name] = v
			locators[name] = joinJSONPointer(prefix, path)
		}
		if len(locators) > 0 {
			item["_evidence"] = apiItemEvidence{ResponseIndex: responseIndex, Pointers: locators}
			items = append(items, item)
		}
	}
	next := ""
	if plan.NextPointer != "" {
		v, err := pointer(value, plan.NextPointer)
		if err != nil {
			return items, fieldErrors, "", fmt.Errorf("next_pointer: %w", err)
		}
		if v != nil && v != "" {
			ref, ok := v.(string)
			if !ok {
				return items, fieldErrors, "", fmt.Errorf("next_pointer must select the observed next URL string or null")
			}
			base, _ := url.Parse(sourceURL)
			relative, err := url.Parse(ref)
			if err != nil {
				return items, fieldErrors, "", err
			}
			if base != nil {
				next = base.ResolveReference(relative).String()
			} else {
				next = ref
			}
		}
	}
	return items, fieldErrors, next, nil
}

func executeAPIWithRequester(ctx context.Context, plan APIPlan, request func(context.Context, acquisition.RequestOptions) (acquisition.Response, error)) (result engine.ProgramResult, cause error) {
	started := time.Now()
	result.Collection = map[string]any{"items": []any{}}
	result.HTTPUsageKnown = true
	items := []any{}
	var fieldErrors []error
	matchedFields := map[string]bool{}
	defer func() {
		result.DurationMS = time.Since(started).Milliseconds()
		result.Collection["items"] = items
		// Ignore sparse item-level field misses as long as every declared field matched at least once;
		// fail when zero items matched or a declared field matched nothing across all pages.
		if cause == nil && len(fieldErrors) > 0 {
			missingEverywhere := len(items) == 0
			for name := range plan.Fields {
				if name != "source_url" && !matchedFields[name] {
					missingEverywhere = true
					break
				}
			}
			if missingEverywhere {
				cause = errors.Join(fieldErrors...)
			}
		}
	}()
	normalizedPlan, err := normalizeAPIPlan(plan)
	if err != nil {
		return result, err
	}
	plan = normalizedPlan
	visited := map[string]bool{}
	next := plan.URL
	method := strings.ToUpper(strings.TrimSpace(plan.Method))
	if method == "" {
		method = http.MethodGet
	}
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
		opts := acquisition.RequestOptions{Method: method, URL: next, Headers: plan.Headers, Body: plan.Body}
		response, err := request(ctx, opts)
		result.Stats.Requests++
		result.Stats.Bytes += response.Receipt.Bytes
		result.Requests = append(result.Requests, response.Receipt)
		var responseIndex int
		if response.Receipt.URL != "" && !response.Receipt.FetchedAt.IsZero() && (err == nil || response.Receipt.Status >= 400) {
			result.SourceEvidence = append(result.SourceEvidence, response)
			responseIndex = len(result.SourceEvidence)
		}
		if err != nil {
			return result, err
		}
		if response.Receipt.Status < 200 || response.Receipt.Status >= 300 {
			return result, fmt.Errorf("API HTTP status %d", response.Receipt.Status)
		}
		var value any
		decoder := json.NewDecoder(strings.NewReader(response.Body))
		decoder.UseNumber()
		if err := decoder.Decode(&value); err != nil {
			return result, fmt.Errorf("API response is not JSON: %w; inspect an actual API endpoint or use run_lua", err)
		}
		if decoder.Decode(&struct{}{}) != io.EOF {
			return result, fmt.Errorf("API response is not a single JSON document; inspect an actual API endpoint or use run_lua")
		}
		pageItems, pageFieldErrs, nextPage, err := extractAPIItems(value, plan, response.Receipt.URL, responseIndex)
		for _, rawItem := range pageItems {
			if m, ok := rawItem.(map[string]any); ok {
				for k, v := range m {
					if k != "_evidence" && k != "source_url" && v != nil {
						matchedFields[k] = true
					}
				}
			}
		}
		items = append(items, pageItems...)
		fieldErrors = append(fieldErrors, pageFieldErrs...)
		if err != nil {
			return result, err
		}
		next = nextPage
	}
	return result, nil
}
