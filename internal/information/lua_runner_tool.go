package information

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"

	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/engine"
	"go-lua-crawler/internal/knowledge"
	"go-lua-crawler/internal/programrunner"
	"gorm.io/gorm"
)

type luaTarget struct {
	URL    string         `json:"url"`
	Params map[string]any `json:"params,omitempty"`
}

type luaSourceRef struct {
	DocID    string `json:"doc_id"`
	Revision string `json:"revision"`
}

type luaRunArgs struct {
	Code           string         `json:"code"`
	Mode           string         `json:"mode,omitempty" jsonschema:"enum=probe,enum=sample,enum=batch,description=probe（只读探测：返回结构/接口诊断和可复用快照引用，不写入业务记录，适合单次脚本探清多个问题）；sample（默认：提取并保存可用记录，返回字段类型与范围）；batch（有界并发执行多目标并落库）"`
	URL            string         `json:"url,omitempty" jsonschema:"description=单个目标 URL（与 urls/targets 任选其一；自包含 http_request 脚本可全部省略）"`
	URLs           []string       `json:"urls,omitempty" jsonschema:"description=要执行同一 Lua 脚本的目标 URL 列表"`
	Targets        []luaTarget    `json:"targets,omitempty" jsonschema:"description=带独立 PARAMS 的目标列表（{url, params}）"`
	SourceRef      *luaSourceRef  `json:"source_ref,omitempty" jsonschema:"description=仅在基于已捕获快照重跑解析时传 {doc_id, revision}（勿同时传 url/urls/targets），Lua 通过 SOURCE_RESPONSE 读取原响应"`
	Params         map[string]any `json:"params,omitempty" jsonschema:"description=传给 Lua 全局变量 PARAMS 的公共参数对象"`
	EmptyReason    string         `json:"empty_reason,omitempty"`
	Description    string         `json:"description,omitempty"`
	ExcerptQueries []string       `json:"excerpt_queries,omitempty" jsonschema:"description=Search this run's saved captures for these phrases and return evidence handles in the same tool receipt"`
}

func (h *informationHost) loadLuaSource(ctx context.Context, ref luaSourceRef) (knowledge.Document, *acquisition.Response, error) {
	unavailable := func(reason string) error {
		return businessError("LUA_SOURCE_UNAVAILABLE", "acquisition", reason, "使用本任务采集回执 references 中的 doc_id 和 revision，或采集一个新的可用响应")
	}
	if ref.DocID == "" || ref.Revision == "" {
		return knowledge.Document{}, nil, unavailable("缺少快照 ID 或版本")
	}
	var link taskDocument
	err := h.service.DB.WithContext(ctx).Where("request_id = ? AND doc_id = ? AND revision = ? AND status IN ? AND body_valid = ? AND http_status >= 200 AND http_status < 300",
		h.request.ID, ref.DocID, ref.Revision, []string{"succeeded", reusedResponseStatus}, true).Order("receipt_id").First(&link).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return knowledge.Document{}, nil, unavailable("当前任务没有这份成功的采集快照")
	}
	if err != nil {
		return knowledge.Document{}, nil, err
	}
	doc, err := knowledge.NewStore(h.service.DB).ResolveDocument(ctx, ref.DocID, ref.Revision, knowledge.RetrievalScope{URLHosts: h.input.SourceScope})
	if errors.Is(err, knowledge.ErrInvalidReference) || errors.Is(err, gorm.ErrRecordNotFound) {
		return knowledge.Document{}, nil, unavailable("快照版本不可访问")
	}
	if err != nil {
		return knowledge.Document{}, nil, err
	}
	body := doc.RawBody
	if body == "" {
		body = doc.Body
	}
	if !urlInScope(doc.URL, h.input.SourceScope) || doc.FetchedAt.IsZero() || body == "" {
		return knowledge.Document{}, nil, unavailable("快照缺少可用正文、来源或获取时间")
	}
	return doc, &acquisition.Response{
		Body: body, Receipt: acquisition.Receipt{URL: doc.URL, Status: link.HTTPStatus, FetchedAt: doc.FetchedAt},
	}, nil
}

func (s *Service) executeLuaProgram(ctx context.Context, req programrunner.Request) (engine.ProgramResult, error) {
	if s.LuaRunner != nil {
		return s.LuaRunner(ctx, req)
	}
	if s.Crawler != nil && s.Crawler.Runner != nil {
		return s.Crawler.Runner.Execute(ctx, req)
	}
	return engine.ProgramResult{}, actionFailure("LUA_RUNNER_UNAVAILABLE", errors.New("isolated Lua runner is not configured"), "Restore the Lua runner, use a verified API for this source, or report the unavailable source in the delivery gaps.")
}

func resolveLuaTargets(args luaRunArgs, fallbackURLs, scope []string) ([]luaTarget, error) {
	return resolveLuaTargetsWithOptions(args, fallbackURLs, scope, false)
}

