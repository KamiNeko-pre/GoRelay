package information

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/knowledge"
	"gorm.io/gorm"
)

type informationAcquireArgs struct {
	URLs           []string            `json:"urls,omitempty"`
	InspectURL     string              `json:"inspect_url,omitempty"`
	InspectURLs    []string            `json:"inspect_urls,omitempty"`
	ExcerptQueries []string            `json:"excerpt_queries,omitempty"`
	API            *APIPlan            `json:"api,omitempty"`
	AssetID        string              `json:"asset_id,omitempty"`
	URL            string              `json:"url,omitempty"`
	Method         string              `json:"method,omitempty"`
	Headers        map[string]string   `json:"headers,omitempty"`
	Body           string              `json:"body,omitempty"`
	Params         map[string]any      `json:"params,omitempty"`
	Acquisition    *AcquisitionRequest `json:"acquisition,omitempty"`
	Description    string              `json:"description,omitempty"`
	ResearchDepth  string              `json:"research_depth,omitempty"`
	ResearchTrack  string              `json:"research_track,omitempty"`
	Reason         string              `json:"reason,omitempty"`
}

func (h *informationHost) acquire(ctx context.Context, raw string, step Step) (any, *informationWait, error) {
	var args informationAcquireArgs
	if err := decodeInformationArgs(raw, &args); err != nil {
		return nil, nil, err
	}
	r := &h.request
	args.ExcerptQueries = nil // The outer receipt controls presentation of the same acquisition.
	if args.AssetID != "" {
		return h.executeAssetCall(ctx, args, step)
	}
	if len(args.InspectURLs) > 0 || args.InspectURL != "" {
		urls, single := args.InspectURLs, len(args.InspectURLs) == 0
		if single {
			urls = []string{args.InspectURL}
		}
		result, err := h.observeBatch(ctx, urls, single)
		return result, nil, err
	}
	if args.API != nil {
		fieldNames := make([]string, 0, len(args.API.Fields))
		for k := range args.API.Fields {
			fieldNames = append(fieldNames, k)
		}
		acqReq := ensureAcquisitionFromFieldNames(args.Acquisition, h.input.Goal, fieldNames)
		var err error
		ctx, err = withLocalAcquisition(ctx, acqReq)
		if err != nil {
			return nil, nil, err
		}
		trial, err := h.service.TrialAPI(ctx, r.ID, args.Description, *args.API)
		if err != nil {
			return trial, nil, err
		}
		return acquisitionTrialSummary(trial), nil, nil
	}

	urls := append([]string{}, args.URLs...)
	if len(urls) == 0 && strings.TrimSpace(args.URL) != "" {
		urls = append(urls, strings.TrimSpace(args.URL))
	}
	if len(urls) == 0 {
		return nil, nil, actionFailure("SOURCE_URLS_REQUIRED", fmt.Errorf("acquire_sources requires urls, url, inspect_url, api, or asset_id"), "Provide urls=[...], api={...}, or use run_lua / discover_sources.")
	}
	for i, u := range urls {
		u = strings.TrimSpace(u)
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			u = "https://" + u
		}
		urls[i] = u
		if !urlInScope(u, h.input.SourceScope) {
			return nil, nil, actionFailure("ASSET_SCOPE_INVALID", fmt.Errorf("source URL is outside scope or malformed"), "Use observed public HTTP(S) source URLs inside the original scope.")
		}
	}
	fetcher, err := h.service.NewFetcher(h.input.SourceScope)
	if err != nil {
		return nil, nil, actionFailure("ACQUISITION_FETCHER_UNAVAILABLE", err, "Use inspect_url, browser_extract, or verify source scope.")
	}
	defer fetcher.Close()

	execution, fresh, err := h.service.reserveStep(ctx, *r, "capture_sources", args)
	if err != nil {
		return nil, nil, err
	}
	if !fresh {
		if execution.ResultJSON == "" {
			return nil, nil, ErrUnknownStep
		}
		return validRaw(execution.ResultJSON), nil, restoredDomainFailure(execution)
	}
	method := strings.ToUpper(strings.TrimSpace(args.Method))
	if method == "" {
		method = http.MethodGet
	}
	options := make([]acquisition.RequestOptions, len(urls))
	for i, target := range urls {
		options[i] = acquisition.RequestOptions{Method: method, URL: target, Headers: args.Headers, Body: args.Body}
	}
	snapshots, err := h.snapshotsForRequests(ctx, fetcher, options)
	if err != nil {
		return nil, nil, err
	}

	type fetchSlot struct {
		resp acquisition.Response
		err  error
	}
	fetchedSlots := make([]fetchSlot, len(urls))
	runOne := func(opts acquisition.RequestOptions) fetchSlot {
		resp, fErr := fetcher.FetchWithSnapshots(ctx, opts, snapshots.responses)
		if fErr == nil && (resp.Receipt.Status < 200 || resp.Receipt.Status >= 300) {
			fErr = fmt.Errorf("source HTTP status %d", resp.Receipt.Status)
		}
		if reason, blocked := browserSourceDiagnosis(resp); fErr == nil && blocked {
			fErr = errors.New(reason)
		}
		return fetchSlot{resp: resp, err: fErr}
	}
	var wg sync.WaitGroup
	for i, opts := range options {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fetchedSlots[i] = runOne(opts)
		}()
	}
	wg.Wait()

	successes := 0
	var rows []aiapp.Record
	var allCaptures []acquisition.Response
	savedCaptures := map[int]knowledge.Document{}
	var fetchErr error
	var browserApproved []string
	sourceStatuses := make([]map[string]any, 0, len(urls))
	entriesByCapture := make(map[int]map[string]any, len(urls))
	for i, u := range urls {
		resp, fErr := fetchedSlots[i].resp, fetchedSlots[i].err
		entry := map[string]any{"url": u}
		sourceStatuses = append(sourceStatuses, entry)
		if resp.Receipt.URL != "" && !resp.Receipt.FetchedAt.IsZero() {
			entriesByCapture[len(allCaptures)] = entry
			snapshots.bind(resp, savedCaptures, len(allCaptures))
			allCaptures = append(allCaptures, resp)
		}
		if fErr != nil {
			fetchErr = fErr
			entry["error"] = fErr.Error()
			if resp.Receipt.Status > 0 {
				entry["status"] = resp.Receipt.Status
				entry["headers"] = resp.Receipt.DiagnosticHeaders()
				addRateLimitAdvice(entry, resp.Receipt)
			}
			if approvedURL, ok := h.service.maybeApproveBrowserFromFetch(ctx, r, step, h.input.SourceScope, resp); ok {
				browserApproved = append(browserApproved, approvedURL)
				entry["browser_approved"] = true
			}
			continue
		}
		entry["url"] = resp.Receipt.URL
		entry["status"] = resp.Receipt.Status
		if resp.Receipt.ReusedSnapshot {
			entry["reused_snapshot"] = true
		}
		extracted := extractJSONRecordsFromResponses([]acquisition.Response{resp})
		entry["record_count"] = len(extracted)
		if len(extracted) == 0 && strings.TrimSpace(resp.Body) != "" {
			metadata := acquisitionResponseMetadata(resp)
			if metadata["is_json"] == true {
				entry["json_keys"] = metadata["json_keys"]
				entry["json_paths"] = metadata["json_paths"]
				entry["next_step"] = "Use an observed array pointer for api.items_pointer and its sample_item_paths for api.fields, or extract custom structures from the saved source_ref with run_lua."
			} else if metadata["is_html"] == true || metadata["is_xml"] == true {
				if outline, ok := metadata["html_outline"]; ok {
					entry["html_outline"] = outline
				}
				if outline, ok := metadata["xml_outline"]; ok {
					entry["xml_outline"] = outline
				}
				entry["next_step"] = snapshotLinkAdvice
			} else {
				entry["next_step"] = "Response captured. Extract structured items from the saved source_ref with run_lua."
			}
		}
		rows = append(rows, extracted...)
		successes++
	}

	out := map[string]any{"status": "captured", "sources": sourceStatuses}
	if len(browserApproved) > 0 {
		out["browser_approved_urls"] = browserApproved
	}
	var cause error
	if successes == 0 && fetchErr != nil {
		out["status"] = "failed"
		failure := actionFailure("ACQUISITION_FETCH_FAILED", fetchErr, "Inspect HTTP status/headers to adjust method or headers, use run_lua for custom requests/parsing, or switch to an alternate source.")
		failure.Details = map[string]any{"sources": sourceStatuses, "browser_approved_urls": browserApproved}
		if len(browserApproved) > 0 {
			failure.Steps = []string{"Use browser_extract for one of the approved URLs, or continue with another applicable source."}
		}
		cause = failure
	} else if fetchErr != nil {
		out["status"] = "partial"
	}
	if err := h.commitAcquisition(ctx, execution, rows, allCaptures, out, cause, savedCaptures, entriesByCapture); err != nil {
		return nil, nil, err
	}
	return out, nil, cause
}

