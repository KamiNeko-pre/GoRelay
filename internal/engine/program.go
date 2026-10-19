package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	lua "github.com/yuin/gopher-lua"
	"go-lua-crawler/internal/acquisition"
)

type ProgramRequest struct {
	Code      string         `json:"code"`
	TargetURL string         `json:"target_url,omitempty"`
	Params    map[string]any `json:"params,omitempty"`
}
type ProgramLimits struct {
	Timeout        time.Duration `json:"timeout_ns,omitempty"`
	MaxRequests    int           `json:"max_requests"`
	MaxBytes       int64         `json:"max_bytes"`
	MaxItems       int           `json:"max_items"`
	MaxOutputBytes int           `json:"max_output_bytes"`
}

func DefaultProgramLimits() ProgramLimits {
	return ProgramLimits{Timeout: 20 * time.Second, MaxRequests: 20, MaxBytes: 8 << 20, MaxItems: 1000, MaxOutputBytes: 2 << 20}
}

type ProgramResult struct {
	ExecutionDiagnostics []DOMDiagnostic        `json:"execution_diagnostics,omitempty"`
	HTTPUsageKnown       bool                   `json:"http_usage_known,omitempty"` // Set by the network owner, not Lua output.
	Collection           map[string]any         `json:"collection"`
	Requests             []acquisition.Receipt  `json:"requests"`
	SourceEvidence       []acquisition.Response `json:"source_evidence,omitempty"` // Trusted fetch output, never Lua-created proof.
	Stats                acquisition.Stats      `json:"stats"`                     // logical calls/body bytes; network redirects are additionally tracked by PublicFetcher.
	DurationMS           int64                  `json:"duration_ms"`
}

// RunProgram is the generated-code entry point. Production executes it in the
// separate lua-runner process, whose container supplies the hard memory limit.
// This Go API itself provides cancellation and IO/output budgets, not a heap cap.
func RunProgram(parent context.Context, in ProgramRequest, limits ProgramLimits, fetch acquisition.FetchFunc) (result ProgramResult, err error) {
	start := time.Now()
	result.Requests = []acquisition.Receipt{}
	defer func() {
		result.DurationMS = time.Since(start).Milliseconds()
		if p := recover(); p != nil {
			err = fmt.Errorf("program VM panic: %v", p)
			result.Collection = nil
		}
	}()
	if limits.Timeout <= 0 || limits.MaxRequests <= 0 || limits.MaxBytes <= 0 || limits.MaxItems <= 0 || limits.MaxOutputBytes <= 0 {
		return result, fmt.Errorf("positive program limits are required")
	}
	if len(in.Code) == 0 || len(in.Code) > 64<<10 {
		return result, fmt.Errorf("program must contain 1..65536 bytes")
	}
	ctx, cancel := context.WithTimeout(parent, limits.Timeout)
	defer cancel()
	L := lua.NewState(lua.Options{SkipOpenLibs: true})
	defer L.Close()
	if err = openLuaSandboxLibraries(L); err != nil {
		return result, err
	}
	L.SetGlobal("print", lua.LNil)
	L.SetContext(ctx)
	dom := newDOMCache()
	defer func() { result.ExecutionDiagnostics = dom.diagnostics }()
	for name, fn := range map[string]lua.LGFunction{"html_find": dom.htmlFind, "html_find_all": dom.htmlFindAll, "html_attr_all": dom.htmlAttrAll, "source_text": sourceText, "json_decode": JSONDecode, "url_resolve": URLResolve, "sleep_ms": SleepMS} {
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
	var hardFailure error
	installFetch := func(method string) lua.LGFunction {
		return func(L *lua.LState) int {
			if hardFailure != nil {
				return pushLuaError(L, hardFailure)
			}
			if result.Stats.Requests >= limits.MaxRequests {
				hardFailure = acquisition.ErrBudgetExceeded
				return pushLuaError(L, hardFailure)
			}
			result.Stats.Requests++
			if fetch == nil {
				return pushLuaError(L, fmt.Errorf("network adapter is unavailable"))
			}
			response, e := fetch(ctx, method, L.CheckString(1))
			result.Stats.Bytes += int64(len(response.Body))
			if result.Stats.Bytes > limits.MaxBytes {
				e = acquisition.ErrBudgetExceeded
			}
			if e != nil {
				if errors.Is(e, acquisition.ErrBudgetExceeded) || errors.Is(e, acquisition.ErrNotAllowed) {
					hardFailure = e
				}
				return pushLuaError(L, e)
			}
			result.Requests = append(result.Requests, response.Receipt)
			if method == http.MethodGet {
				result.SourceEvidence = append(result.SourceEvidence, response)
				if response.Receipt.Status >= 200 && response.Receipt.Status < 300 {
					dom.observeSource(response.Body)
				}
			}
			if method == http.MethodHead {
				L.Push(lua.LNumber(response.Receipt.Status))
			} else {
				L.Push(lua.LString(response.Body))
			}
			return 1
		}
	}
	L.SetGlobal("http_get", L.NewFunction(installFetch(http.MethodGet)))
	L.SetGlobal("http_head", L.NewFunction(installFetch(http.MethodHead)))
	if e = L.DoString(in.Code); e != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		if hardFailure != nil {
			return result, hardFailure
		}
		return result, fmt.Errorf("program execution: %w", e)
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if hardFailure != nil {
		return result, hardFailure
	}
	decoded, e := decodeLuaResult(L)
	if e != nil {
		return result, e
	}
	if len(decoded.Collection["items"].([]any)) > limits.MaxItems {
		return result, fmt.Errorf("program item limit exceeded")
	}
	data, e := json.Marshal(decoded.Collection)
	if e != nil {
		return result, e
	}
	if len(data) > limits.MaxOutputBytes {
		return result, fmt.Errorf("program output byte limit exceeded")
	}
	result.Collection = decoded.Collection
	return result, nil
}