func resolveLuaTargetsWithOptions(args luaRunArgs, fallbackURLs, scope []string, allowLocalNoURL bool) ([]luaTarget, error) {
	out := make([]luaTarget, 0, len(args.Targets)+len(args.URLs)+1)
	seen := map[string]bool{}
	add := func(raw string, params map[string]any) error {
		u := strings.TrimSpace(raw)
		if u == "" {
			return nil
		}
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			u = "https://" + u
		}
		if !urlInScope(u, scope) {
			return actionFailure("ASSET_SCOPE_INVALID", fmt.Errorf("target URL %q is outside scope or malformed", raw), "Use observed public HTTP(S) URLs within source_scope.")
		}
		merged := map[string]any{}
		for k, v := range args.Params {
			merged[k] = v
		}
		for k, v := range params {
			merged[k] = v
		}
		key := u + "|" + string(mustMarshal(merged))
		if seen[key] {
			return nil
		}
		seen[key] = true
		out = append(out, luaTarget{URL: u, Params: merged})
		return nil
	}
	if err := add(args.URL, nil); err != nil {
		return nil, err
	}
	for _, u := range args.URLs {
		if err := add(u, nil); err != nil {
			return nil, err
		}
	}
	for _, t := range args.Targets {
		if err := add(t.URL, t.Params); err != nil {
			return nil, err
		}
	}
	if len(out) == 0 {
		for _, u := range fallbackURLs {
			if err := add(u, nil); err != nil {
				return nil, err
			}
		}
	}
	if len(out) == 0 && (luaCodePerformsHTTP(args.Code) || allowLocalNoURL) {
		return []luaTarget{{Params: args.Params}}, nil
	}
	if len(out) == 0 {
		return nil, actionFailure("SOURCE_URLS_REQUIRED", fmt.Errorf("run_lua needs a source to extract records"), "For a captured source, pass only source_ref={doc_id,revision} and read SOURCE_RESPONSE. For new sources, pass url/targets. For pure arithmetic, use submit_delivery.calculations with the saved source values and formula.")
	}
	return out, nil
}

func luaCodePerformsHTTP(code string) bool {
	return strings.Contains(code, "http_request") ||
		strings.Contains(code, "http_get") ||
		strings.Contains(code, "http_head")
}

func normalizeLuaItems(items []any) ([]map[string]any, map[string]int) {
	clean := make([]map[string]any, 0, len(items))
	omittedFields := map[string]int{}
	for _, raw := range items {
		fields, _ := raw.(map[string]any)
		projected := map[string]any{}
		hasData := false
		for k, v := range fields {
			if k == "source_url" || k == "source_response" || k == "source_json_pointers" {
				projected[k] = v
				continue
			}
			val, keep := projectObservationValue(v)
			if number, ok := val.(float64); ok && (math.IsNaN(number) || math.IsInf(number, 0)) {
				keep = false
			}
			if !keep {
				omittedFields[k]++
				continue
			}
			projected[k] = val
			hasData = true
		}
		if hasData {
			clean = append(clean, projected)
		}
	}
	return clean, omittedFields
}

type luaDOMMatch struct {
	API       string `json:"api"`
	Selector  string `json:"selector"`
	Attribute string `json:"attribute,omitempty"`
	Matches   int    `json:"matches"`
}

func sanitizeProbeMeta(meta map[string]any) map[string]any {
	out := make(map[string]any, len(meta))
	for k, v := range meta {
		if s, ok := v.(string); ok {
			out[k] = truncateRunes(strings.Join(strings.Fields(s), " "), 120)
		} else {
			out[k] = v
		}
	}
	return out
}

func luaProbeFeedback(result engine.ProgramResult, includeMatches bool) map[string]any {
	feedback := map[string]any{}
	if meta, ok := result.Collection["meta"].(map[string]any); ok && len(meta) > 0 {
		feedback["probe"] = sanitizeProbeMeta(meta)
	}
	if len(result.PrintLogs) > 0 {
		feedback["print_logs"] = result.PrintLogs
	}
	if len(result.ExecutionDiagnostics) > 0 {
		feedback["lua_diagnostics"] = result.ExecutionDiagnostics
	}
	if includeMatches && len(result.ExecutionObservations) > 0 {
		matches := make([]luaDOMMatch, 0, len(result.ExecutionObservations))
		seen := map[luaDOMMatch]bool{}
		for _, observation := range result.ExecutionObservations {
			match := luaDOMMatch{observation.API, observation.Selector, observation.Attribute, observation.Matches}
			if !seen[match] {
				seen[match] = true
				matches = append(matches, match)
			}
		}
		feedback["dom_matches"] = matches
	}
	return feedback
}

func resolveSourceResponseNumber(value any, sourceURL string, result engine.ProgramResult, eligible map[int]bool) int {
	if value == nil {
		return 0
	}
	if m, ok := value.(map[string]any); ok {
		if inner, hasInner := m["source_response"]; hasInner {
			value = inner
		}
	}
	if n, ok := viewNumber(value); ok && n.IsInt() && n.Num().IsInt64() {
		i := n.Num().Int64()
		if i > 0 && i <= int64(len(result.SourceEvidence)) && eligible[int(i)] {
			receipt := result.SourceEvidence[i-1].Receipt
			if sourceURL == "" || sourceURL == receipt.RequestedURL || sourceURL == receipt.URL {
				return int(i)
			}
		}
		return 0
	}
	if len(eligible) == 1 && sourceURL == "" {
		for only := range eligible {
			return only
		}
	}
	return 0
}

