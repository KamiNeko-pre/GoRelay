package information

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/agentstatus"
	"go-lua-crawler/internal/aiapp"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var informationAgentInstructions = `Execute requests using the 5-stage pipeline:

1. Plan & Source Discovery:
   - Reuse matching assets and source URLs from the host facts or the update baseline. When known official URLs are enough to start, plan and start those sources immediately. Use read_materials(kind='assets') for additional catalog detail when needed.
   - If an exact applicable URL is known or appears in a receipt's related_links, inspect or fetch it directly. Batch the independent links. Use discover_sources(query=...) only for still-unknown entries; without a search provider use available saved entries or public directories. Reuse returned verified_sources[].source_ref directly for extraction and evidence.
   - Call update_plan in the same turn as independent initial acquisition. Plan coverage of requested facts and reasoning; set needs_calculation only when the user wants a derived numeric result, not when listing or comparing published figures.

2. Choose an acquisition route:
   - Prefer a verified structured API. For unfamiliar pages inspect in a single acquire_sources(inspect_urls=[...]) request. When you already know which phrases or rules to check, pass excerpt_queries in the same acquire_sources or run_lua call to get evidence.matches from the captures. Use observed URLs and source_ref. data_endpoint_paths are configuration fragments; resolve them with the source's link or request-assembly code before attempting an endpoint.
   - Extract directly via acquire_sources(api=...) or run_lua(mode="sample" / mode="batch") once you already know the verified API schema, saved_assets rule, or inspected DOM/JSON structure. Get all required values, units, exemptions, and supporting excerpts from that source in one pass when applicable.
   - Send independent source calls together in the same assistant turn; use targets for a shared script or parallel run_lua calls for different formats. Group multiple unknown questions into ONE mode="probe" script, and reparse existing captures via source_ref or search captured prose/terms via read_materials(kind="excerpts", queries=[...]) rather than writing many serial grep scripts or re-fetching the same page.

3. Lua Generation & Validation:
   - Extract concise structured records via acquire_sources(api=...) or run_lua(mode="sample" / mode="batch"): return true, { items = { { ... }, ... } } (or return false, "reason"). Parse JSON with json_decode and keep page prose inside the acquisition runtime.
   - Empty items={} without empty_reason is rejected in sample/batch modes; use mode="probe" for read-only inspection (arbitrary diagnostic tables and bounded print(...) logs are captured without persisting rows).
   - Lua globals: TARGET_URL, PARAMS, SOURCE_RESPONSE (when source_ref is passed), print(...) (captured as print_logs), http_request({url, method?, headers?, body?, referer?}) -> {ok, status, final_url, headers, cookies, body, error, source_response} (preferred; shares an automatic per-execution Cookie Jar across calls in the same script), http_get(url) -> body_or_nil, err, http_get_referer(url, ref) -> body_or_nil, err, http_head(url) -> status_or_nil, err, html_find(html, sel), html_find_all(html, sel) -> text_array, html_find_html(html, sel), html_find_all_html(html, sel) -> html_array, html_attr_all(html, sel, attr), source_text(html), clean_text(html), json_decode(str), json_encode(val), url_resolve(base, rel), sleep_ms(ms).
   - Single-request scripts and SOURCE_RESPONSE scripts bind provenance automatically. When one sample/batch script issues multiple http_request calls across URLs/pages, attach source_response = res.source_response on each item returned from res.
   - Pass source_ref={doc_id, revision} (omitting url/urls/targets) to probe or extract an already-captured snapshot via SOURCE_RESPONSE.body instead of re-fetching over HTTP. Once structure is verified via inspect_urls or mode="probe", extract all targets directly in a single mode="batch" call (or self-contained pagination loop) without an extra sample round-trip.

4. Fault Recovery:
   - On HTTP or selector failure, use the status, diagnostic headers, dom_matches, lua_diagnostics, and source_structure to correct the specific request or Lua operation. Respect Retry-After on rate-limited sources and continue independent sources.
   - Escalate to browser_extract(url=...) only when anti-bot protection or JS rendering blocks direct HTTP/Lua and the URL appears in browser_approved_urls; use its returned html_outline, references (for run_lua source_ref=...), and XHR/Fetch network_apis (to replay public JSON APIs directly in run_lua; Cookie Jar handles Set-Cookie automatically while res.cookies exposes CSRF/session token values for headers) or pass browser.fields to browser_extract.

5. Data Delivery:
   - When a Lua item reads a number directly from JSON, add source_json_pointers={field="/absolute/path"} to that item. Match the item's actual source_response; use RFC 6901 escapes (~0 for ~ and ~1 for /). The host checks the value against the original response and returns sample_evidence only for verified paths.
   - Extracted records and snapshots persist automatically. In submit_delivery, bind each formula variable once in calculation_inputs. Use prompt_indices={name:index} for original quantities. For captured JSON numbers, copy the field from the sample_evidence entry with the matching zero-based row_index in sample_items (API) or observations (Lua) into evidence_inputs={name:{source_value:{source_ref:{doc_id,revision},json_pointer}}}. Any exact JSON path already observed can use the same form; do not reread those values. For prose, bind {evidence_id,number_index} from evidence.matches; batch only missing values or rules via read_materials(kind="excerpts"). Keep units and conditions grounded in source citations, especially when a JSON path alone does not identify them. Convert inside calculations[].formula and use {{id}} for computed results. Omit tables to assemble saved detail rows, or provide summary rows without a row_observation_ids matrix.
   - Submit one concise report for conclusions, applied rules, and assumptions. Put comparison values in tables using {{id}}. If the user requests formulas, put {{calculations:details}} in the report; the host expands verified formulas and inputs. Resolve material conflicts by checking the governing source and its scope or effective date; once the governing rule is established, proceed. Distinguish allowed estimation conventions from unresolved gaps that materially affect the answer. Use partial for those gaps and no_match for genuine empty results.`

