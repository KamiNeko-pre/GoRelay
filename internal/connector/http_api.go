package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"gorelay/internal/acquisition"
	"gorelay/internal/engine"
	"gorelay/internal/fetchgov"
	"gorelay/internal/pipeline"
)

type apiCursorState struct {
	Page       int    `json:"page,omitempty"`
	Offset     int    `json:"offset,omitempty"`
	Cursor     string `json:"cursor,omitempty"`
	PagesDone  int    `json:"pages_done,omitempty"`
}

// ExecuteHTTPAPI runs a declarative APISpec through fetchgov.SafeClient with
// cycle detection and optional checkpoint.get/save resumption.
func ExecuteHTTPAPI(
	ctx context.Context,
	client *fetchgov.SafeClient,
	spec APISpec,
	params map[string]any,
	authProfile string,
	maxPagesOverride int,
	cp *engine.CheckpointHooks,
) ([]map[string]any, []acquisition.Response, error) {
	if client == nil {
		return nil, nil, fmt.Errorf("safe client is required for http_api execution")
	}
	if strings.TrimSpace(spec.URLTemplate) == "" {
		return nil, nil, fmt.Errorf("api_spec.url_template is required")
	}
	method := strings.ToUpper(strings.TrimSpace(spec.Method))
	if method == "" {
		method = http.MethodGet
	}
	maxPages := spec.MaxPages
	if maxPages <= 0 {
		maxPages = 5
	}
	if maxPages > 20 {
		maxPages = 20
	}
	if maxPagesOverride > 0 && maxPagesOverride < maxPages {
		maxPages = maxPagesOverride
	}

	state := apiCursorState{Page: 1}
	if cp != nil && cp.Get != nil {
		rawCP, err := cp.Get(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("load api checkpoint: %w", err)
		}
		if rawCP != nil {
			encoded, _ := json.Marshal(rawCP)
			_ = json.Unmarshal(encoded, &state)
			if state.Page <= 0 {
				state.Page = 1
			}
		}
	} else if params != nil {
		if p, ok := asPositiveInt(params["page"]); ok {
			state.Page = p
		}
		if o, ok := asPositiveInt(params["offset"]); ok {
			state.Offset = o
		}
		if c, ok := params["cursor"].(string); ok && strings.TrimSpace(c) != "" {
			state.Cursor = strings.TrimSpace(c)
		}
	}

	remainingPages := maxPages
	if state.PagesDone > 0 && maxPagesOverride == 0 {
		remainingPages = maxPages - state.PagesDone
		if remainingPages <= 0 {
			return nil, nil, nil
		}
	}

	urlGuard := pipeline.NewJobURLSet(time.Hour, 500)
	var allRows []map[string]any
	var allEvidence []acquisition.Response

	for pageIter := 0; pageIter < remainingPages; pageIter++ {
		if err := ctx.Err(); err != nil {
			return allRows, allEvidence, err
		}
		targetURL, err := buildAPIURL(spec, params, state)
		if err != nil {
			return allRows, allEvidence, err
		}
		if !urlGuard.MarkSeen(targetURL) {
			break
		}

		resp, err := client.Fetch(ctx, fetchgov.FetchRequest{
			URL:         targetURL,
			Method:      method,
			Headers:     spec.Headers,
			Body:        expandTemplate(spec.BodyTemplate, params, state, spec.Pagination, false),
			AuthProfile: authProfile,
		})
		if err != nil {
			return allRows, allEvidence, err
		}
		allEvidence = append(allEvidence, resp)
		if resp.Receipt.Status < 200 || resp.Receipt.Status >= 300 {
			return allRows, allEvidence, fmt.Errorf("http_api status %d on %s", resp.Receipt.Status, targetURL)
		}

		dec := json.NewDecoder(strings.NewReader(resp.Body))
		dec.UseNumber()
		var root any
		if err := dec.Decode(&root); err != nil {
			return allRows, allEvidence, fmt.Errorf("http_api invalid JSON from %s: %w", targetURL, err)
		}

		pageRows, err := extractAPIRows(root, spec, targetURL)
		if err != nil {
			return allRows, allEvidence, err
		}
		allRows = append(allRows, pageRows...)

		nextState, hasMore := computeNextCursor(root, spec, state, len(pageRows))
		if cp != nil && cp.Save != nil {
			var nextCursor any
			if hasMore {
				nextCursor = nextState
			}
			if err := cp.Save(ctx, nextCursor, pageRows, []acquisition.Response{resp}); err != nil {
				return allRows, allEvidence, err
			}
		}
		if !hasMore || len(pageRows) == 0 {
			break
		}
		state = nextState
	}
	return allRows, allEvidence, nil
}

