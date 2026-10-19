package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go-lua-crawler/internal/acquisition"
)

func TestProgramPaginationAndTypedResult(t *testing.T) {
	fetched := []string{}
	fetch := func(ctx context.Context, method, url string) (acquisition.Response, error) {
		fetched = append(fetched, url)
		body := `{"items":[{"name":"A","count":2,"active":true}]}`
		if strings.HasSuffix(url, "2") {
			body = `{"items":[{"name":"B","count":3,"active":false}]}`
		}
		return acquisition.Response{Body: body, Receipt: acquisition.Receipt{URL: url, Status: 200, FetchedAt: time.Now().UTC(), Bytes: int64(len(body))}}, nil
	}
	got, err := RunProgram(context.Background(), ProgramRequest{Code: `local items = {}; for p=1,2 do local body,err=http_get(TARGET_URL.."?page="..p); if not body then return false,err end; local obj=json_decode(body); for _,row in ipairs(obj.items) do table.insert(items,row) end end; return true,{items=items,meta={pages=2}}`, TargetURL: "https://example.com/list"}, ProgramLimits{Timeout: time.Second, MaxRequests: 3, MaxBytes: 4096, MaxItems: 10, MaxOutputBytes: 4096}, fetch)
	if err != nil {
		t.Fatal(err)
	}
	items := got.Collection["items"].([]any)
	if len(items) != 2 || items[0].(map[string]any)["count"] != float64(2) || items[1].(map[string]any)["active"] != false || len(got.Requests) != 2 || len(fetched) != 2 {
		t.Fatalf("lost typed rows or receipts: %+v", got)
	}
}

func TestProgramBudgetCannotBeCaughtOrIgnored(t *testing.T) {
	fetch := func(context.Context, string, string) (acquisition.Response, error) {
		return acquisition.Response{Body: "12345"}, nil
	}
	_, err := RunProgram(context.Background(), ProgramRequest{Code: `pcall(function() http_get("https://example.com") end); return true,{items={}}`}, ProgramLimits{Timeout: time.Second, MaxRequests: 1, MaxBytes: 4, MaxItems: 5, MaxOutputBytes: 4096}, fetch)
	if !errors.Is(err, acquisition.ErrBudgetExceeded) {
		t.Fatalf("program hid hard budget error: %v", err)
	}
}

func TestProgramKeepsHardFailureClassificationWhenLuaRaises(t *testing.T) {
	for _, cause := range []error{acquisition.ErrNotAllowed, acquisition.ErrBudgetExceeded} {
		fetch := func(context.Context, string, string) (acquisition.Response, error) {
			return acquisition.Response{}, cause
		}
		_, err := RunProgram(context.Background(), ProgramRequest{Code: `local body,err=http_get("https://example.com"); assert(body,err); return true,{items={}}`}, ProgramLimits{Timeout: time.Second, MaxRequests: 1, MaxBytes: 1024, MaxItems: 5, MaxOutputBytes: 4096}, fetch)
		if !errors.Is(err, cause) {
			t.Fatalf("Lua erased hard failure type %v: %v", cause, err)
		}
	}
}

func TestProgramHasNoWriteOrHostLibrariesAndStopsLoop(t *testing.T) {
	code := `assert(os==nil and io==nil and package==nil and debug==nil and require==nil and load==nil and http_post==nil and print==nil); return true,{items={}}`
	_, err := RunProgram(context.Background(), ProgramRequest{Code: code}, ProgramLimits{Timeout: time.Second, MaxRequests: 1, MaxBytes: 1024, MaxItems: 5, MaxOutputBytes: 4096}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = RunProgram(context.Background(), ProgramRequest{Code: `while true do end`}, ProgramLimits{Timeout: 20 * time.Millisecond, MaxRequests: 1, MaxBytes: 1024, MaxItems: 5, MaxOutputBytes: 4096}, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unbounded loop: %v", err)
	}
}

func TestProgramOutputLimitAndParameters(t *testing.T) {
	limits := ProgramLimits{Timeout: time.Second, MaxRequests: 1, MaxBytes: 1024, MaxItems: 1, MaxOutputBytes: 4096}
	_, err := RunProgram(context.Background(), ProgramRequest{Code: `return true,{items={{x=PARAMS.x},{x=2}}}`, Params: map[string]any{"x": "value"}}, limits, nil)
	if err == nil {
		t.Fatal("item cap missing")
	}
	limits.MaxItems = 3
	got, err := RunProgram(context.Background(), ProgramRequest{Code: `return true,{items={{x=PARAMS.x}}}`, Params: map[string]any{"x": "value"}}, limits, nil)
	if err != nil || got.Collection["items"].([]any)[0].(map[string]any)["x"] != "value" {
		t.Fatalf("params not bound: %+v %v", got, err)
	}
}