const snapshotLinkAdvice = "Extract from this saved snapshot with run_lua(source_ref=...). For missing facts on linked pages, acquire the exact related_links together; use discovery only if no applicable entry is known."

func addRateLimitAdvice(entry map[string]any, receipt acquisition.Receipt) {
	if receipt.Status == http.StatusTooManyRequests {
		if retryAfter := receipt.Headers["retry-after"]; retryAfter != "" {
			entry["retry_after"] = retryAfter
		}
		entry["next_step"] = "The source is rate-limiting requests. Honor Retry-After when provided, reduce request frequency, or continue with an independent source. Changing headers or using Browser does not resolve a rate limit."
	}
}

func (h *informationHost) discover(ctx context.Context, raw string, step Step) (any, *informationWait, error) {
	var args struct {
		Query     string   `json:"query"`
		Count     int      `json:"count"`
		URLs      []string `json:"urls"`
		SavedOnly bool     `json:"saved_only"`
		Offset    int      `json:"offset"`
		Limit     int      `json:"limit"`
	}
	if err := decodeInformationArgs(raw, &args); err != nil {
		return nil, nil, err
	}
	if args.SavedOnly {
		sources, err := h.service.knownSources(ctx, h.input.SourceScope)
		if err != nil {
			return nil, nil, err
		}
		page, err := sourceDirectoryPage(sources, args.Query, args.Offset, args.Limit)
		return page, nil, err
	}
	if len(args.URLs) > 0 {
		return h.acquire(ctx, string(mustMarshal(informationAcquireArgs{URLs: args.URLs})), step)
	}
	if strings.TrimSpace(args.Query) == "" {
		return nil, nil, actionFailure("ROUTING_ARGUMENTS_INVALID", fmt.Errorf("query is required"), "Provide a focused query for source candidates, or use saved_only to browse saved sources.")
	}
	if h.request.Purpose == "source_discovery" {
		if h.service.Search == nil {
			return nil, nil, actionFailure("SEARCH_PROVIDER_UNAVAILABLE", fmt.Errorf("no search provider is configured"), "Use discover_sources(saved_only=true) for saved entries and use acquire_sources or run_lua on public search pages, indexes or directories. Verify the candidate URLs before submitting.")
		}
		if args.Count < 1 {
			return nil, nil, actionFailure("SEARCH_COUNT_REQUIRED", fmt.Errorf("the configured search provider needs a result count"), "Pass count based on the task's current source gaps, or use a verified saved entry.")
		}
		execution, fresh, err := h.service.reserveStep(ctx, h.request, "search_sources", args)
		if err != nil {
			return nil, nil, err
		}
		if !fresh {
			if execution.ResultJSON == "" {
				return nil, nil, ErrUnknownStep
			}
			return validRaw(execution.ResultJSON), nil, restoredDomainFailure(execution)
		}
		result, err := h.service.Search.Search(ctx, args.Query, args.Count)
		var cause error
		if err != nil {
			cause = actionFailure("SEARCH_PROVIDER_FAILED", err, "Use saved sources or verify public directory pages with acquire_sources or run_lua.")
		}
		candidates := []aiapp.SearchCandidate{}
		for _, candidate := range result.Candidates {
			normal, err := sourceIdentity(candidate.URL)
			if err == nil && urlInScope(normal, h.input.SourceScope) {
				candidate.URL = normal
				candidates = append(candidates, candidate)
			}
		}
		out := map[string]any{"candidates": candidates, "verified": false}
		if err := h.commitAcquisition(ctx, execution, nil, nil, out, cause, nil); err != nil {
			return nil, nil, err
		}
		return out, nil, cause
	}

	var link discoveryChildLink
	if step.ExecutionKind == "source_discovery" && step.ExecutionResultJSON != "" {
		if err := json.Unmarshal([]byte(step.ExecutionResultJSON), &link); err != nil {
			return nil, nil, err
		}
	} else {
		err := h.owned(ctx, func(tx *gorm.DB) error {
			scoped := *h.service
			scoped.DB = tx
			childInput := Input{
				RequestKey:  "discovery:" + h.owner.RequestID + ":" + agentdata.Hash([]any{args.Query, step.ID}),
				Goal:        discoverySubtaskGoal(args.Query),
				SourceScope: h.input.SourceScope,
				Refresh:     true,
			}
			if !h.request.Deadline.IsZero() {
				rem := max(int64(1), int64(time.Until(h.request.Deadline).Seconds()))
				childInput.TimeoutSeconds = &rem
			}
			child, err := scoped.create(ctx, childInput, "source_discovery")
			if err != nil {
				return err
			}
			link.ChildRequestID = child.ID
			return tx.Model(&Step{}).Where("id = ? AND request_id = ?", step.ID, step.RequestID).Updates(map[string]any{
				"execution_kind": "source_discovery", "execution_result_json": string(mustMarshal(link)),
			}).Error
		})
		if err != nil {
			return nil, nil, err
		}
	}
	sub, err := h.service.Request(ctx, link.ChildRequestID)
	if err != nil {
		return nil, nil, err
	}
	dep := discoveryDependency(sub)
	if !dep.Terminal {
		return link, &informationWait{Stage: "discover_sources", Reason: "Await independent source discovery.", Dependencies: []informationDependency{dep}}, nil
	}
	if sub.CurrentDeliveryID == "" {
		return nil, nil, actionFailure("SOURCE_DISCOVERY_FAILED", fmt.Errorf("discovery %s ended with %s: %s", sub.ID, sub.Status, sub.LastError), "Narrow the search to the missing source type, use known sources, or report the remaining source gap.")
	}
	_, draft, err := h.service.SavedDelivery(ctx, sub.CurrentDeliveryID)
	if err != nil {
		return nil, nil, err
	}
	candidates := make([]aiapp.SearchCandidate, 0, len(draft.DiscoveredSources))
	for _, u := range draft.DiscoveredSources {
		candidates = append(candidates, aiapp.SearchCandidate{URL: u})
	}
	if len(candidates) > 0 {
		if err := h.service.rememberSources(ctx, h.owner.RequestID, args.Query, candidates, true); err != nil {
			return nil, nil, err
		}
	}
	verified, err := h.attachDiscoverySources(ctx, step, sub.ID, draft)
	if err != nil {
		return nil, nil, err
	}
	return map[string]any{"child_request_id": sub.ID, "sources": draft.DiscoveredSources, "verified_sources": verified, "outcome": draft.Outcome, "gaps": draft.Gaps}, nil, nil
}

