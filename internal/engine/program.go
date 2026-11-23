package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	lua "github.com/yuin/gopher-lua"
	"go-lua-crawler/internal/acquisition"
)

type ProgramRequest struct {
	Code              string                `json:"code"`
	TargetURL         string                `json:"target_url,omitempty"`
	Params            map[string]any        `json:"params,omitempty"`
	AllowPartialItems bool                  `json:"allow_partial_items,omitempty"`
	ProbeMode         bool                  `json:"probe_mode,omitempty"`
	SavedResponse     *acquisition.Response `json:"saved_response,omitempty"`
}
type ProgramLimits struct {
	Timeout        time.Duration `json:"timeout_ns,omitempty"`
	MaxRequests    int           `json:"max_requests"`
	MaxBytes       int64         `json:"max_bytes"`
	MaxItems       int           `json:"max_items"`
	MaxOutputBytes int           `json:"max_output_bytes"`
}

func DefaultProgramLimits() ProgramLimits {
	return ProgramLimits{} // Inherit caller cancellation without per-program task quotas.
}

type ProgramResult struct {
	ExecutionObservations []DOMObservation       `json:"execution_observations,omitempty"`
	ExecutionDiagnostics  []DOMDiagnostic        `json:"execution_diagnostics,omitempty"`
	PrintLogs             []string                        `json:"print_logs,omitempty"`
	HTTPUsageKnown        bool                            `json:"http_usage_known,omitempty"` // Set by the network owner, not Lua output.
	Collection            map[string]any                  `json:"collection"`
	Requests              []acquisition.Receipt           `json:"requests"`
	NetworkAPIs           []acquisition.NetworkAPICapture `json:"network_apis,omitempty"`
	SourceEvidence        []acquisition.Response          `json:"source_evidence,omitempty"` // Trusted fetch output, never Lua-created proof.
	Stats                 acquisition.Stats               `json:"stats"`                     // logical calls/body bytes; network redirects are additionally tracked by PublicFetcher.
	DurationMS            int64                           `json:"duration_ms"`
}

// RequestFunc executes an HTTP request with optional method, headers and body,
// returning the full Response (even for non-2xx HTTP status codes) so scripts
// can inspect status codes and headers directly.
type RequestFunc func(context.Context, acquisition.RequestOptions) (acquisition.Response, error)

// RunProgram is the generated-code entry point. Production executes it in the
// separate lua-runner process, whose container supplies the hard memory limit.
// This Go API provides caller cancellation; process isolation owns the heap limit.
func RunProgram(parent context.Context, in ProgramRequest, limits ProgramLimits, fetch acquisition.FetchFunc) (result ProgramResult, err error) {
	if fetch == nil {
		return RunProgramWithRequester(parent, in, limits, nil)
	}
	return RunProgramWithRequester(parent, in, limits, func(ctx context.Context, opts acquisition.RequestOptions) (acquisition.Response, error) {
		resp, err := fetch(ctx, opts.Method, opts.URL)
		return resp, err
	})
}

