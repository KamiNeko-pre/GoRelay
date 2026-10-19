package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/PuerkitoBio/goquery"
	lua "github.com/yuin/gopher-lua"
	"go-lua-crawler/internal/config"
	"go-lua-crawler/internal/logger"
	"go.uber.org/zap"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"
)

// routeCache 以 host 为键缓存最近一次可用的网络路径：true 表示代理，false 表示直连。
// sync.Map 适合多个 worker 高频读写且键集合不固定的场景。
var routeCache sync.Map

// hostRateSchedules coordinates request start times across all Lua VMs owned
// by this Worker process. Different hosts retain independent schedules.
var hostRateSchedules sync.Map
var hostHTTPGates sync.Map

const maxConcurrentRequestsPerHost = 2

// defaultUserAgent 使用主流浏览器标识，避免真实站点因 Go 默认 UA 直接拒绝匿名采集请求。
const defaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"

// applyBrowserHeaders 为沙箱出站请求补齐浏览器风格基础头，referer 可选。
func applyBrowserHeaders(request *http.Request, referer string) {
	if request.Header.Get("User-Agent") == "" {
		request.Header.Set("User-Agent", defaultUserAgent)
	}
	request.Header.Set("Accept", "text/html,application/xhtml+xml,application/json;q=0.9,*/*;q=0.8")
	request.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	if referer != "" {
		request.Header.Set("Referer", referer)
	}
}

func luaMilliseconds(value int, name string, minimum time.Duration) (time.Duration, error) {
	maxMilliseconds := maxLuaWait.Milliseconds()
	if int64(value) < minimum.Milliseconds() || int64(value) > maxMilliseconds {
		return 0, fmt.Errorf("%s must be between %d and %d milliseconds", name, minimum.Milliseconds(), maxMilliseconds)
	}
	return time.Duration(value) * time.Millisecond, nil
}