func (h *informationHost) executeAssetCall(ctx context.Context, args informationAcquireArgs, step Step) (any, *informationWait, error) {
	var asset Asset
	if err := h.service.DB.WithContext(ctx).First(&asset, "id = ? AND active = ?", args.AssetID, true).Error; err != nil {
		return nil, nil, actionFailure("ASSET_NOT_ELIGIBLE", err, "Choose an active asset ID from read_materials(kind='assets') or a successful trial.")
	}
	ctx, err := withAssetAcquisition(ctx, asset)
	if err != nil {
		return nil, nil, err
	}
	if asset.Kind == "lua" {
		return nil, nil, actionFailure("ASSET_NOT_ELIGIBLE", fmt.Errorf("use run_lua to execute Lua scripts directly in sandbox"), "Call run_lua(code=..., targets=[...]).")
	}
	invocation := assetInvocation{URL: args.URL, Params: args.Params}
	effective, err := nativeAssetForExecution(h.input, asset, invocation)
	if err != nil {
		return nil, nil, err
	}
	rows, err := h.service.ExecuteAsset(ctx, h.request.ID, effective.ID, invocation)
	return map[string]any{"asset_id": effective.ID, "records": rows, "record_count": len(rows)}, nil, err
}

func (s *Service) informationDependency(ctx context.Context, kind, id string) (informationDependency, error) {
	d := informationDependency{Kind: kind, ID: id}
	if kind != "information" {
		return d, fmt.Errorf("unknown information dependency kind %s", kind)
	}
	row, err := s.Request(ctx, id)
	if err != nil {
		return d, err
	}
	return discoveryDependency(row), nil
}

