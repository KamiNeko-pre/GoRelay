package engine

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExecuteLuaScriptStopsDeadloopAtDeadline(t *testing.T) {
	scriptText, err := os.ReadFile(filepath.Join("..", "..", "scripts", "deadloop.lua"))
	if err != nil {
		t.Fatalf("read deadloop script: %v", err)
	}

	startedAt := time.Now()
	_, err = executeLuaScript(string(scriptText), "https://example.com", 200*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want wrapped context deadline exceeded", err)
	}
	if elapsed := time.Since(startedAt); elapsed > time.Second {
		t.Fatalf("Lua execution returned too late: %v", elapsed)
	}
}

func TestExecuteLuaScriptParsesCollectionContractAndHTMLFind(t *testing.T) {
	result, err := executeLuaScript(`
local title, findErr = html_find("<article><h1 class='title'>LuaSpider</h1></article>", ".title")
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
`, "https://github.com/example/project", time.Second)
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
	if item["url"] != "https://github.com/example/project" || item["name"] != "LuaSpider" {
		t.Fatalf("item = %#v, want resolved URL and parsed title", item)
	}
	if item["stars"] != float64(12300) {
		t.Fatalf("stars = %#v, want 12300", item["stars"])
	}
}

func TestExecuteLuaScriptExposesCollectionAndURLHelpers(t *testing.T) {
	result, err := executeLuaScript(`
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
`, "https://example.com/catalog/index.html", time.Second)
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

func TestExecuteLuaScriptDecodesJSONForRules(t *testing.T) {
	result, err := executeLuaScript(`
local payload, decodeErr = json_decode([[
{
  "jobs": [
    {"id": "job-1", "title": "Go Intern", "remote": true, "salary": 300.5},
    {"id": "job-2", "title": "Backend Intern", "remote": false, "salary": null}
  ]
}
]])
if decodeErr then return false, decodeErr end

return true, {
    items = {
        {
            id = payload.jobs[1].id,
            title = payload.jobs[1].title,
            remote = payload.jobs[1].remote,
            salary = payload.jobs[1].salary,
        },
        {
            id = payload.jobs[2].id,
            title = payload.jobs[2].title,
            remote = payload.jobs[2].remote,
        },
    },
    meta = {count = #payload.jobs},
}
`, "https://example.com/jobs", time.Second)
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
	meta := result.Collection["meta"].(map[string]any)
	if meta["count"] != float64(2) {
		t.Fatalf("meta count = %#v, want 2", meta["count"])
	}
}

func TestExecuteLuaScriptReportsInvalidJSON(t *testing.T) {
	_, err := executeLuaScript(`
local _, decodeErr = json_decode("{not-json}")
if decodeErr then return false, decodeErr end
return true, {items = {}}
`, "https://example.com/jobs", time.Second)
	if err == nil || !strings.Contains(err.Error(), "decode JSON") {
		t.Fatalf("error = %v, want JSON decode error", err)
	}
}

func TestExecuteLuaScriptReturnsRuleError(t *testing.T) {
	_, err := executeLuaScript(`return false, "selector not found"`, "https://example.com", time.Second)
	if err == nil || !strings.Contains(err.Error(), "selector not found") {
		t.Fatalf("error = %v, want rule error message", err)
	}
}

func TestExecuteLuaScriptDoesNotExposeDangerousLibraries(t *testing.T) {
	_, err := executeLuaScript(`
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
`, "https://example.com", time.Second)
	if err != nil {
		t.Fatalf("dangerous libraries should not be exposed: %v", err)
	}
}

func TestExecuteLuaScriptRateLimitsRequestsToTheSameHost(t *testing.T) {
	startedAt := time.Now()
	_, err := executeLuaScript(`
local ok, waitErr = rate_limit("https://rate-limit-test.example/jobs", 40)
if waitErr then return false, waitErr end
local ok2, waitErr2 = rate_limit("https://rate-limit-test.example/jobs/2", 40)
if waitErr2 then return false, waitErr2 end
return true, {items = {}, meta = {first = ok, second = ok2}}
`, "https://example.com", time.Second)
	if err != nil {
		t.Fatalf("execute rate-limited Lua script: %v", err)
	}
	if elapsed := time.Since(startedAt); elapsed < 35*time.Millisecond {
		t.Fatalf("same-host calls were not spaced: elapsed %v", elapsed)
	}
}

func TestExecuteLuaScriptExposesHostLimitedHTTPGet(t *testing.T) {
	_, err := executeLuaScript(`
local _, requestErr = http_get_limited("http://127.0.0.1/jobs", 100)
if not requestErr then return false, "private target unexpectedly accepted" end
if not requestErr:find("URL validation failed", 1, true) then
    return false, "unexpected error: " .. requestErr
end
return true, {items = {}, meta = {validated = true}}
`, "https://example.com", time.Second)
	if err != nil {
		t.Fatalf("limited HTTP helper should be registered and preserve URL validation: %v", err)
	}
}

func TestAcquireHostHTTPGateLimitsConcurrentRequests(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	releaseFirst, err := acquireHostHTTPGate(ctx, "host-gate-concurrency-test.example")
	if err != nil {
		t.Fatalf("acquire first host slot: %v", err)
	}
	firstReleased := false
	defer func() {
		if !firstReleased {
			releaseFirst()
		}
	}()
	releaseSecond, err := acquireHostHTTPGate(ctx, "host-gate-concurrency-test.example")
	if err != nil {
		t.Fatalf("acquire second host slot: %v", err)
	}
	defer releaseSecond()

	thirdAcquired := make(chan func(), 1)
	thirdError := make(chan error, 1)
	go func() {
		release, acquireErr := acquireHostHTTPGate(ctx, "host-gate-concurrency-test.example")
		if acquireErr != nil {
			thirdError <- acquireErr
			return
		}
		thirdAcquired <- release
	}()

	select {
	case <-thirdAcquired:
		t.Fatalf("third request acquired a slot while %d were still in flight", maxConcurrentRequestsPerHost)
	case err := <-thirdError:
		t.Fatalf("third request failed before a slot was released: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	releaseFirst()
	firstReleased = true
	select {
	case releaseThird := <-thirdAcquired:
		releaseThird()
	case err := <-thirdError:
		t.Fatalf("third request failed after a slot was released: %v", err)
	case <-time.After(time.Second):
		t.Fatal("third request was not admitted after a slot was released")
	}
}

func TestLuaMillisecondsRejectsOverflowingInput(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	if _, err := luaMilliseconds(maxInt, "rate-limit interval", time.Millisecond); err == nil {
		t.Fatal("an overflowing millisecond input should be rejected before duration conversion")
	}
	if delay, err := luaMilliseconds(int(maxLuaWait.Milliseconds()), "sleep duration", 0); err != nil || delay != maxLuaWait {
		t.Fatalf("maximum allowed duration = %v, %v; want %v, nil", delay, err, maxLuaWait)
	}
}

func TestExecuteLuaScriptSleepStopsAtContextDeadline(t *testing.T) {
	startedAt := time.Now()
	_, err := executeLuaScript(`
local ok, sleepErr = sleep_ms(1000)
if sleepErr then return false, sleepErr end
return true, {items = {}, meta = {slept = ok}}
`, "https://example.com", 50*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want wrapped context deadline exceeded", err)
	}
	if elapsed := time.Since(startedAt); elapsed > 500*time.Millisecond {
		t.Fatalf("sleep ignored context deadline: %v", elapsed)
	}
}

func TestHTTPRequestsStopAtContextDeadline(t *testing.T) {
	tests := []struct {
		name string
		run  func(ctx context.Context, serverURL string) (*http.Response, error)
	}{
		{
			name: "direct request",
			run: func(ctx context.Context, serverURL string) (*http.Response, error) {
				return doDirectRequest(ctx, http.MethodGet, serverURL, "", "", "", time.Second)
			},
		},
		{
			name: "proxy request",
			run: func(ctx context.Context, serverURL string) (*http.Response, error) {
				return doProxyRequest(ctx, http.MethodGet, "http://example.com", "", "", "", serverURL, time.Second)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requestStarted := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				close(requestStarted)
				<-r.Context().Done()
			}))
			defer server.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			errCh := make(chan error, 1)
			go func() {
				_, err := tt.run(ctx, server.URL)
				errCh <- err
			}()

			select {
			case <-requestStarted:
			case <-time.After(time.Second):
				t.Fatal("test server did not receive the request")
			}

			startedAt := time.Now()
			err := <-errCh
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("error = %v, want wrapped context deadline exceeded", err)
			}
			if elapsed := time.Since(startedAt); elapsed > time.Second {
				t.Fatalf("request returned too late after deadline: %v", elapsed)
			}
		})
	}
}