func luaRecordsFromItems(items []map[string]any, result engine.ProgramResult, fallbackURL string, captureOffset int, defaultSourceResponse ...any) ([]aiapp.Record, bool) {
	byURL := map[string][]int{}
	eligible := make(map[int]bool)
	for index, capture := range result.SourceEvidence {
		receipt := capture.Receipt
		if receipt.Status < 200 || receipt.Status >= 300 || receipt.FetchedAt.IsZero() || receipt.URL == "" {
			continue
		}
		if _, blocked := browserSourceDiagnosis(capture); blocked {
			continue
		}
		number := index + 1
		eligible[number] = true
		byURL[receipt.URL] = append(byURL[receipt.URL], number)
		if receipt.RequestedURL != "" && receipt.RequestedURL != receipt.URL {
			byURL[receipt.RequestedURL] = append(byURL[receipt.RequestedURL], number)
		}
	}
	var fallbackSR any
	if len(defaultSourceResponse) > 0 {
		fallbackSR = defaultSourceResponse[0]
	}
	rows := make([]aiapp.Record, 0, len(items))
	jsonBodies := map[int]any{}
	for idx, fields := range items {
		rawURL, provided := fields["source_url"]
		sourceURL, isString := rawURL.(string)
		if provided && !isString {
			continue
		}
		sourceURL = strings.TrimSpace(sourceURL)
		number := 0
		if value, hasSR := fields["source_response"]; hasSR {
			number = resolveSourceResponseNumber(value, sourceURL, result, eligible)
		} else {
			matches := byURL[sourceURL]
			if sourceURL == "" {
				matches = byURL[fallbackURL]
				if len(eligible) == 1 {
					for only := range eligible {
						matches = []int{only}
					}
				}
			}
			if len(matches) == 1 {
				number = matches[0]
			} else if fallbackSR != nil {
				number = resolveSourceResponseNumber(fallbackSR, sourceURL, result, eligible)
			}
		}
		if number == 0 {
			continue
		}
		capture := result.SourceEvidence[number-1]
		receipt := capture.Receipt
		locations, _ := fields["source_json_pointers"].(map[string]any)
		delete(fields, "source_json_pointers")
		var support map[string]aiapp.FieldSupport
		if len(locations) > 0 {
			root, parsed := jsonBodies[number]
			if !parsed {
				root, _ = decodeJSONCalculationSource(capture.Body)
				jsonBodies[number] = root
			}
			support = verifyLuaJSONPointers(root, fields, locations)
		}
		delete(fields, "source_response")
		fields["source_url"] = receipt.URL
		rows = append(rows, aiapp.Record{
			Key: fmt.Sprintf("%s#%d", receipt.URL, idx), SchemaVersion: "v1", Fields: fields,
			Provenance: aiapp.Provenance{URL: receipt.URL, FetchedAt: receipt.FetchedAt, ResponseIndex: captureOffset + number, FieldSupport: support},
		})
	}
	return rows, len(eligible) > 0
}

func verifyLuaJSONPointers(root any, fields, locations map[string]any) map[string]aiapp.FieldSupport {
	var support map[string]aiapp.FieldSupport
	for field, location := range locations {
		path, ok := location.(string)
		observed, numeric := deliveryJSONNumber(fields[field])
		if !ok || !numeric {
			continue
		}
		path, err := normalizeJSONPointer(path)
		if err != nil {
			continue
		}
		value, err := pointer(root, path)
		if err != nil {
			continue
		}
		actual, ok := deliveryJSONNumber(value)
		if !ok || actual != observed {
			continue
		}
		if support == nil {
			support = map[string]aiapp.FieldSupport{}
		}
		support[field] = aiapp.FieldSupport{Status: "supported", ActualLocator: path}
	}
	return support
}