func RunProgramWithRequester(parent context.Context, in ProgramRequest, limits ProgramLimits, request RequestFunc) (result ProgramResult, err error) {
	start := time.Now()
	var requestFailure error
	result.Requests = []acquisition.Receipt{}
	defer func() {
		result.DurationMS = time.Since(start).Milliseconds()
		if p := recover(); p != nil {
			err = fmt.Errorf("program VM panic: %v", p)
			result.Collection = nil
		}
		if err != nil && requestFailure != nil && strings.Contains(err.Error(), requestFailure.Error()) {
			err = fmt.Errorf("%w: %v", requestFailure, err)
		}
	}()
	if limits.Timeout < 0 {
		return result, fmt.Errorf("execution timeout must be nonnegative")
	}
	if len(in.Code) == 0 {
		return result, fmt.Errorf("program must contain nonempty Lua source")
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	if limits.Timeout > 0 {
		var deadlineCancel context.CancelFunc
		ctx, deadlineCancel = context.WithTimeout(ctx, limits.Timeout)
		defer deadlineCancel()
	}
	L := lua.NewState(lua.Options{SkipOpenLibs: true})
	defer L.Close()
	if err = openLuaSandboxLibraries(L); err != nil {
		return result, err
	}
	if in.ProbeMode {
		const maxPrintLogBytes = 8192
		var printLogBytes int
		var printTruncated bool
		formatPrintArg := func(v lua.LValue) string {
			if tbl, ok := v.(*lua.LTable); ok {
				if converted, convErr := luaJSON(tbl, map[*lua.LTable]bool{}, probeCollectionNode); convErr == nil {
					if encoded, encErr := json.Marshal(converted); encErr == nil {
						return string(encoded)
					}
				}
			}
			return v.String()
		}
		L.SetGlobal("print", L.NewFunction(func(L *lua.LState) int {
			if printTruncated {
				return 0
			}
			top := L.GetTop()
			parts := make([]string, 0, top)
			for i := 1; i <= top; i++ {
				parts = append(parts, formatPrintArg(L.Get(i)))
			}
			line := strings.Join(parts, "\t")
			if len(line) > 2048 {
				line = line[:2048] + "...(truncated)"
			}
			if printLogBytes+len(line) > maxPrintLogBytes {
				printTruncated = true
				result.PrintLogs = append(result.PrintLogs, "...(print log budget reached)")
				return 0
			}
			printLogBytes += len(line)
			result.PrintLogs = append(result.PrintLogs, line)
			return 0
		}))
	} else {
		L.SetGlobal("print", lua.LNil)
	}
	L.SetContext(ctx)
	dom := newDOMCache()
	defer func() { result.ExecutionDiagnostics = dom.diagnostics; result.ExecutionObservations = dom.observations }()
	cleanText := func(L *lua.LState) int {
		cleaned, e := acquisition.CleanText(L.CheckString(1))
		if e != nil {
			return pushLuaError(L, e)
		}
		L.Push(lua.LString(cleaned))
		return 1
	}
	for name, fn := range map[string]lua.LGFunction{
		"html_find":          dom.htmlFind,
		"html_find_all":      dom.htmlFindAll,
		"html_find_html":     dom.htmlFindHTML,
		"html_find_all_html": dom.htmlFindAllHTML,
		"html_attr_all":      dom.htmlAttrAll,
		"source_text":        sourceText,
		"clean_text":         cleanText,
		"json_decode":        JSONDecode,
		"json_encode":        JSONEncode,
		"url_resolve":        URLResolve,
		"url_query_value":    URLQueryValue,
		"sleep_ms":           SleepMS,
	} {
		L.SetGlobal(name, L.NewFunction(fn))
	}
	L.SetGlobal("TARGET_URL", lua.LString(in.TargetURL))
	params := in.Params
	if params == nil {
		params = map[string]any{}
	}
	value, e := jsonValueToLua(L, params, 0)
	if e != nil {
		return result, e
	}
	L.SetGlobal("PARAMS", value)
	responseTable := func(response acquisition.Response, sourceIndex int, err error) *lua.LTable {
		table := L.NewTable()
		table.RawSetString("status", lua.LNumber(response.Receipt.Status))
		table.RawSetString("final_url", lua.LString(response.Receipt.URL))
		table.RawSetString("body", lua.LString(response.Body))
		if sourceIndex > 0 {
			table.RawSetString("source_response", lua.LNumber(sourceIndex))
		}
		headers := L.NewTable()
		for key, value := range response.Receipt.Headers {
			headers.RawSetString(key, lua.LString(value))
		}
		table.RawSetString("headers", headers)
		cookies := L.NewTable()
		for key, value := range response.Receipt.Cookies {
			cookies.RawSetString(key, lua.LString(value))
		}
		table.RawSetString("cookies", cookies)
		table.RawSetString("ok", lua.LBool(err == nil && response.Receipt.Status >= 200 && response.Receipt.Status < 300))
		if err != nil {
			table.RawSetString("error", lua.LString(err.Error()))
		}
		return table
	}
	if in.SavedResponse != nil {
		result.SourceEvidence = append(result.SourceEvidence, *in.SavedResponse)
		dom.observeSource(in.SavedResponse.Body)
		L.SetGlobal("SOURCE_RESPONSE", responseTable(*in.SavedResponse, len(result.SourceEvidence), nil))
	}
	execHTTP := func(opts acquisition.RequestOptions, strictStatus bool) (acquisition.Response, error) {
		result.Stats.Requests++
		if request == nil {
			return acquisition.Response{}, fmt.Errorf("network adapter is unavailable")
		}
		response, e := request(ctx, opts)
		result.Stats.Bytes += int64(len(response.Body))
		if e != nil {
			requestFailure = e
			return response, e
		}
		if response.Receipt.URL != "" || response.Receipt.Status != 0 {
			result.Requests = append(result.Requests, response.Receipt)
			if strings.EqualFold(opts.Method, http.MethodGet) || strings.EqualFold(opts.Method, http.MethodPost) {
				result.SourceEvidence = append(result.SourceEvidence, response)
				if response.Receipt.Status >= 200 && response.Receipt.Status < 300 {
					dom.observeSource(response.Body)
				}
			}
		} else if response.Body != "" && (strings.EqualFold(opts.Method, http.MethodGet) || strings.EqualFold(opts.Method, http.MethodPost)) {
			dom.observeSource(response.Body)
		}
		if strictStatus && response.Receipt.Status != 0 && (response.Receipt.Status < 200 || response.Receipt.Status >= 300) {
			statusErr := fmt.Errorf("source HTTP status %d", response.Receipt.Status)
			requestFailure = statusErr
			return response, statusErr
		}
		return response, nil
	}
	installFetch := func(method string) lua.LGFunction {
		return func(L *lua.LState) int {
			opts := acquisition.RequestOptions{Method: method, URL: L.CheckString(1)}
			if method == http.MethodGet && L.GetTop() >= 2 && L.Get(2).Type() == lua.LTString {
				opts.Headers = map[string]string{"Referer": L.CheckString(2)}
			}
			response, e := execHTTP(opts, true)
			if e != nil {
				return pushLuaError(L, e)
			}
			if method == http.MethodHead {
				L.Push(lua.LNumber(response.Receipt.Status))
			} else {
				L.Push(lua.LString(response.Body))
			}
			return 1
		}
	}
	luaHTTPRequest := func(L *lua.LState) int {
		opts := acquisition.RequestOptions{Method: http.MethodGet, Headers: map[string]string{}}
		if L.Get(1).Type() == lua.LTString {
			opts.URL = L.CheckString(1)
		} else {
			tbl := L.CheckTable(1)
			opts.URL = tbl.RawGetString("url").String()
			if m := tbl.RawGetString("method"); m != lua.LNil {
				opts.Method = strings.ToUpper(strings.TrimSpace(m.String()))
			}
			if b := tbl.RawGetString("body"); b != lua.LNil {
				opts.Body = b.String()
			}
			if ref := tbl.RawGetString("referer"); ref != lua.LNil && ref.String() != "" {
				opts.Headers["Referer"] = ref.String()
			}
			if hdrs, ok := tbl.RawGetString("headers").(*lua.LTable); ok {
				hdrs.ForEach(func(k, v lua.LValue) {
					if ks, ok := k.(lua.LString); ok {
						opts.Headers[string(ks)] = v.String()
					}
				})
			}
		}
		previousCaptures := len(result.SourceEvidence)
		response, e := execHTTP(opts, false)
		sourceIndex := 0
		if len(result.SourceEvidence) > previousCaptures {
			sourceIndex = len(result.SourceEvidence)
		}
		L.Push(responseTable(response, sourceIndex, e))
		return 1
	}
	L.SetGlobal("http_get", L.NewFunction(installFetch(http.MethodGet)))
	L.SetGlobal("http_get_referer", L.NewFunction(installFetch(http.MethodGet)))
	L.SetGlobal("http_head", L.NewFunction(installFetch(http.MethodHead)))
	L.SetGlobal("http_request", L.NewFunction(luaHTTPRequest))
	if e = L.DoString(in.Code); e != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		return result, fmt.Errorf("program execution: %w", e)
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	var decoded luaScriptResult
	if in.ProbeMode {
		decoded, e = decodeLuaProbeResult(L, len(result.PrintLogs) > 0)
	} else {
		decoded, e = decodeLuaResult(L, in.AllowPartialItems)
	}
	if e != nil {
		return result, e
	}
	result.Collection = decoded.Collection
	return result, nil
}