type userQuestion struct {
	Question string `json:"question" jsonschema:"minLength=1"`
}

type informationTool struct {
	host *informationHost
	info *schema.ToolInfo
}

func (t *informationTool) Info(context.Context) (*schema.ToolInfo, error) { return t.info, nil }
func (t *informationTool) InvokableRun(ctx context.Context, args string, _ ...tool.Option) (string, error) {
	out, err := t.host.invoke(ctx, compose.GetToolCallID(ctx), t.info.Name, args)
	if err != nil {
		return out, err
	}
	return out, nil
}
func (h *informationHost) unknownTool(ctx context.Context, name, args string) (string, error) {
	return h.invoke(ctx, compose.GetToolCallID(ctx), name, args)
}
func informationObject(properties map[string]any, required ...string) any {
	executionRequired := make([]string, 0, len(required))
	for _, field := range required {
		if field != "reason" {
			executionRequired = append(executionRequired, field)
		}
	}
	return map[string]any{"type": "object", "properties": properties, "required": executionRequired, "additionalProperties": false}
}
func informationToolDefinitions() []aiapp.ToolDefinition {
	str := map[string]any{"type": "string"}
	list := map[string]any{"type": "array", "items": str}
	integer := map[string]any{"type": "integer", "minimum": 1}
	boolean := map[string]any{"type": "boolean"}
	headersSchema := map[string]any{"type": "object", "additionalProperties": str}
	api := informationObject(map[string]any{"url": str, "method": str, "headers": headersSchema, "body": str, "items_pointer": str, "fields": map[string]any{"type": "object", "additionalProperties": str}, "next_pointer": str}, "url", "items_pointer", "fields")
	browser := informationObject(map[string]any{"empty_state": informationObject(map[string]any{"selector": str, "text": str}, "selector", "text"), "url": str, "rows_selector": str, "wait_selector": str, "next_selector": str, "fields": map[string]any{"type": "object", "additionalProperties": informationObject(map[string]any{"selector": str, "attribute": str, "url_transform": informationObject(map[string]any{"query_parameter": str, "resolve": boolean})})}}, "fields")
	acquisitionSchema := businessInputSchema(AcquisitionRequest{})
	luaSchema := businessInputSchema(luaRunArgs{})
	defs := []struct {
		name, description string
		params            any
	}{
		{"ask_user", "Ask one concise question when required user input or clarification is missing.", businessInputSchema(userQuestion{})},
		{"discover_sources", "Pass query to run an independent source discovery task (or saved_only=true to query stored entries; inside discovery, pass query+count to a search provider or urls to verify candidates).", informationObject(map[string]any{"query": str, "count": integer, "urls": list, "saved_only": boolean, "offset": map[string]any{"type": "integer", "minimum": 0}, "limit": map[string]any{"type": "integer", "minimum": 0}})},
		{"acquire_sources", "Fetch HTTP sources, inspect via inspect_urls/inspect_url, call structured api, or run an asset_id. Pass excerpt_queries with inspection, api, or direct HTTP to return verbatim source evidence in the same receipt.", informationObject(map[string]any{"urls": list, "inspect_url": str, "inspect_urls": list, "excerpt_queries": list, "api": api, "asset_id": str, "url": str, "method": str, "headers": headersSchema, "body": str, "params": map[string]any{"type": "object"}, "acquisition": acquisitionSchema, "description": str, "reason": str})},
		{"run_lua", "Fetch and extract fields in one script. Return true,{items={...}} to save records; use probe for a specific unknown. Pass excerpt_queries to get original evidence from this run's captures. Independent calls in one turn run concurrently. Pass source_ref={doc_id,revision} alone to parse SOURCE_RESPONSE. Equivalent stateless GETs reuse snapshots from this task.", luaSchema},
		{"browser_extract", "Fallback browser rendering for host-approved anti-bot or JS-only URLs (from browser_approved_urls). Renders page and returns html_outline + references for run_lua(source_ref=...), or extracts rows directly when browser.fields are supplied.", informationObject(map[string]any{"url": str, "browser": browser, "acquisition": acquisitionSchema, "description": str}, "url")},
	}
	out := []aiapp.ToolDefinition{}
	for _, d := range defs {
		out = append(out, aiapp.ToolDefinition{Type: "function", Function: aiapp.FunctionDefinition{Name: d.name, Description: d.description, Parameters: mustMarshal(d.params)}})
	}
	return append(out, businessToolDefinitions()...)
}
func (h *informationHost) tools(ctx context.Context) ([]tool.BaseTool, error) {
	infos, err := aiapp.EinoToolInfos(informationToolDefinitions())
	if err != nil {
		return nil, err
	}
	out := make([]tool.BaseTool, 0, len(infos))
	for _, info := range infos {
		out = append(out, &informationTool{host: h, info: info})
	}
	return out, nil
}
func cleanJSONString(raw string) string {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "```") {
		lines := strings.Split(raw, "\n")
		if len(lines) >= 2 && strings.HasPrefix(lines[len(lines)-1], "```") {
			raw = strings.Join(lines[1:len(lines)-1], "\n")
		} else {
			raw = strings.TrimPrefix(raw, "```json")
			raw = strings.TrimPrefix(raw, "```")
			raw = strings.TrimSuffix(raw, "```")
		}
		raw = strings.TrimSpace(raw)
	}
	return raw
}