func (h *informationHost) runLuaTool(ctx context.Context, raw string, step Step) (any, *informationWait, error) {
	var args luaRunArgs
	if err := decodeInformationArgs(raw, &args); err != nil {
		return nil, nil, err
	}
	args.ExcerptQueries = nil // Evidence queries do not change the Lua execution identity.
	args.Code = strings.TrimSpace(args.Code)
	if args.Code == "" {
		return nil, nil, actionFailure("ROUTING_ARGUMENTS_INVALID", fmt.Errorf("run_lua requires non-empty code"), "Provide Lua script returning true, {items=...} or false, diagnostic.")
	}
	mode := strings.ToLower(strings.TrimSpace(args.Mode))
	if mode == "" {
		mode = "sample"
	}
	if mode != "probe" && mode != "sample" && mode != "batch" {
		return nil, nil, actionFailure("ROUTING_ARGUMENTS_INVALID", fmt.Errorf("mode must be 'probe', 'sample', or 'batch'"), "Use mode='probe' for read-only structure/API inspection (no persistence), mode='sample' to extract and verify records, or mode='batch' for concurrent multi-target extraction.")
	}
	isProbe := mode == "probe"
	var targets []luaTarget
	var savedDoc knowledge.Document
	var savedResponse *acquisition.Response
	var err error
	if args.SourceRef == nil {
		targets, err = resolveLuaTargetsWithOptions(args, h.input.EntryURLs, h.input.SourceScope, isProbe)
	} else {
		if args.URL != "" || len(args.URLs) > 0 || len(args.Targets) > 0 {
			return nil, nil, businessError("LUA_SOURCE_AMBIGUOUS", "acquisition", "source_ref 与 url、urls、targets 指定了不同目标入口", "要在快照上修复脚本时只传 source_ref 和代码；批量取新页面时传 targets")
		}
		savedDoc, savedResponse, err = h.loadLuaSource(ctx, *args.SourceRef)
		targets = []luaTarget{{URL: savedDoc.URL, Params: args.Params}}
	}
	if err != nil {
		return nil, nil, err
	}

	execution, fresh, err := h.service.reserveStep(ctx, h.request, "run_lua", args)
	if err != nil {
		return nil, nil, err
	}
	if !fresh {
		if execution.ResultJSON == "" {
			return nil, nil, ErrUnknownStep
		}
		return validRaw(execution.ResultJSON), nil, restoredDomainFailure(execution)
	}
	snapshots := acquisitionSnapshots{responses: map[string]acquisition.Response{}, documents: map[string]knowledge.Document{}}
	if args.SourceRef == nil || luaCodePerformsHTTP(args.Code) {
		snapshots, err = h.acquisitionSnapshots(ctx)
		if err != nil {
			return nil, nil, err
		}
	}
	if len(targets) > 1 && len(snapshots.responses) > 0 && h.service.LuaRunner == nil && h.service.Crawler != nil && h.service.Crawler.Runner != nil {
		batchCtx, closeBatch, err := h.service.Crawler.Runner.ShareSnapshots(ctx, snapshots.responses)
		if err == nil {
			ctx = batchCtx
			defer closeBatch()
		} else if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
	}
	limits := engine.DefaultProgramLimits()

	outcomes := make([]luaTargetOutcome, len(targets))
	runTarget := func(target luaTarget) luaTargetOutcome {
		res, execErr := h.service.executeLuaProgram(ctx, programrunner.Request{
			Program: engine.ProgramRequest{
				Code: args.Code, TargetURL: target.URL, Params: target.Params, AllowPartialItems: true, ProbeMode: isProbe, SavedResponse: savedResponse,
			},
			Limits:         limits,
			AllowedDomains: h.input.SourceScope,
			Snapshots:      snapshots.responses,
		})
		outcome := luaTargetOutcome{target: target, result: res, err: execErr}
		if execErr == nil && !isProbe {
			rawItems, itemsPresent := res.Collection["items"].([]any)
			outcome.items, outcome.omittedFields = normalizeLuaItems(rawItems)
			outcome.droppedRows = len(rawItems) - len(outcome.items)
			verifiedEmpty := itemsPresent && len(rawItems) == 0 && strings.TrimSpace(args.EmptyReason) != ""
			if len(outcome.items) == 0 && !verifiedEmpty {
				outcome.err = errors.New("Lua returned no usable records. Return extracted structured records in items={...}, use mode='probe' for read-only inspection, or supply empty_reason for a verified empty page.")
			}
		}
		return outcome
	}
	if len(targets) == 1 {
		outcomes[0] = runTarget(targets[0])
	} else {
		maxWorkers := max(2, min(runtime.GOMAXPROCS(0), 8))
		if maxWorkers > len(targets) {
			maxWorkers = len(targets)
		}
		globalSem := make(chan struct{}, maxWorkers)
		hostLimit := min(maxWorkers, 3)
		var hostMu sync.Mutex
		hostSems := map[string]chan struct{}{}
		getHostSem := func(rawURL string) chan struct{} {
			u, parseErr := url.Parse(rawURL)
			if parseErr != nil || u.Hostname() == "" {
				return nil
			}
			host := strings.ToLower(u.Hostname())
			hostMu.Lock()
			defer hostMu.Unlock()
			sem, exists := hostSems[host]
			if !exists {
				sem = make(chan struct{}, hostLimit)
				hostSems[host] = sem
			}
			return sem
		}
		var wg sync.WaitGroup
		for i, target := range targets {
			wg.Add(1)
			go func(idx int, tgt luaTarget) {
				defer wg.Done()
				// Acquire per-host slot before global execution slot so a throttled host
				// never holds global worker capacity while waiting in its host queue.
				hSem := getHostSem(tgt.URL)
				if hSem != nil {
					select {
					case hSem <- struct{}{}:
					case <-ctx.Done():
						outcomes[idx] = luaTargetOutcome{target: tgt, err: ctx.Err()}
						return
					}
					defer func() { <-hSem }()
				}
				select {
				case globalSem <- struct{}{}:
				case <-ctx.Done():
					outcomes[idx] = luaTargetOutcome{target: tgt, err: ctx.Err()}
					return
				}
				defer func() { <-globalSem }()
				outcomes[idx] = runTarget(tgt)
			}(i, target)
		}
		wg.Wait()
	}

	if isProbe {
		return h.finishLuaProbe(ctx, execution, step, args, savedResponse != nil, outcomes, snapshots)
	}

	allRows := []aiapp.Record{}
	allCaptures := []acquisition.Response{}
	savedCaptures := map[int]knowledge.Document{}
	failedTargets := []map[string]any{}
	targetDiagnostics := []map[string]any{}
	succeededTargets, partialTargets := 0, 0
	reqSummaries := []map[string]any{}
	sampleItems := []map[string]any{}
	browserApproved := []string{}

	for _, o := range outcomes {
		for _, req := range o.result.Requests {
			if len(reqSummaries) < 12 {
				entry := map[string]any{
					"url":             req.URL,
					"method":          req.Method,
					"status":          req.Status,
					"duration_ms":     req.DurationMS,
					"reused_snapshot": req.ReusedSnapshot,
				}
				addRateLimitAdvice(entry, req)
				reqSummaries = append(reqSummaries, entry)
			}
		}
		captureOffset := len(allCaptures)
		for i, ev := range o.result.SourceEvidence {
			allCaptures = append(allCaptures, ev)
			if savedResponse != nil && i == 0 {
				savedCaptures[captureOffset] = savedDoc
				continue
			}
			if snapshots.bind(ev, savedCaptures, captureOffset+i) {
				continue
			}
			if approvedURL, ok := h.service.maybeApproveBrowserFromFetch(ctx, &h.request, step, h.input.SourceScope, ev); ok {
				browserApproved = append(browserApproved, approvedURL)
			}
		}
		feedback := luaProbeFeedback(o.result, mode == "sample")
		if len(o.omittedFields) > 0 {
			feedback["omitted_fields"] = o.omittedFields
		}
		if o.droppedRows > 0 {
			feedback["rows_without_data"] = o.droppedRows
		}
		var recs []aiapp.Record
		if o.err == nil {
			var validCapture bool
			recs, validCapture = luaRecordsFromItems(o.items, o.result, o.target.URL, captureOffset, o.result.Collection["source_response"])
			switch {
			case !validCapture && len(browserApproved) > 0:
				o.err = errors.New("Target returned a script-rendered or protected shell requiring browser execution; call browser_extract(url=...) on browser_approved_urls to inspect rendered DOM and network_apis.")
			case !validCapture:
				o.err = errors.New("No successful source response was captured. Check the HTTP receipt and fetch the actual source using http_request.")
			case len(recs) == 0 && len(o.items) > 0:
				o.err = errors.New("No row points to a unique successful response. Assign source_response = res.source_response inside each item returned from res, or run one target per call with run_lua(mode='batch', targets=[...]).")
			case hasOversizedRawDump(recs):
				o.err = errors.New("Returned oversized raw text/body dump (>2500 chars) in <=2 items; parse into concise structured items instead of returning res.body/clean_text slices. Use acquire_sources(inspect_url=...) for unknown JSON schemas.")
			}
			if len(recs) != len(o.items) {
				feedback["rows_without_evidence"] = len(o.items) - len(recs)
				feedback["source_binding_hint"] = "Assign source_response = res.source_response inside each item returned from res (not only on the top-level return table), or use run_lua(mode='batch', targets=[...])."
			}
		}
		if (o.err != nil || len(o.result.ExecutionDiagnostics) > 0) && len(o.result.SourceEvidence) > 0 {
			for _, ev := range o.result.SourceEvidence {
				if strings.TrimSpace(ev.Body) != "" {
					feedback["source_structure"] = acquisitionResponseMetadata(ev)
					break
				}
			}
		}
		if len(feedback) > 0 {
			feedback["url"] = o.target.URL
			if len(o.target.Params) > 0 {
				feedback["params"] = o.target.Params
			}
			targetDiagnostics = append(targetDiagnostics, feedback)
		}
		if o.err != nil {
			diag := map[string]any{"url": o.target.URL, "error": o.err.Error()}
			if hint := luaRepairHint(args.Code, o.err); hint != "" {
				diag["fix"] = hint
			}
			if n := len(o.result.Requests); n > 0 {
				httpReceipt := o.result.Requests[n-1]
				diag["http"] = httpReceipt
				addRateLimitAdvice(diag, httpReceipt)
			}
			failedTargets = append(failedTargets, diag)
			continue
		}
		if o.droppedRows > 0 || len(recs) != len(o.items) {
			partialTargets++
		} else {
			succeededTargets++
		}
		allRows = append(allRows, recs...)
		for _, record := range recs {
			if len(sampleItems) < 5 {
				sampleItems = append(sampleItems, record.Fields)
			}
		}
	}

	var cause error
	if succeededTargets == 0 && partialTargets == 0 && len(failedTargets) > 0 {
		causeMsg := fmt.Sprintf("Lua execution produced 0 valid records across %d target(s)", len(failedTargets))
		if msg, _ := failedTargets[0]["error"].(string); msg != "" {
			causeMsg = msg
		}
		var steps []string
		if len(browserApproved) > 0 {
			steps = []string{
				"Anti-bot or JS rendering blocked direct HTTP; host approved browser_approved_urls.",
				"Call browser_extract(url=...) for an approved URL to inspect html_outline or extract directly, then pass source_ref to run_lua if needed.",
			}
		} else if hint, ok := failedTargets[0]["fix"].(string); ok {
			steps = []string{hint}
		} else {
			steps = []string{
				"Inspect failed_targets, HTTP status/headers, and target_diagnostics (including source_structure.html_outline if captured) to fix the request config or Lua selectors.",
				"If a valid capture reference was returned in references, pass source_ref={doc_id,revision} (without url/urls/targets) to avoid re-fetching.",
			}
		}
		failure := actionFailure("LUA_EXECUTION_FAILED", errors.New(causeMsg), steps...)
		failure.Details = map[string]any{
			"mode":           mode,
			"failed_targets": failedTargets,
			"requests":       reqSummaries,
		}
		if len(targetDiagnostics) > 0 {
			failure.Details["target_diagnostics"] = targetDiagnostics
		}
		if len(browserApproved) > 0 {
			failure.Details["browser_approved_urls"] = browserApproved
		}
		cause = failure
	}

	out := map[string]any{
		"status":              mode + "_succeeded",
		"deliverable_receipt": len(allRows) > 0,
		"mode":                mode,
		"targets_total":       len(targets),
		"targets_succeeded":   succeededTargets,
		"targets_partial":     partialTargets,
		"records_saved":       len(allRows),
		"requests":            reqSummaries,
	}
	if mode == "sample" {
		types, ranges := describeLuaSample(sampleItems)
		out["field_types"], out["numeric_ranges"] = types, ranges
		if len(allRows) > 0 && len(failedTargets) == 0 {
			out["next_step"] = "Sampled records saved. Run run_lua(mode='batch', targets=[...]) if additional pages/targets remain; otherwise proceed to the next source or submit_delivery."
		} else {
			out["next_step"] = "Review failed_targets and target_diagnostics.source_structure (html_outline/json_paths), fix selectors or headers in Lua, and retry."
		}
	}
	if len(targetDiagnostics) > 0 {
		out["target_diagnostics"] = targetDiagnostics
	}
	if args.SourceRef != nil {
		out["reused_source"] = args.SourceRef
	}
	if len(failedTargets) > 0 {
		out["failed_targets"] = failedTargets
	}
	if len(browserApproved) > 0 {
		out["browser_approved_urls"] = browserApproved
	}
	if cause != nil {
		out["status"] = mode + "_failed"
		out["next_step"] = strings.Join(cause.(*ActionError).Steps, " ")
	} else if len(failedTargets) > 0 || partialTargets > 0 {
		out["status"] = mode + "_partial"
	}
	if err := h.commitAcquisition(ctx, execution, allRows, allCaptures, out, cause, savedCaptures); err != nil {
		return nil, nil, err
	}
	return out, nil, cause
}