func acquireHostHTTPGate(ctx context.Context, host string) (func(), error) {
	value, _ := hostHTTPGates.LoadOrStore(host, make(chan struct{}, maxConcurrentRequestsPerHost))
	gate := value.(chan struct{})
	select {
	case gate <- struct{}{}:
		return func() { <-gate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// HttpGetLimited holds a host slot until the response body has been consumed.
// Limits are shared by Lua VMs in this process, not across containers.
func HttpGetLimited(L *lua.LState) int {
	rawURL := L.CheckString(1)
	parsed, err := validateTargetURL(rawURL)
	if err != nil {
		L.Push(lua.LNil)
		L.Push(lua.LString("URL validation failed: " + err.Error()))
		return 2
	}
	ctx := L.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	release, err := acquireHostHTTPGate(ctx, strings.ToLower(parsed.Hostname()))
	if err != nil {
		L.RaiseError("host slot wait interrupted: %v", ctx.Err())
		return 0
	}
	defer release()
	if n := RateLimit(L); n != 1 {
		return n
	}
	L.Pop(1)
	return performHTTP(L, http.MethodGet, rawURL, "", "", "")
}

const maxHTTPResponseBodyBytes int64 = 4 * 1024 * 1024
const maxLuaWait = 60 * time.Second

type hostRateSchedule struct {
	mu   sync.Mutex
	next time.Time
}

func waitForLuaContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// SleepMS gives trusted Lua rules a bounded, cancellation-aware backoff tool.
func SleepMS(L *lua.LState) int {
	delay, err := luaMilliseconds(L.CheckInt(1), "sleep duration", 0)
	if err != nil {
		L.Push(lua.LNil)
		L.Push(lua.LString(err.Error()))
		return 2
	}
	ctx := L.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	if err := waitForLuaContext(ctx, delay); err != nil {
		L.RaiseError("sleep interrupted: %v", err)
		return 0
	}
	L.Push(lua.LTrue)
	return 1
}

// RateLimit reserves the next request slot for a URL host. All Lua VMs in the
// process share that host schedule, while unrelated hosts remain independent.
func RateLimit(L *lua.LState) int {
	rawURL := L.CheckString(1)
	interval, err := luaMilliseconds(L.CheckInt(2), "rate-limit interval", time.Millisecond)
	if err != nil {
		L.Push(lua.LNil)
		L.Push(lua.LString(err.Error()))
		return 2
	}
	parsedURL, err := validateTargetURL(rawURL)
	if err != nil {
		L.Push(lua.LNil)
		L.Push(lua.LString("rate-limit URL validation failed: " + err.Error()))
		return 2
	}

	host := strings.ToLower(parsedURL.Hostname())
	value, _ := hostRateSchedules.LoadOrStore(host, &hostRateSchedule{})
	schedule := value.(*hostRateSchedule)
	now := time.Now()
	schedule.mu.Lock()
	reservedAt := now
	if schedule.next.After(reservedAt) {
		reservedAt = schedule.next
	}
	schedule.next = reservedAt.Add(interval)
	schedule.mu.Unlock()

	ctx := L.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	if err := waitForLuaContext(ctx, time.Until(reservedAt)); err != nil {
		L.RaiseError("rate-limit wait interrupted: %v", err)
		return 0
	}
	L.Push(lua.LTrue)
	return 1
}

func doDirectRequest(ctx context.Context, method, targetURL, body, contentType, referer string, timeout time.Duration) (*http.Response, error) {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, targetURL, reader)
	if err != nil {
		return nil, fmt.Errorf("create direct request: %w", err)
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	applyBrowserHeaders(request, referer)
	client := &http.Client{
		Timeout:       timeout,
		CheckRedirect: validateRedirect,
	}
	return client.Do(request)
}

func validateTargetURL(rawURL string) (*url.URL, error) {
	parsedURL, err := url.ParseRequestURI(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse target URL: %w", err)
	}
	if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
		return nil, fmt.Errorf("unsupported URL scheme %q", parsedURL.Scheme)
	}

	host := strings.ToLower(parsedURL.Hostname())
	if host == "" {
		return nil, fmt.Errorf("target URL must include a host")
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return nil, fmt.Errorf("localhost targets are not allowed")
	}

	ip, err := netip.ParseAddr(host)
	if err == nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ip.IsMulticast()) {
		return nil, fmt.Errorf("non-public IP target %q is not allowed", host)
	}

	return parsedURL, nil
}

func validateRedirect(request *http.Request, _ []*http.Request) error {
	if _, err := validateTargetURL(request.URL.String()); err != nil {
		return fmt.Errorf("reject redirect target: %w", err)
	}
	return nil
}

func readResponseBody(body io.Reader, contentLength int64) ([]byte, error) {
	if contentLength > maxHTTPResponseBodyBytes {
		return nil, fmt.Errorf("response body exceeds %d byte limit", maxHTTPResponseBodyBytes)
	}

	limitedBody := io.LimitReader(body, maxHTTPResponseBodyBytes+1)
	data, err := io.ReadAll(limitedBody)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}
	if int64(len(data)) > maxHTTPResponseBodyBytes {
		return nil, fmt.Errorf("response body exceeds %d byte limit", maxHTTPResponseBodyBytes)
	}
	return data, nil
}

// performHTTP is the shared request path for the Lua sandbox. It mirrors the
// previous GET-only flow but accepts an HTTP method and an optional form body,
// so rules can call POST endpoints (for example job-search APIs).
func performHTTP(L *lua.LState, method, targetUrl, body, contentType, referer string) int {
	ctx := L.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	cfg := config.Get()

	proxyStr := cfg.App.Proxy

	parsedURL, err := validateTargetURL(targetUrl)

	if err != nil {
		L.Push(lua.LNil)
		L.Push(lua.LString("URL validation failed: " + err.Error()))
		return 2
	}
	host := strings.ToLower(parsedURL.Hostname())

	useProxy := false

	// 已有路径记录时直接复用，减少每次请求先直连再回退代理的额外延迟。
	if val, ok := routeCache.Load(host); ok {
		useProxy = val.(bool)
	}
	var resp *http.Response
	var reqErr error
	proxyTimeout := time.Duration(cfg.Engine.HTTPTimeoutProxy) * time.Second
	if useProxy && proxyStr != "" {
		resp, reqErr = doProxyRequest(ctx, method, targetUrl, body, contentType, referer, proxyStr, proxyTimeout)
	} else {
		resp, reqErr = doDirectRequest(ctx, method, targetUrl, body, contentType, referer, time.Duration(cfg.Engine.HTTPTimeoutDirect)*time.Second)
		// 首次访问优先直连；直连失败且配置了代理时才降级到代理，
		// 成功后的选择会写入缓存，供同一 host 后续请求使用。
		if reqErr != nil && proxyStr != "" {
			logger.Log.Warn("直连失败, 启动代理并记录至路由表", zap.String("host", host))
			resp, reqErr = doProxyRequest(ctx, method, targetUrl, body, contentType, referer, proxyStr, proxyTimeout)
			if reqErr == nil {
				routeCache.Store(host, true)
			}
		} else if reqErr == nil {
			routeCache.Store(host, false)
		}
	}
	if reqErr != nil {
		L.Push(lua.LNil)
		L.Push(lua.LString(reqErr.Error()))
		return 2
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		L.Push(lua.LNil)
		L.Push(lua.LString(fmt.Sprintf("HTTP 状态异常: %d", resp.StatusCode)))
		return 2
	}

	respBody, err := readResponseBody(resp.Body, resp.ContentLength)
	if err != nil {
		L.Push(lua.LNil)
		L.Push(lua.LString(err.Error()))
		return 2
	}
	L.Push(lua.LString(string(respBody)))
	return 1
}