func decodeInformationArgs(raw string, out any) error {
	raw = cleanJSONString(raw)
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(out); err != nil {
		return actionFailure("ROUTING_ARGUMENTS_INVALID", err, "Use the exact advertised tool schema and correct the named field from actual observations.")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return actionFailure("ROUTING_ARGUMENTS_INVALID", fmt.Errorf("arguments must contain one JSON object"), "Remove trailing JSON or text.")
	}
	return nil
}
func informationBusinessArgs(raw, name string) (any, error) {
	raw = normalizeToolArgsDeterministic(name, raw)
	var args map[string]any
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&args); err != nil {
		return nil, err
	}
	if args == nil {
		return nil, fmt.Errorf("tool arguments must be a JSON object")
	}
	var declared struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	invalidShape := validateInformationToolArgs(name, raw) != nil
	for _, d := range informationToolDefinitions() {
		if d.Function.Name == name {
			_ = json.Unmarshal(d.Function.Parameters, &declared)
			break
		}
	}
	invalid := []string{}
	for _, key := range []string{"reason", "description", "call_id", "tool_call_id", "timestamp", "time"} {
		value, present := args[key]
		if !present {
			continue
		}
		if _, accepted := declared.Properties[key]; !accepted {
			invalid = append(invalid, key)
			delete(args, key)
		} else if _, textual := value.(string); textual {
			delete(args, key)
		}
	}

	// Rejected argument shapes cannot alias corrected valid arguments. Transport
	// values remain irrelevant within the rejected shape, so changing them does
	// not bypass duplicate admission.
	if len(invalid) > 0 {
		return map[string]any{"arguments": args, "invalid_transport_fields": invalid}, nil
	}
	if invalidShape {
		return map[string]any{"arguments": args, "invalid_argument_shape": true}, nil
	}
	return args, nil
}