func discoveryDependency(row Request) informationDependency {
	return informationDependency{
		Kind: "information", ID: row.ID,
		Version:  agentdata.Hash([]any{row.Status, row.Stage, row.CurrentDeliveryID}),
		Terminal: terminal(row.Status) || row.Status == "needs_input" || strings.HasPrefix(row.Stage, "blocked_"),
	}
}

func (s *Service) informationReady(ctx context.Context, r Request) (bool, error) {
	if r.Purpose == nativeSourcePurpose {
		return !terminal(r.Status) && !terminal(r.Stage), nil
	}
	if terminal(r.Status) || terminal(r.Stage) {
		return false, nil
	}
	if r.deadlineReached(time.Now()) {
		return true, nil
	}
	if r.Status == "needs_input" || r.Stage == "needs_input" || strings.HasPrefix(r.Status, "blocked_") || strings.HasPrefix(r.Stage, "blocked_") {
		return false, nil
	}
	if r.WaitJSON == "" {
		return true, nil
	}
	var wait informationWait
	if err := json.Unmarshal([]byte(r.WaitJSON), &wait); err != nil {
		return false, err
	}
	if wait.Stage == "await_evidence" || r.Stage == "await_evidence" {
		return true, nil
	}
	if wait.WakeAt != nil && !wait.WakeAt.After(time.Now()) {
		return true, nil
	}
	allTerminal := len(wait.Dependencies) > 0
	for _, old := range wait.Dependencies {
		current, err := s.informationDependency(ctx, old.Kind, old.ID)
		if err != nil {
			return false, err
		}
		allTerminal = allTerminal && current.Terminal
	}
	return allTerminal, nil
}