func TestValidateTargetURL(t *testing.T) {
	tests := []struct {
		name    string
		rawURL  string
		wantErr bool
	}{
		{name: "public HTTPS domain", rawURL: "https://github.com/golang/go"},
		{name: "public IP", rawURL: "http://8.8.8.8"},
		{name: "unsupported scheme", rawURL: "file:///etc/passwd", wantErr: true},
		{name: "missing host", rawURL: "https:///only-a-path", wantErr: true},
		{name: "localhost", rawURL: "http://localhost:8080", wantErr: true},
		{name: "localhost subdomain", rawURL: "http://api.localhost", wantErr: true},
		{name: "IPv4 loopback", rawURL: "http://127.0.0.1", wantErr: true},
		{name: "IPv6 loopback", rawURL: "http://[::1]", wantErr: true},
		{name: "loopback IPv4", rawURL: "http://127.0.0.1:8080", wantErr: true},
		{name: "link-local IPv4", rawURL: "http://169.254.169.254", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := validateTargetURL(tt.rawURL)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateTargetURL(%q) error = %v, wantErr %v", tt.rawURL, err, tt.wantErr)
			}
		})
	}
}

func TestValidateRedirect(t *testing.T) {
	request, err := http.NewRequest(http.MethodGet, "http://127.0.0.1/internal", nil)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	if err := validateRedirect(request, nil); err == nil {
		t.Fatal("private redirect target should be rejected")
	}
}

func TestReadResponseBodyRejectsOversizedBody(t *testing.T) {
	tooLarge := bytes.Repeat([]byte("a"), int(maxHTTPResponseBodyBytes)+1)
	_, err := readResponseBody(bytes.NewReader(tooLarge), -1)
	if err == nil {
		t.Fatal("oversized response body should be rejected")
	}
}