func informationToolInput(name string, args any, readVersion string) string {
	if name == "acquire_sources" || name == "run_lua" {
		if fields, ok := args.(map[string]any); ok && fields["excerpt_queries"] != nil {
			physical := maps.Clone(fields)
			delete(physical, "excerpt_queries")
			args = physical
		}
	}
	return agentdata.Hash([]any{name, args, readVersion})
}

type informationReceipt struct {
	Name              string              `json:"name"`
	CallID            string              `json:"call_id"`
	ReceiptID         uint64              `json:"receipt_id,omitempty"`
	Status            string              `json:"status"`
	Data              any                 `json:"data,omitempty"`
	Error             string              `json:"error,omitempty"`
	Code              string              `json:"code,omitempty"`
	Owner             string              `json:"owner,omitempty"`
	Recoverability    string              `json:"recoverability,omitempty"`
	SuggestedActions  []string            `json:"suggested_actions,omitempty"`
	EvidenceRefs      []string            `json:"evidence_refs,omitempty"`
	PriorStepID       uint64              `json:"prior_step_id,omitempty"`
	Recovery          []string            `json:"recovery,omitempty"`
	Details           map[string]any      `json:"details,omitempty"`
	OperationalStatus *agentstatus.Status `json:"operational_status,omitempty"`
}