func extractJSONRecordsFromResponses(responses []acquisition.Response) []aiapp.Record {
	var rows []aiapp.Record
	for _, resp := range responses {
		trimmed := strings.TrimSpace(resp.Body)
		if trimmed == "" {
			continue
		}
		var raw any
		if json.Unmarshal([]byte(trimmed), &raw) != nil {
			continue
		}
		switch data := raw.(type) {
		case []any:
			for idx, element := range data {
				if item, ok := element.(map[string]any); ok {
					fields := normalizeRecordFields(item, resp.Receipt.URL)
					if len(fields) > 1 {
						rows = append(rows, aiapp.Record{Key: fmt.Sprintf("%s:%d", resp.Receipt.URL, idx), SchemaVersion: "v1", Fields: fields, Provenance: aiapp.Provenance{URL: resp.Receipt.URL, FetchedAt: resp.Receipt.FetchedAt}})
					}
				}
			}
		case map[string]any:
			var objectArray []any
			ambiguous := false
			for _, value := range data {
				array, ok := value.([]any)
				if !ok || len(array) == 0 {
					continue
				}
				for _, element := range array {
					if _, ok := element.(map[string]any); ok {
						ambiguous = objectArray != nil
						objectArray = array
						break
					}
				}
				if ambiguous {
					break
				}
			}
			if ambiguous {
				continue
			}
			if objectArray != nil {
				for idx, element := range objectArray {
					if item, ok := element.(map[string]any); ok {
						fields := normalizeRecordFields(item, resp.Receipt.URL)
						if len(fields) > 1 {
							rows = append(rows, aiapp.Record{Key: fmt.Sprintf("%s:%d", resp.Receipt.URL, idx), SchemaVersion: "v1", Fields: fields, Provenance: aiapp.Provenance{URL: resp.Receipt.URL, FetchedAt: resp.Receipt.FetchedAt}})
						}
					}
				}
				continue
			}
			fields := normalizeRecordFields(data, resp.Receipt.URL)
			if len(fields) > 1 {
				rows = append(rows, aiapp.Record{Key: resp.Receipt.URL, SchemaVersion: "v1", Fields: fields, Provenance: aiapp.Provenance{URL: resp.Receipt.URL, FetchedAt: resp.Receipt.FetchedAt}})
			}
		}
	}
	return rows
}