func buildAPIURL(spec APISpec, params map[string]any, state apiCursorState) (string, error) {
	expanded := expandTemplate(spec.URLTemplate, params, state, spec.Pagination, true)
	u, err := url.Parse(expanded)
	if err != nil {
		return "", fmt.Errorf("invalid expanded url_template: %w", err)
	}
	pagMode := strings.ToLower(strings.TrimSpace(spec.Pagination))
	if pagMode == "" || pagMode == "none" {
		return u.String(), nil
	}
	q := u.Query()
	paramName := spec.PageParam
	switch pagMode {
	case "page":
		if paramName == "" {
			paramName = "page"
		}
		if !strings.Contains(spec.URLTemplate, "{page}") {
			q.Set(paramName, strconv.Itoa(state.Page))
		}
	case "offset":
		if paramName == "" {
			paramName = "offset"
		}
		if !strings.Contains(spec.URLTemplate, "{offset}") {
			q.Set(paramName, strconv.Itoa(state.Offset))
		}
	case "cursor":
		if paramName == "" {
			paramName = "cursor"
		}
		if state.Cursor != "" && !strings.Contains(spec.URLTemplate, "{cursor}") {
			q.Set(paramName, state.Cursor)
		}
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func expandTemplate(tpl string, params map[string]any, state apiCursorState, pagMode string, escapeQuery bool) string {
	if tpl == "" {
		return ""
	}
	out := tpl
	val := func(raw string) string {
		if escapeQuery {
			return url.QueryEscape(raw)
		}
		return raw
	}
	// Only let declarative pagination state own {page}/{offset}/{cursor} when
	// pagination is active; when pagination is "none", caller params own them.
	switch strings.ToLower(strings.TrimSpace(pagMode)) {
	case "page":
		out = strings.ReplaceAll(out, "{page}", val(strconv.Itoa(state.Page)))
	case "offset":
		out = strings.ReplaceAll(out, "{offset}", val(strconv.Itoa(state.Offset)))
	case "cursor":
		out = strings.ReplaceAll(out, "{cursor}", val(state.Cursor))
	}
	for k, v := range params {
		out = strings.ReplaceAll(out, "{"+k+"}", val(fmt.Sprint(v)))
	}
	return out
}

func asPositiveInt(v any) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, x > 0
	case int64:
		return int(x), x > 0
	case float64:
		return int(x), x > 0 && float64(int(x)) == x
	case json.Number:
		if n, err := x.Int64(); err == nil && n > 0 {
			return int(n), true
		}
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(x)); err == nil && n > 0 {
			return n, true
		}
	}
	return 0, false
}

func extractAPIRows(root any, spec APISpec, sourceURL string) ([]map[string]any, error) {
	target := root
	if spec.ItemsPath != "" {
		var ok bool
		target, ok = evalJSONPointer(root, spec.ItemsPath)
		if !ok {
			return nil, fmt.Errorf("items_path %q not found in API JSON", spec.ItemsPath)
		}
	}
	var items []any
	switch v := target.(type) {
	case []any:
		items = v
	case map[string]any:
		items = []any{v}
	default:
		return nil, fmt.Errorf("items_path %q resolved to %T, expected array or object", spec.ItemsPath, target)
	}

	out := make([]map[string]any, 0, len(items))
	for _, entry := range items {
		obj, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		row := make(map[string]any)
		if len(spec.FieldMapping) == 0 {
			for k, v := range obj {
				row[k] = v
			}
		} else {
			for targetField, ptr := range spec.FieldMapping {
				if val, exists := evalJSONPointer(obj, ptr); exists {
					row[targetField] = val
				}
			}
		}
		if _, hasURL := row["source_url"]; !hasURL {
			row["source_url"] = sourceURL
		}
		out = append(out, row)
	}
	return out, nil
}

func computeNextCursor(root any, spec APISpec, cur apiCursorState, gotRows int) (apiCursorState, bool) {
	mode := strings.ToLower(strings.TrimSpace(spec.Pagination))
	if mode == "" || mode == "none" || gotRows == 0 {
		return apiCursorState{}, false
	}
	next := cur
	next.PagesDone++
	switch mode {
	case "page":
		if spec.NextCursorPath != "" {
			v, ok := evalJSONPointer(root, spec.NextCursorPath)
			if !ok || v == nil || v == "" || v == false {
				return apiCursorState{}, false
			}
		}
		next.Page = cur.Page + 1
		return next, true
	case "offset":
		next.Offset = cur.Offset + gotRows
		return next, true
	case "cursor":
		if spec.NextCursorPath == "" {
			return apiCursorState{}, false
		}
		val, ok := evalJSONPointer(root, spec.NextCursorPath)
		if !ok || val == nil {
			return apiCursorState{}, false
		}
		s := strings.TrimSpace(fmt.Sprint(val))
		if s == "" || s == "<nil>" {
			return apiCursorState{}, false
		}
		next.Cursor = s
		return next, true
	default:
		return apiCursorState{}, false
	}
}

func evalJSONPointer(node any, ptr string) (any, bool) {
	if ptr == "" || ptr == "/" {
		return node, true
	}
	path := ptr
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	cur := node
	for _, raw := range strings.Split(path[1:], "/") {
		key := strings.ReplaceAll(strings.ReplaceAll(raw, "~1", "/"), "~0", "~")
		switch typed := cur.(type) {
		case map[string]any:
			val, ok := typed[key]
			if !ok {
				return nil, false
			}
			cur = val
		case []any:
			idx, err := strconv.Atoi(key)
			if err != nil || idx < 0 || idx >= len(typed) {
				return nil, false
			}
			cur = typed[idx]
		default:
			return nil, false
		}
	}
	return cur, true
}