// HttpGet exposes a GET request to Lua. 成功返回 body，失败返回 nil 和错误信息。
func HttpGet(L *lua.LState) int {
	return performHTTP(L, http.MethodGet, L.CheckString(1), "", "", "")
}

// HttpPost exposes a form-encoded POST request to Lua.
// Usage: local body, err = http_post(url, "k=v&k2=v2")
func HttpPost(L *lua.LState) int {
	return performHTTP(L, http.MethodPost, L.CheckString(1), L.CheckString(2), "application/x-www-form-urlencoded", "")
}

// HttpPostJSON exposes a JSON-body POST request to Lua, with an optional referer.
// Usage: local body, err = http_post_json(url, '{"k":1}'[, 'https://example.com/'])
func HttpPostJSON(L *lua.LState) int {
	referer := ""
	if L.GetTop() >= 3 {
		referer = L.CheckString(3)
	}
	return performHTTP(L, http.MethodPost, L.CheckString(1), L.CheckString(2), "application/json", referer)
}

// HttpGetReferer issues a GET with a Referer header (some sites require a
// same-site referer for detail pages). Usage: local body, err = http_get_referer(url, ref)
func HttpGetReferer(L *lua.LState) int {
	referer := ""
	if L.GetTop() >= 2 {
		referer = L.CheckString(2)
	}
	return performHTTP(L, http.MethodGet, L.CheckString(1), "", "", referer)
}

func doProxyRequest(ctx context.Context, method, targetURL, body, contentType, referer, proxyStr string, timeout time.Duration) (*http.Response, error) {
	// 为代理请求单独设置超时，避免代理线路的波动占用直连请求的时间预算。
	proxyURL, err := url.Parse(proxyStr)
	if err != nil {
		return nil, fmt.Errorf("parse proxy URL: %w", err)
	}
	if proxyURL.Scheme == "" || proxyURL.Host == "" {
		return nil, fmt.Errorf("proxy URL must include scheme and host")
	}
	proxyClient := &http.Client{
		Timeout:       timeout,
		CheckRedirect: validateRedirect,
		Transport: &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
		},
	}
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, targetURL, reader)
	if err != nil {
		return nil, fmt.Errorf("create proxy request: %w", err)
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	applyBrowserHeaders(request, referer)
	return proxyClient.Do(request)

}

type luaScriptResult struct {
	Collection map[string]any `json:"-"`
}

func openLuaSandboxLibraries(L *lua.LState) error {
	libraries := []struct {
		name string
		open lua.LGFunction
	}{
		{lua.BaseLibName, lua.OpenBase},
		{lua.TabLibName, lua.OpenTable},
		{lua.StringLibName, lua.OpenString},
		{lua.MathLibName, lua.OpenMath},
	}

	for _, library := range libraries {
		if err := L.CallByParam(lua.P{
			Fn:      L.NewFunction(library.open),
			NRet:    0,
			Protect: true,
		}, lua.LString(library.name)); err != nil {
			return fmt.Errorf("open Lua library %q: %w", library.name, err)
		}
	}

	// OpenBase provides common helpers, but its file and dynamic-loading entry
	// points do not belong in a user-provided rule sandbox.
	for _, globalName := range []string{
		"dofile",
		"load",
		"loadfile",
		"loadstring",
		"require",
		"module",
		"collectgarbage",
		"_printregs",
		"newproxy",
	} {
		L.SetGlobal(globalName, lua.LNil)
	}

	return nil
}

// executeLuaScript owns one Lua VM for one task. It accepts timeout explicitly
// so the production wrapper can use configuration while tests stay fast.
func executeLuaScript(scriptText, targetURL string, timeout time.Duration) (luaScriptResult, error) {
	return executeLuaScriptContext(context.Background(), scriptText, targetURL, timeout)
}

