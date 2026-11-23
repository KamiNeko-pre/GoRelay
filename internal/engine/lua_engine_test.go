package engine

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"gorelay/internal/acquisition"
)

func TestRunProgramStopsDeadloopAtDeadline(t *testing.T) {
	startedAt := time.Now()
	_, err := RunProgram(context.Background(), ProgramRequest{
		Code:      `while true do end`,
		TargetURL: "https://example.com",
	}, ProgramLimits{Timeout: 200 * time.Millisecond}, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want wrapped context deadline exceeded", err)
	}
	if elapsed := time.Since(startedAt); elapsed > time.Second {
		t.Fatalf("Lua execution returned too late: %v", elapsed)
	}
}

func TestRunProgramParsesCollectionContractAndHTMLFind(t *testing.T) {
	result, err := RunProgram(context.Background(), ProgramRequest{
		Code: `
local title, findErr = html_find("<article><h1 class='title'>GoRelay</h1></article>", ".title")
if findErr then
    return false, findErr
end

return true, {
    items = {{
        url = TARGET_URL,
        name = title,
        stars = 12300,
    }},
    meta = {source = "test"},
}
`,
		TargetURL: "https://github.com/example/project",
	}, ProgramLimits{Timeout: time.Second}, nil)
	if err != nil {
		t.Fatalf("execute Lua script: %v", err)
	}
	items, ok := result.Collection["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items = %#v, want one item", result.Collection["items"])
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("item = %#v, want object", items[0])
	}
	if item["url"] != "https://github.com/example/project" || item["name"] != "GoRelay" {
		t.Fatalf("item = %#v, want resolved URL and parsed title", item)
	}
	if item["stars"] != float64(12300) {
		t.Fatalf("stars = %#v, want 12300", item["stars"])
	}
}

func TestRunProgramExposesCollectionAndURLHelpers(t *testing.T) {
	result, err := RunProgram(context.Background(), ProgramRequest{
		Code: `
local html = [[
<article><a href="/first"> First </a></article>
<article><a href="second">Second</a></article>
]]

local titles, titleErr = html_find_all(html, "article a")
if titleErr then return false, titleErr end
local links, linkErr = html_attr_all(html, "article a", "href")
if linkErr then return false, linkErr end
local absoluteURL, resolveErr = url_resolve(TARGET_URL, links[1])
if resolveErr then return false, resolveErr end

return true, {
    items = {
        {url = absoluteURL, title = titles[1]},
        {url = links[2], title = titles[2]},
    },
    meta = {count = #titles},
}
`,
		TargetURL: "https://example.com/catalog/index.html",
	}, ProgramLimits{Timeout: time.Second}, nil)
	if err != nil {
		t.Fatalf("execute Lua helper script: %v", err)
	}
	items, ok := result.Collection["items"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("items = %#v, want two items", result.Collection["items"])
	}
	first, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("first item = %#v, want object", items[0])
	}
	if first["url"] != "https://example.com/first" || first["title"] != "First" {
		t.Fatalf("first item = %#v, want resolved URL and trimmed title", first)
	}
	meta, ok := result.Collection["meta"].(map[string]any)
	if !ok || meta["count"] != float64(2) {
		t.Fatalf("meta = %#v, want count 2", result.Collection["meta"])
	}
}

func TestRunProgramDecodesAndEncodesJSON(t *testing.T) {
	result, err := RunProgram(context.Background(), ProgramRequest{
		Code: `
local payload, decodeErr = json_decode([[
{
  "jobs": [
    {"id": "job-1", "title": "Go Intern", "remote": true, "salary": 300.5},
    {"id": "job-2", "title": "Backend Intern", "remote": false, "salary": null}
  ]
}
]])
if decodeErr then return false, decodeErr end

local encoded, encErr = json_encode({ok=true, nums={1, 2}})
if encErr then return false, encErr end

return true, {
    items = {
        {
            id = payload.jobs[1].id,
            title = payload.jobs[1].title,
            remote = payload.jobs[1].remote,
            salary = payload.jobs[1].salary,
            encoded = encoded,
        },
        {
            id = payload.jobs[2].id,
            title = payload.jobs[2].title,
            remote = payload.jobs[2].remote,
        },
    },
    meta = {count = #payload.jobs},
}
`,
		TargetURL: "https://example.com/jobs",
	}, ProgramLimits{Timeout: time.Second}, nil)
	if err != nil {
		t.Fatalf("execute Lua JSON rule: %v", err)
	}

	items, ok := result.Collection["items"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("items = %#v, want two decoded jobs", result.Collection["items"])
	}
	first := items[0].(map[string]any)
	if first["id"] != "job-1" || first["remote"] != true || first["salary"] != 300.5 {
		t.Fatalf("first item = %#v, want decoded JSON scalar fields", first)
	}
	encPayload := first["encoded"].(string)
	if !strings.Contains(encPayload, `"ok":true`) || !strings.Contains(encPayload, `[1,2]`) {
		t.Fatalf("json_encode payload = %q", encPayload)
	}
}