func normalizeRecordFields(fields map[string]any, sourceURL string) map[string]any {
	out := make(map[string]any, len(fields)+1)
	for k, v := range fields {
		if projected, ok := projectObservationValue(v); ok {
			out[k] = projected
		}
	}
	out["source_url"] = sourceURL
	return out
}

func projectObservationValue(v any) (any, bool) {
	switch val := v.(type) {
	case nil:
		return nil, false
	case string:
		trimmed := strings.TrimSpace(val)
		if trimmed == "" {
			return nil, false
		}
		return trimmed, true
	case bool, int, int64, float64, json.Number:
		return val, true
	case []string:
		out := make([]string, 0, len(val))
		for _, item := range val {
			if trimmed := strings.TrimSpace(item); trimmed != "" {
				out = append(out, trimmed)
			}
		}
		if len(out) == 0 {
			return nil, false
		}
		return out, true
	case []any:
		if len(val) == 0 {
			return nil, false
		}
		out := make([]string, 0, len(val))
		for _, elem := range val {
			s, ok := elem.(string)
			if !ok {
				return nil, false
			}
			if trimmed := strings.TrimSpace(s); trimmed != "" {
				out = append(out, trimmed)
			}
		}
		if len(out) == 0 {
			return nil, false
		}
		return out, true
	default:
		return nil, false
	}
}

func truncateRunes(s string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + "..."
}

func compactAPISample(records []aiapp.Record, maxItems int) []map[string]any {
	out := make([]map[string]any, 0, min(len(records), maxItems))
	for i, rec := range records {
		if i >= maxItems {
			break
		}
		out = append(out, rec.Fields)
	}
	return out
}

func compactObservationPreviews(obs []Observation, maxBytes int) []map[string]any {
	return compactObservationPreviewsWithLimits(obs, maxBytes, 0)
}

func sharedObservationSourceURL(obs []Observation) (string, bool) {
	shared := ""
	for _, o := range obs {
		raw, _ := o.Fields["source_url"].(string)
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return "", false
		}
		if shared == "" {
			shared = raw
			continue
		}
		if raw != shared {
			return "", false
		}
	}
	return shared, shared != ""
}

func compactObservationPreviewsWithLimits(obs []Observation, maxBytes, maxFieldRunes int) []map[string]any {
	if len(obs) == 0 {
		return nil
	}
	if maxFieldRunes <= 0 {
		maxFieldRunes = max(240, min(900, maxBytes/max(len(obs), 1)/2))
	}
	_, omitRepeatedSourceURL := sharedObservationSourceURL(obs)
	previews := make([]map[string]any, 0, len(obs))
	totalBytes := 2
	for _, o := range obs {
		item := map[string]any{"observation_id": o.ID}
		keys := make([]string, 0, len(o.Fields))
		for k, v := range o.Fields {
			if v == nil || (omitRepeatedSourceURL && k == "source_url") {
				continue
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if s, ok := o.Fields[k].(string); ok {
				item[k] = truncateRunes(s, maxFieldRunes)
			} else {
				item[k] = o.Fields[k]
			}
		}
		encoded, err := json.Marshal(item)
		if err != nil {
			continue
		}
		if len(previews) > 0 && maxBytes > 0 && totalBytes+len(encoded)+1 > maxBytes {
			break
		}
		totalBytes += len(encoded) + 1
		previews = append(previews, item)
	}
	return previews
}