func executeLuaScriptContext(parent context.Context, scriptText, targetURL string, timeout time.Duration) (result luaScriptResult, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			result = luaScriptResult{}
			err = fmt.Errorf("Lua execution panic: %v", recovered)
		}
	}()
	L := lua.NewState(lua.Options{SkipOpenLibs: true})
	defer L.Close()

	if err := openLuaSandboxLibraries(L); err != nil {
		return luaScriptResult{}, fmt.Errorf("initialize Lua sandbox: %w", err)
	}

	L.SetGlobal("http_get", L.NewFunction(HttpGet))
	L.SetGlobal("http_get_limited", L.NewFunction(HttpGetLimited))
	L.SetGlobal("http_get_referer", L.NewFunction(HttpGetReferer))
	L.SetGlobal("http_post", L.NewFunction(HttpPost))
	L.SetGlobal("http_post_json", L.NewFunction(HttpPostJSON))
	L.SetGlobal("rate_limit", L.NewFunction(RateLimit))
	L.SetGlobal("sleep_ms", L.NewFunction(SleepMS))
	// A Lua VM owns exactly one task. Keep only that task's most recently parsed
	// document so rules can make several selector queries without reparsing the
	// same response or retaining DOM trees across tasks.
	dom := newDOMCache()
	L.SetGlobal("html_find", L.NewFunction(dom.htmlFind))
	L.SetGlobal("html_find_all", L.NewFunction(dom.htmlFindAll))
	L.SetGlobal("html_attr_all", L.NewFunction(dom.htmlAttrAll))
	L.SetGlobal("url_resolve", L.NewFunction(URLResolve))
	L.SetGlobal("json_decode", L.NewFunction(JSONDecode))
	L.SetGlobal("TARGET_URL", lua.LString(targetURL))

	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	L.SetContext(ctx)

	if err := L.DoString(scriptText); err != nil {
		if parent.Err() != nil {
			return luaScriptResult{}, parent.Err()
		}
		if ctx.Err() == context.DeadlineExceeded {
			return luaScriptResult{}, fmt.Errorf("Lua script execution timed out after %v: %w", timeout, ctx.Err())
		}
		return luaScriptResult{}, fmt.Errorf("Lua sandbox execution failed: %w", err)
	}

	return decodeLuaResult(L)
}

func decodeLuaResult(L *lua.LState) (luaScriptResult, error) {
	if L.GetTop() < 2 {
		return luaScriptResult{}, fmt.Errorf("Lua script must return [boolean, data or error]")
	}
	if !lua.LVAsBool(L.Get(1)) {
		return luaScriptResult{}, fmt.Errorf("%s", L.Get(2).String())
	}

	luaData, ok := L.Get(2).(*lua.LTable)
	if !ok {
		return luaScriptResult{}, fmt.Errorf("Lua script success data must be a table")
	}
	payload, err := decodeCollection(luaData)
	return luaScriptResult{Collection: payload}, err
}

// domCache is deliberately task-local and holds only the latest HTML value.
// It is not a process-wide cache: documents disappear with the Lua VM after
// the task completes, so different targets cannot leak state into one another.
type DOMDiagnostic struct {
	Code         string `json:"code"`
	API          string `json:"api"`
	Selector     string `json:"selector"`
	Attribute    string `json:"attribute,omitempty"`
	InputKind    string `json:"input_kind"`
	SourceDigest string `json:"source_digest"`
	Instruction  string `json:"instruction"`
}
type domInput struct{ kind, source string }
type domCache struct {
	html            string
	doc             *goquery.Document
	inputs          map[string]domInput
	diagnostics     []DOMDiagnostic
	seenDiagnostics map[string]bool
}

func newDOMCache() *domCache {
	return &domCache{inputs: map[string]domInput{}, seenDiagnostics: map[string]bool{}}
}