func TestRunProgramReportsInvalidJSONAndScriptErrors(t *testing.T) {
	_, err := RunProgram(context.Background(), ProgramRequest{
		Code: `
local _, decodeErr = json_decode("{not-json}")
if decodeErr then return false, decodeErr end
return true, {items = {}}
`,
		TargetURL: "https://example.com/jobs",
	}, ProgramLimits{Timeout: time.Second}, nil)
	if err == nil || !strings.Contains(err.Error(), "decode JSON") {
		t.Fatalf("error = %v, want JSON decode error", err)
	}

	_, err = RunProgram(context.Background(), ProgramRequest{
		Code:      `return false, "selector not found"`,
		TargetURL: "https://example.com",
	}, ProgramLimits{Timeout: time.Second}, nil)
	if err == nil || !strings.Contains(err.Error(), "selector not found") {
		t.Fatalf("error = %v, want rule error message", err)
	}
}

func TestRunProgramDoesNotExposeDangerousLibraries(t *testing.T) {
	_, err := RunProgram(context.Background(), ProgramRequest{
		Code: `
local forbidden = {
    "os", "io", "debug", "package",
    "dofile", "load", "loadfile", "loadstring", "require", "module",
}

for _, name in ipairs(forbidden) do
    if _G[name] ~= nil then
        return false, "forbidden global exposed: " .. name
    end
end

return true, {
    items = {},
    meta = {source = "sandbox-test"},
}
`,
		TargetURL: "https://example.com",
	}, ProgramLimits{Timeout: time.Second}, nil)
	if err != nil {
		t.Fatalf("dangerous libraries should not be exposed: %v", err)
	}
}

func TestLuaMillisecondsRejectsOverflowingInput(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	if _, err := luaMilliseconds(maxInt, "sleep duration", time.Millisecond); err == nil {
		t.Fatal("an overflowing millisecond input should be rejected before duration conversion")
	}
	if delay, err := luaMilliseconds(int(maxLuaWait.Milliseconds()), "sleep duration", 0); err != nil || delay != maxLuaWait {
		t.Fatalf("maximum allowed duration = %v, %v; want %v, nil", delay, err, maxLuaWait)
	}
}

func TestRunProgramSleepStopsAtContextDeadline(t *testing.T) {
	startedAt := time.Now()
	_, err := RunProgram(context.Background(), ProgramRequest{
		Code: `
local ok, sleepErr = sleep_ms(1000)
if sleepErr then return false, sleepErr end
return true, {items = {}, meta = {slept = ok}}
`,
		TargetURL: "https://example.com",
	}, ProgramLimits{Timeout: 50 * time.Millisecond}, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want wrapped context deadline exceeded", err)
	}
	if elapsed := time.Since(startedAt); elapsed > 500*time.Millisecond {
		t.Fatalf("sleep ignored context deadline: %v", elapsed)
	}
}

func TestHTTPRequestExposesCookiesAndHeaders(t *testing.T) {
	req := ProgramRequest{
		Code: `
			local res = http_request({url = TARGET_URL})
			return true, { items = {{ tok = res.cookies["csrf"], etag = res.headers["etag"], sr = res.source_response }} }
		`,
		TargetURL: "https://example.org/csrf",
	}
	out, err := RunProgramWithRequester(context.Background(), req, DefaultProgramLimits(), func(_ context.Context, opts acquisition.RequestOptions) (acquisition.Response, error) {
		return acquisition.Response{
			Body: `{"ok":true}`,
			Receipt: acquisition.Receipt{
				Method:    http.MethodGet,
				URL:       opts.URL,
				Status:    200,
				FetchedAt: time.Now().UTC(),
				Headers:   map[string]string{"etag": `"v7"`, "content-type": "application/json"},
				Cookies:   map[string]string{"csrf": "tok-xyz"},
			},
		}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	items := out.Collection["items"].([]any)
	first := items[0].(map[string]any)
	if first["tok"] != "tok-xyz" || first["etag"] != `"v7"` || first["sr"] != float64(1) {
		t.Fatalf("expected cookies/headers/source_response in Lua response table, got %+v", first)
	}
}