func (h *informationHost) invoke(ctx context.Context, id, name, raw string) (string, error) {
	h.mu.Lock()
	gate, hasGate := h.batchGates[id]
	if hasGate {
		delete(h.batchGates, id)
	}
	h.mu.Unlock()
	if hasGate {
		defer close(gate.done)
		for _, ch := range gate.waits {
			select {
			case <-ch:
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
	}
	h.mu.Lock()
	if h.pause != nil {
		var saved Step
		e := h.service.DB.WithContext(ctx).Where("request_id = ? AND eino_epoch = ? AND call_id = ? AND kind = ?", h.owner.RequestID, h.request.EinoEpoch, id, name).First(&saved).Error
		h.mu.Unlock()
		if e == nil && (saved.Stage == "succeeded" || saved.Stage == "failed") {
			return saved.ResultJSON, nil
		}
		out := informationReceipt{Name: name, CallID: id, Status: "failed", Code: "BATCH_NOT_DISPATCHED", Error: "The earlier tool paused this batch; this action was not dispatched.", Recovery: []string{"Restore the reported dependency and resume the original task. Existing receipts and evidence are preserved."}}
		return string(mustMarshal(out)), nil
	}
	if h.finished {
		h.mu.Unlock()
		return string(mustMarshal(informationReceipt{Name: name, CallID: id, Status: "failed", Code: "BATCH_NOT_DISPATCHED", Error: "A preceding delivery was accepted; this later batch action was not dispatched.", Recovery: []string{"Read the committed result. Start a new user request for further acquisition."}})), nil
	}
	epoch := h.request.EinoEpoch
	h.mu.Unlock()
	// 1. Apply deterministic normalizations (JSON pointers, aliases) before admission or idempotency.
	normRaw := normalizeToolArgsDeterministic(name, raw)
	var exactStep Step
	exactErr := h.service.DB.WithContext(ctx).Where("request_id = ? AND eino_epoch = ? AND call_id = ? AND kind = ?", h.owner.RequestID, epoch, id, name).Order("id ASC").First(&exactStep).Error
	if exactErr != nil && !errors.Is(exactErr, gorm.ErrRecordNotFound) {
		return "", exactErr
	}
	var boundRaw string
	var bindErr error
	var args any
	var parseErr error
	var readVersion string
	var readErr error
	var hash string
	if errors.Is(exactErr, gorm.ErrRecordNotFound) {
		// 2. Bind DB-dependent defaults ONCE before initial admission; recovery replays saved InputJSON verbatim.
		boundRaw, bindErr = h.bindCurrentRead(ctx, name, normRaw)
		if bindErr != nil {
			var be *BusinessError
			var ae *ActionError
			if !errors.As(bindErr, &be) && !errors.As(bindErr, &ae) {
				return "", bindErr
			}
			boundRaw = normRaw
		}
		args, parseErr = informationBusinessArgs(boundRaw, name)
		if parseErr != nil {
			args = map[string]any{"invalid_json": boundRaw}
		}
		readVersion, readErr = h.currentReadVersion(ctx, name, boundRaw)
		hash = informationToolInput(name, args, readVersion)
	}
	var step Step
	fresh := false
	err := h.owned(ctx, func(tx *gorm.DB) error {
		fresh = false
		var exact Step
		e := tx.Where("request_id = ? AND eino_epoch = ? AND call_id = ? AND kind = ?", h.owner.RequestID, epoch, id, name).Order("id ASC").First(&exact).Error
		if e == nil {
			priorRaw := exact.RawInputJSON
			if priorRaw == "" {
				priorRaw = exact.InputJSON
			}
			if !sameInformationCallArguments(name, normRaw, priorRaw) {
				return agentdata.ErrConflict
			}
			step, raw = exact, exact.InputJSON
			return nil
		}
		if !errors.Is(e, gorm.ErrRecordNotFound) {
			return e
		}
		if boundRaw == "" {
			boundRaw = normRaw
			args, parseErr = informationBusinessArgs(boundRaw, name)
			if parseErr != nil {
				args = map[string]any{"invalid_json": boundRaw}
			}
			readVersion, readErr = h.currentReadVersion(ctx, name, boundRaw)
			hash = informationToolInput(name, args, readVersion)
		}
		raw = boundRaw
		stepKey := "eino_tool:" + hash
		step = Step{EinoEpoch: epoch, RequestID: h.owner.RequestID, Key: stepKey, Kind: name, CallID: id, InputHash: hash, RawInputJSON: normRaw, InputJSON: boundRaw, Stage: "reserved"}
		q := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&step)
		if q.Error != nil {
			return q.Error
		}
		fresh = q.RowsAffected == 1
		if !fresh {
			if err := tx.Where(map[string]any{"request_id": step.RequestID, "key": step.Key}).First(&step).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	recovering := false
	if !fresh {
		if step.CallID != id {
			out := informationReceipt{Name: name, CallID: id, Status: "failed", Code: "ACTION_ALREADY_CALLED", Error: "Same tool and effective arguments already admitted.", PriorStepID: step.ID, Data: validRaw(step.ResultJSON), Recovery: []string{"Use the prior receipt. Only change a parameter when it addresses the specific cause or reaches a genuinely different source."}}
			if name == "acquire_sources" || name == "run_lua" {
				out.Recovery = []string{"Use the prior receipt. To find additional excerpts, call read_materials(kind='excerpts') on the saved references; changing only excerpt_queries does not rerun acquisition."}
			}
			return string(mustMarshal(out)), nil
		}
		if step.Stage == "succeeded" || step.Stage == "failed" {
			return step.ResultJSON, nil
		}
		recovering = step.Stage != "waiting"
	}
	if err = h.event(ctx, "tool_started", "正在执行工具", name, "running"); err != nil {
		return "", err
	}
	var data any
	var wait *informationWait
	ctx = context.WithValue(ctx, acquisitionReceiptKey{}, step.ID)
	if recovering {
		var known bool
		data, wait, err, known = h.restoreDomainAction(ctx, name, raw, step)
		if !known {
			err = &ActionError{Code: "ACTION_OUTCOME_UNKNOWN", Message: "The original tool was admitted but has no committed external result.", PriorStepID: step.ID, Details: map[string]any{"tool": name, "original_call_id": step.CallID, "saved_receipt": validRaw(step.ResultJSON)}, Steps: []string{"Review saved local evidence or adjust parameters/source."}}
		}
	} else if bindErr != nil {
		err = bindErr
	} else if readErr != nil {
		err = readErr
	} else if parseErr != nil {
		err = actionFailure("ROUTING_ARGUMENTS_INVALID", parseErr, "Provide one JSON object matching the advertised tool schema.")
	} else {
		data, wait, err = h.dispatch(ctx, name, raw, step)
	}
	if data != nil && (name == "acquire_sources" || name == "run_lua") {
		var excerptErr error
		data, excerptErr = h.attachAcquisitionExcerpts(ctx, raw, data)
		if excerptErr != nil && err == nil {
			err = actionFailure("ACQUISITION_EXCERPTS_FAILED", excerptErr, "Use the saved references to query evidence via read_materials(kind='excerpts'). The acquisition is already persisted.")
		}
	}
	if errors.Is(err, agentdata.ErrConflict) || ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return "", err
	}
	receipt := informationReceipt{Name: name, CallID: id, ReceiptID: step.ID, Status: "succeeded", Data: data}
	if err != nil {
		receipt.Status = "failed"
		receipt.Error = err.Error()
		receipt.Code = "TOOL_ACTION_FAILED"
		var action *ActionError
		if errors.As(err, &action) {
			receipt.Code, receipt.PriorStepID, receipt.Recovery = action.Code, action.PriorStepID, action.Steps
			receipt.Details = action.Details
		}
		var correctable *BusinessError
		if errors.As(err, &correctable) {
			receipt.Code = correctable.Code
			receipt.Recovery = correctable.SuggestedActions
			receipt.Details = correctable.Details
			receipt.Owner = correctable.Owner
			receipt.Recoverability = correctable.Recoverability
			receipt.SuggestedActions = correctable.SuggestedActions
			receipt.EvidenceRefs = correctable.EvidenceRefs
		}
		status := aiapp.OperationalStatus(err)
		receipt.OperationalStatus = &status
		if receipt.Owner == "" {
			receipt.Owner = status.Owner
		}
		if receipt.Recoverability == "" {
			receipt.Recoverability = "agent"
		}
		if len(receipt.SuggestedActions) == 0 {
			receipt.SuggestedActions = receipt.Recovery
		}
	}
	if receipt.Error != "" && len(receipt.Recovery) == 0 {
		receipt.Recovery = []string{"Read the exact cause above and adjust Lua code, request headers/method, or source parameters before retrying."}
		receipt.SuggestedActions = receipt.Recovery
	}
	stage := receipt.Status
	if wait != nil {
		stage = "waiting"
		receipt.Status = "waiting"
	}
	saved := string(mustMarshal(receipt))
	saveErr := h.owned(context.WithoutCancel(ctx), func(tx *gorm.DB) error {
		return tx.Model(&Step{}).Where("id = ? AND call_id = ?", step.ID, id).Updates(map[string]any{"stage": stage, "result_json": saved, "error": receipt.Error}).Error
	})
	if saveErr != nil {
		return "", saveErr
	}
	if wait != nil {
		return "", h.interrupt(ctx, *wait)
	}
	status := "done"
	if err != nil {
		status = "failed"
	}
	if e := h.event(ctx, "tool", "工具执行完成", name, status); e != nil {
		return "", e
	}
	return saved, nil
}
func (h *informationHost) dispatch(ctx context.Context, name, raw string, step Step) (any, *informationWait, error) {
	s, r := h.service, &h.request
	if err := validateInformationToolArgs(name, raw); err != nil {
		return nil, nil, err
	}
	switch name {
	case "ask_user":
		if r.Purpose == "source_discovery" {
			return nil, nil, businessError("DISCOVERY_GAP", "discovery", "来源发现由父任务对接用户", "提交已核验来源和实际缺口，由父任务判断是否需要澄清")
		}
		var args userQuestion
		if err := decodeInformationArgs(raw, &args); err != nil {
			return nil, nil, err
		}
		h.mu.Lock()
		h.pause = &informationWait{Stage: "needs_input", Reason: args.Question}
		h.mu.Unlock()
		return args, nil, nil
	case "discover_sources":
		return h.discover(ctx, raw, step)
	case "acquire_sources":
		return h.acquire(ctx, raw, step)
	case "run_lua":
		return h.runLuaTool(ctx, raw, step)
	case "browser_extract":
		var args struct {
			URL         string              `json:"url"`
			Browser     *BrowserPlan        `json:"browser"`
			Acquisition *AcquisitionRequest `json:"acquisition"`
			Description string              `json:"description"`
		}
		if err := decodeInformationArgs(raw, &args); err != nil {
			return nil, nil, err
		}
		if args.Browser == nil || len(args.Browser.Fields) == 0 {
			structure, err := h.inspectBrowserSource(ctx, args.URL)
			return structure, nil, err
		}
		if args.Browser.URL == "" {
			args.Browser.URL = args.URL
		}
		fieldNames := make([]string, 0, len(args.Browser.Fields))
		for k := range args.Browser.Fields {
			fieldNames = append(fieldNames, k)
		}
		acqReq := ensureAcquisitionFromFieldNames(args.Acquisition, h.input.Goal, fieldNames)
		ctx, err := withLocalAcquisition(ctx, acqReq)
		if err != nil {
			return nil, nil, err
		}
		trial, err := s.TrialBrowser(ctx, r.ID, args.Description, *args.Browser)
		if err != nil {
			return trial, nil, err
		}
		return acquisitionTrialSummary(trial), nil, nil
	case "update_plan", "revise_requirement_interpretation", "read_materials", "submit_delivery":
		data, err := h.dispatchBusiness(ctx, name, raw, step)
		return data, nil, err
	default:
		return nil, nil, actionFailure("UNKNOWN_TOOL", fmt.Errorf("unknown information domain tool %q", name), "Choose an advertised domain tool and its exact schema.")
	}
}

// Exact Eino crash recovery uses the original arguments and receipt even if a
// mutable local data version has advanced. New model decisions get fresh IDs.
func sameInformationCallArguments(name, left, right string) bool {
	a, e1 := informationBusinessArgs(left, name)
	b, e2 := informationBusinessArgs(right, name)
	if e1 != nil || e2 != nil {
		return left == right
	}
	leftArgs, leftOK := a.(map[string]any)
	rightArgs, rightOK := b.(map[string]any)
	if !leftOK || !rightOK {
		return agentdata.Hash(a) == agentdata.Hash(b)
	}
	for _, args := range []map[string]any{leftArgs, rightArgs} {
		if name == "discover_sources" {
			delete(args, "capability_generation")
		}
	}
	return agentdata.Hash(a) == agentdata.Hash(b)
}