type luaTargetOutcome struct {
	target        luaTarget
	result        engine.ProgramResult
	items         []map[string]any
	omittedFields map[string]int
	droppedRows   int
	err           error
}

var luaErrorLine = regexp.MustCompile(`<string>:(\d+):`)

func luaRepairHint(code string, err error) string {
	if err == nil {
		return ""
	}
	match := luaErrorLine.FindStringSubmatch(err.Error())
	if len(match) == 0 {
		return ""
	}
	line, _ := strconv.Atoi(match[1])
	lines := strings.Split(code, "\n")
	if line < 1 || line > len(lines) {
		return ""
	}
	statement := strings.TrimSpace(lines[line-1])
	if strings.Contains(err.Error(), "non-function object") {
		return fmt.Sprintf("Lua line %d (%s) calls a value that is not a function. Check that the called name is listed in the sandbox API (note: print(...) is only enabled in mode='probe'; in sample/batch modes return true, {items={...}}).", line, statement)
	}
	return fmt.Sprintf("Correct Lua line %d (%s) using the reported error. Keep the same source_ref when only the script changes.", line, statement)
}

func compactProbeValue(v any, depth, maxStrRunes, maxSliceItems, maxMapKeys int) (any, bool) {
	if depth > 6 {
		return "<nested>", true
	}
	switch x := v.(type) {
	case string:
		runes := []rune(x)
		if len(runes) > maxStrRunes {
			return fmt.Sprintf("%s…(+%d chars)", string(runes[:maxStrRunes]), len(runes)-maxStrRunes), true
		}
		return x, false
	case []any:
		truncated := false
		limit := len(x)
		if limit > maxSliceItems {
			limit = maxSliceItems
			truncated = true
		}
		out := make([]any, 0, limit+1)
		for i := 0; i < limit; i++ {
			item, t := compactProbeValue(x[i], depth+1, maxStrRunes, maxSliceItems, maxMapKeys)
			truncated = truncated || t
			out = append(out, item)
		}
		if len(x) > limit {
			out = append(out, fmt.Sprintf("...(+%d more items)", len(x)-limit))
		}
		return out, truncated
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		truncated := false
		limit := len(keys)
		if limit > maxMapKeys {
			limit = maxMapKeys
			truncated = true
		}
		out := make(map[string]any, limit+1)
		for _, k := range keys[:limit] {
			val, t := compactProbeValue(x[k], depth+1, maxStrRunes, maxSliceItems, maxMapKeys)
			truncated = truncated || t
			out[k] = val
		}
		if len(keys) > limit {
			out["_omitted_keys"] = len(keys) - limit
		}
		return out, truncated
	default:
		return v, false
	}
}