// Only successful HTTP bodies and values actually selected from them carry
// source ownership. Lua report strings and invented markup cannot mint it.
func (cache *domCache) observeSource(body string) {
	sum := sha256.Sum256([]byte(body))
	cache.inputs[body] = domInput{kind: "source_html", source: hex.EncodeToString(sum[:])}
}
func (cache *domCache) observeText(input, text string) {
	if owner, ok := cache.inputs[input]; ok && text != "" {
		cache.inputs[text] = domInput{kind: "selected_text", source: owner.source}
	}
}
func (cache *domCache) diagnostic(input, api, selector, attribute, code string) {
	owner, ok := cache.inputs[input]
	if !ok {
		return
	}
	hint := "The selector matched no node in the actual source. Inspect its observed selector inventory before changing the extractor."
	if owner.kind == "selected_text" {
		hint = "html_find and html_find_all return text, not HTML fragments. Query the original fetched HTML with the observed selector; do not parse returned row text as markup. Use document-level field selectors and observed row identities, retaining exact field locators."
	}
	if code == "DOM_ATTRIBUTE_MISSING" {
		hint = "A matched source node lacks the requested attribute. Inspect its actual attributes or child link before choosing the attribute locator."
	}
	d := DOMDiagnostic{Code: code, API: api, Selector: selector, Attribute: attribute, InputKind: owner.kind, SourceDigest: owner.source, Instruction: hint}
	raw, _ := json.Marshal(d)
	key := string(raw)
	if !cache.seenDiagnostics[key] {
		cache.seenDiagnostics[key] = true
		cache.diagnostics = append(cache.diagnostics, d)
	}
}

func (cache *domCache) document(html string) (*goquery.Document, error) {
	if cache.doc != nil && cache.html == html {
		return cache.doc, nil
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return nil, err
	}
	cache.html, cache.doc = html, doc
	return doc, nil
}

func HtmlFind(L *lua.LState) int {
	return newDOMCache().htmlFind(L)
}

func (cache *domCache) htmlFind(L *lua.LState) int {
	// html_find(html, selector) 负责将 HTML 解析和 CSS 选择器能力提供给 Lua。
	// 与 http_get 一样，失败时返回 nil 和错误信息，成功时返回第一个匹配元素的文本。
	htmlstr := L.CheckString(1)
	selector := L.CheckString(2)
	doc, err := cache.document(htmlstr)
	if err != nil {
		L.Push(lua.LNil)
		L.Push(lua.LString("DOM树构建失败: " + err.Error()))
		return 2
	}
	selection := doc.Find(selector).First()

	if selection.Length() == 0 {
		cache.diagnostic(htmlstr, "html_find", selector, "", "DOM_NO_MATCH")
		L.Push(lua.LNil)
		L.Push(lua.LString("在网页中未找到匹配的元素: " + selector))
		return 2
	}
	cleanText := strings.TrimSpace(selection.Text())
	cache.observeText(htmlstr, cleanText)
	L.Push(lua.LString(cleanText))
	return 1

}

func HtmlFindAll(L *lua.LState) int {
	return newDOMCache().htmlFindAll(L)
}

func HtmlAttrAll(L *lua.LState) int {
	return newDOMCache().htmlAttrAll(L)
}

func (cache *domCache) htmlFindAll(L *lua.LState) int {
	return cache.htmlSelectAll(L, "")
}

func (cache *domCache) htmlAttrAll(L *lua.LState) int {
	return cache.htmlSelectAll(L, L.CheckString(3))
}

func (cache *domCache) htmlSelectAll(L *lua.LState, attribute string) int {
	htmlText := L.CheckString(1)
	selector := L.CheckString(2)
	doc, err := cache.document(htmlText)
	if err != nil {
		L.Push(lua.LNil)
		L.Push(lua.LString(err.Error()))
		return 2
	}
	values := L.NewTable()
	selectionSet := doc.Find(selector)
	api := "html_find_all"
	if attribute != "" {
		api = "html_attr_all"
	}
	if selectionSet.Length() == 0 {
		cache.diagnostic(htmlText, api, selector, attribute, "DOM_NO_MATCH")
	}
	selectionSet.Each(func(_ int, selection *goquery.Selection) {
		value := strings.TrimSpace(selection.Text())
		if attribute != "" {
			var present bool
			value, present = selection.Attr(attribute)
			if !present {
				cache.diagnostic(htmlText, api, selector, attribute, "DOM_ATTRIBUTE_MISSING")
			}
			value = strings.TrimSpace(value)
		}
		cache.observeText(htmlText, value)
		values.Append(lua.LString(value))
	})
	L.Push(values)
	return 1
}

func URLResolve(L *lua.LState) int {
	base, err := url.Parse(L.CheckString(1))
	if err != nil {
		L.Push(lua.LNil)
		L.Push(lua.LString(err.Error()))
		return 2
	}
	reference, err := url.Parse(L.CheckString(2))
	if err != nil {
		L.Push(lua.LNil)
		L.Push(lua.LString(err.Error()))
		return 2
	}
	L.Push(lua.LString(base.ResolveReference(reference).String()))
	return 1
}