func compactProbePayload(val any, budgetBytes int) (any, bool) {
	if val == nil {
		return nil, false
	}
	if raw, err := json.Marshal(val); err == nil && len(raw) <= budgetBytes {
		return val, false
	}
	for _, profile := range [][3]int{
		{400, 40, 40},
		{220, 20, 24},
		{120, 10, 14},
		{64, 5, 8},
	} {
		compacted, truncated := compactProbeValue(val, 0, profile[0], profile[1], profile[2])
		if raw, err := json.Marshal(compacted); err == nil && len(raw) <= budgetBytes {
			return compacted, truncated
		}
	}
	compacted, _ := compactProbeValue(val, 0, 48, 3, 6)
	return compacted, true
}

func (h *informationHost) finishLuaProbe(ctx context.Context, execution Step, step Step, args luaRunArgs, hasSavedResponse bool, outcomes []luaTargetOutcome, snapshots acquisitionSnapshots) (any, *informationWait, error) {
	const probeBudgetBytes = 8192
	var newCaptures []acquisition.Response
	saved := map[int]knowledge.Document{}
	var browserApproved []string
	reqSummaries := []map[string]any{}
	failedTargets := []map[string]any{}
	probeResults := make([]map[string]any, 0, len(outcomes))
	jsonOutlines := map[string]json.RawMessage{}
	succeededTargets := 0

	perTargetBudget := probeBudgetBytes / max(1, len(outcomes))
	if perTargetBudget < 1024 {
		perTargetBudget = 1024
	}

	for _, o := range outcomes {
		for _, req := range o.result.Requests {
			if len(reqSummaries) < 16 {
				entry := map[string]any{
					"url":             req.URL,
					"method":          req.Method,
					"status":          req.Status,
					"duration_ms":     req.DurationMS,
					"reused_snapshot": req.ReusedSnapshot,
				}
				if ct := req.Headers["content-type"]; ct != "" {
					entry["content_type"] = ct
				}
				if names := req.DiagnosticCookieNames(); len(names) > 0 {
					entry["set_cookies"] = names
				}
				if rl := req.Headers["x-ratelimit-remaining"]; rl != "" {
					entry["ratelimit_remaining"] = rl
				}
				addRateLimitAdvice(entry, req)
				reqSummaries = append(reqSummaries, entry)
			}
		}
		for i, ev := range o.result.SourceEvidence {
			if hasSavedResponse && i == 0 {
				continue
			}
			reused := snapshots.bind(ev, saved, len(newCaptures))
			newCaptures = append(newCaptures, ev)
			if reused {
				continue
			}
			if _, exists := jsonOutlines[ev.Receipt.URL]; !exists && (len(ev.Body) > perTargetBudget || len(o.result.Collection) == 0) {
				var value any
				if json.Unmarshal([]byte(ev.Body), &value) == nil {
					jsonOutlines[ev.Receipt.URL] = compactAPISchema(mustMarshal(observedJSONPaths(value, "")), perTargetBudget)
				}
			}
			if approvedURL, ok := h.service.maybeApproveBrowserFromFetch(ctx, &h.request, step, h.input.SourceScope, ev); ok {
				browserApproved = append(browserApproved, approvedURL)
			}
		}
		if o.err != nil {
			diag := map[string]any{"url": o.target.URL, "error": o.err.Error()}
			if hint := luaRepairHint(args.Code, o.err); hint != "" {
				diag["fix"] = hint
			}
			if n := len(o.result.Requests); n > 0 {
				httpReceipt := o.result.Requests[n-1]
				diag["http"] = httpReceipt
				addRateLimitAdvice(diag, httpReceipt)
			}
			failedTargets = append(failedTargets, diag)
			continue
		}
		succeededTargets++
		compacted, truncated := compactProbePayload(o.result.Collection, perTargetBudget)
		entry := map[string]any{"url": o.target.URL, "data": compacted}
		if len(o.target.Params) > 0 {
			entry["params"] = o.target.Params
		}
		if truncated {
			entry["truncated"] = true
		}
		if fb := luaProbeFeedback(o.result, true); len(fb) > 0 {
			if d, ok := fb["lua_diagnostics"]; ok {
				entry["lua_diagnostics"] = d
			}
			if m, ok := fb["dom_matches"]; ok {
				entry["dom_matches"] = m
			}
			if logs, ok := fb["print_logs"]; ok {
				entry["print_logs"] = logs
			}
		}
		probeResults = append(probeResults, entry)
	}

	var cause error
	if succeededTargets == 0 && len(failedTargets) > 0 {
		causeMsg := fmt.Sprintf("Lua probe failed across %d target(s)", len(failedTargets))
		if msg, _ := failedTargets[0]["error"].(string); msg != "" {
			causeMsg = msg
		}
		steps := []string{"Inspect failed_targets and requests to fix Lua syntax, request headers, or target URL, then re-run."}
		if len(browserApproved) > 0 {
			steps = []string{"Target requires browser execution; call browser_extract(url=...) on browser_approved_urls to inspect rendered DOM and network_apis."}
		} else if hint, ok := failedTargets[0]["fix"].(string); ok {
			steps = []string{hint}
		}
		failure := actionFailure("LUA_PROBE_FAILED", errors.New(causeMsg), steps...)
		failure.Details = map[string]any{"mode": "probe", "failed_targets": failedTargets, "requests": reqSummaries}
		if len(browserApproved) > 0 {
			failure.Details["browser_approved_urls"] = browserApproved
		}
		cause = failure
	}

	out := map[string]any{
		"status":              "probe_succeeded",
		"deliverable_receipt": false,
		"mode":                "probe",
		"ephemeral":           true,
		"records_saved":       0,
		"targets_total":       len(outcomes),
		"targets_succeeded":   succeededTargets,
		"requests":            reqSummaries,
		"next_step":           "Use this snapshot's source_ref and extract directly in batch once the structure is clear. This probe receipt alone is not a batch of observations.",
	}
	if len(jsonOutlines) > 0 {
		out["json_outline"] = jsonOutlines
	}
	if len(probeResults) == 1 {
		out["probe_result"] = probeResults[0]["data"]
		if probeResults[0]["truncated"] == true {
			out["probe_truncated"] = true
		}
		if d, ok := probeResults[0]["lua_diagnostics"]; ok {
			out["lua_diagnostics"] = d
		}
		if m, ok := probeResults[0]["dom_matches"]; ok {
			out["dom_matches"] = m
		}
		if logs, ok := probeResults[0]["print_logs"]; ok {
			out["print_logs"] = logs
		}
	} else if len(probeResults) > 1 {
		out["probe_results"] = probeResults
	}
	if args.SourceRef != nil {
		out["reused_source"] = args.SourceRef
	}
	if len(failedTargets) > 0 {
		out["failed_targets"] = failedTargets
	}
	if len(browserApproved) > 0 {
		out["browser_approved_urls"] = browserApproved
	}
	if cause != nil {
		out["status"] = "probe_failed"
		out["next_step"] = strings.Join(cause.(*ActionError).Steps, " ")
	} else if len(failedTargets) > 0 {
		out["status"] = "probe_partial"
		out["next_step"] = "Use the successful probe results. For the remaining targets, follow failed_targets.fix or the HTTP diagnostic."
	}
	// Keep the original snapshot identity on replays; probes produce no observations.
	if err := h.commitAcquisition(ctx, execution, nil, newCaptures, out, cause, saved); err != nil {
		return nil, nil, err
	}
	return out, nil, cause
}

func hasOversizedRawDump(recs []aiapp.Record) bool {
	if len(recs) == 0 || len(recs) > 2 {
		return false
	}
	for _, r := range recs {
		for k, v := range r.Fields {
			lk := strings.ToLower(k)
			if lk == "source_url" || lk == "url" || lk == "href" || lk == "link" {
				continue
			}
			s, ok := v.(string)
			if !ok {
				continue
			}
			lower := strings.ToLower(strings.TrimSpace(s))
			if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
				continue
			}
			if len([]rune(s)) > 2500 {
				return true
			}
		}
	}
	return false
}

func describeLuaSample(items []map[string]any) (map[string]string, map[string][2]float64) {
	types := map[string]string{}
	ranges := map[string][2]float64{}
	for _, item := range items {
		for k, v := range item {
			if k == "source_url" || k == "source_response" || k == "_evidence" || v == nil {
				continue
			}
			var kind string
			switch v.(type) {
			case string:
				kind = "string"
			case bool:
				kind = "boolean"
			case int, int64, float64, json.Number:
				kind = "number"
				if numeric, ok := viewNumber(v); ok {
					value, _ := numeric.Float64()
					if !math.IsInf(value, 0) {
						prior, exists := ranges[k]
						if !exists {
							prior = [2]float64{value, value}
						}
						ranges[k] = [2]float64{min(prior[0], value), max(prior[1], value)}
					}
				}
			default:
				kind = fmt.Sprintf("%T", v)
			}
			if prior, exists := types[k]; exists && prior != kind {
				kind = "mixed"
			}
			types[k] = kind
		}
	}
	return types, ranges
}
