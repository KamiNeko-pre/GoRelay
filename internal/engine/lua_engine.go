package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	lua "github.com/yuin/gopher-lua"
)

const maxLuaWait = 60 * time.Second

func luaMilliseconds(value int, name string, minimum time.Duration) (time.Duration, error) {
	maxMilliseconds := maxLuaWait.Milliseconds()
	if int64(value) < minimum.Milliseconds() || int64(value) > maxMilliseconds {
		return 0, fmt.Errorf("%s must be between %d and %d milliseconds", name, minimum.Milliseconds(), maxMilliseconds)
	}
	return time.Duration(value) * time.Millisecond, nil
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

// SleepMS gives sandboxed Lua connectors a bounded, cancellation-aware backoff helper.
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

	// OpenBase provides common helpers, but file and dynamic-loading entry points
	// do not belong in an untrusted connector sandbox.
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

func decodeLuaResult(L *lua.LState, allowPartialItems bool) (luaScriptResult, error) {
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
	payload, err := decodeCollection(luaData, allowPartialItems)
	return luaScriptResult{Collection: payload}, err
}

func decodeLuaProbeResult(L *lua.LState, hasPrintLogs ...bool) (luaScriptResult, error) {
	hasLogs := len(hasPrintLogs) > 0 && hasPrintLogs[0]
	if L.GetTop() == 0 && hasLogs {
		return luaScriptResult{Collection: map[string]any{}}, nil
	}
	if L.GetTop() < 1 {
		return luaScriptResult{}, fmt.Errorf("Lua script must return [boolean, data or error]")
	}
	if !lua.LVAsBool(L.Get(1)) {
		if L.GetTop() >= 2 {
			return luaScriptResult{}, fmt.Errorf("%s", L.Get(2).String())
		}
		return luaScriptResult{}, fmt.Errorf("Lua probe returned false")
	}
	if L.GetTop() < 2 || L.Get(2) == lua.LNil {
		if hasLogs {
			return luaScriptResult{Collection: map[string]any{}}, nil
		}
		return luaScriptResult{}, fmt.Errorf("Lua script must return [boolean, data or error]")
	}
	payload, err := decodeProbeCollection(L.Get(2))
	return luaScriptResult{Collection: payload}, err
}

// domCache is task-local and holds only the latest parsed HTML document.
// Documents disappear with the Lua VM after execution completes.
type DOMDiagnostic struct {
	Code         string `json:"code"`
	API          string `json:"api"`
	Selector     string `json:"selector"`
	Attribute    string `json:"attribute,omitempty"`
	InputKind    string `json:"input_kind"`
	SourceDigest string `json:"source_digest"`
	Instruction  string `json:"instruction"`
}

// DOMObservation records selector matches verified against host-fetched HTML.
type DOMObservation struct {
	API          string `json:"api"`
	Selector     string `json:"selector"`
	Attribute    string `json:"attribute,omitempty"`
	SourceDigest string `json:"source_digest"`
	ValueDigest  string `json:"value_digest"`
	Matches      int    `json:"matches"`
}

type domInput struct{ kind, source string }

type domCache struct {
	html            string
	doc             *goquery.Document
	inputs          map[string]domInput
	observations    []DOMObservation
	diagnostics     []DOMDiagnostic
	seenDiagnostics map[string]bool
}

func newDOMCache() *domCache {
	return &domCache{inputs: map[string]domInput{}, seenDiagnostics: map[string]bool{}}
}

func (cache *domCache) observeSource(body string) {
	sum := sha256.Sum256([]byte(body))
	cache.inputs[body] = domInput{kind: "source_html", source: hex.EncodeToString(sum[:])}
}

func (cache *domCache) observeValue(input, value, kind string) {
	if owner, ok := cache.inputs[input]; ok && value != "" {
		if cache.inputs[value].kind == "source_html" {
			return
		}
		cache.inputs[value] = domInput{kind: kind, source: owner.source}
	}
}

func (cache *domCache) diagnostic(input, api, selector, attribute, code string) {
	owner, ok := cache.inputs[input]
	if !ok {
		return
	}
	hint := "Selector matched no nodes. Try standard structural selectors (e.g. main, article, section, table tr, ul li, h1-h3, a) or pattern matching on clean_text(body)/source_text(body) inside Lua."
	if owner.kind == "selected_text" {
		hint = "html_find and html_find_all return text. Use html_find_html or html_find_all_html to obtain markup for nested queries, or query the original fetched HTML directly."
	}
	if code == "DOM_ATTRIBUTE_MISSING" {
		hint = "Matched node lacks this attribute. Query a descendant element (such as 'a') or another attribute in Lua."
	}
	d := DOMDiagnostic{Code: code, API: api, Selector: selector, Attribute: attribute, InputKind: owner.kind, SourceDigest: owner.source, Instruction: hint}
	raw, _ := json.Marshal(d)
	key := string(raw)
	if !cache.seenDiagnostics[key] {
		cache.seenDiagnostics[key] = true
		cache.diagnostics = append(cache.diagnostics, d)
	}
}

func (cache *domCache) observeSelection(input, api, selector, attribute string, values []string) {
	owner, ok := cache.inputs[input]
	if !ok || owner.kind != "source_html" {
		return
	}
	unique := map[string]bool{}
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			unique[value] = true
		}
	}
	if len(unique) == 0 {
		return
	}
	canonical := make([]string, 0, len(unique))
	for value := range unique {
		canonical = append(canonical, value)
	}
	sort.Strings(canonical)
	raw, _ := json.Marshal(canonical)
	sum := sha256.Sum256(raw)
	observation := DOMObservation{API: api, Selector: selector, Attribute: attribute, SourceDigest: owner.source, ValueDigest: hex.EncodeToString(sum[:]), Matches: len(values)}
	for _, old := range cache.observations {
		if old == observation {
			return
		}
	}
	cache.observations = append(cache.observations, observation)
}

func wrapHTMLFragmentIfNeeded(raw string) string {
	trimmed := strings.TrimSpace(raw)
	lower := strings.ToLower(trimmed)
	switch {
	case strings.HasPrefix(lower, "<tr"), strings.HasPrefix(lower, "<tbody"), strings.HasPrefix(lower, "<thead"), strings.HasPrefix(lower, "<tfoot"):
		return "<table>" + trimmed + "</table>"
	case strings.HasPrefix(lower, "<td"), strings.HasPrefix(lower, "<th"):
		return "<table><tbody><tr>" + trimmed + "</tr></tbody></table>"
	default:
		return raw
	}
}

func (cache *domCache) document(html string) (*goquery.Document, error) {
	if cache.doc != nil && cache.html == html {
		return cache.doc, nil
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(wrapHTMLFragmentIfNeeded(html)))
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
	htmlstr := L.CheckString(1)
	selector := L.CheckString(2)
	doc, err := cache.document(htmlstr)
	if err != nil {
		L.Push(lua.LNil)
		L.Push(lua.LString("DOM parse failed: " + err.Error()))
		return 2
	}
	selection := doc.Find(selector).First()
	if selection.Length() == 0 {
		cache.diagnostic(htmlstr, "html_find", selector, "", "DOM_NO_MATCH")
		L.Push(lua.LNil)
		L.Push(lua.LString("no match for selector: " + selector))
		return 2
	}
	cleanText := strings.TrimSpace(selection.Text())
	cache.observeValue(htmlstr, cleanText, "selected_text")
	cache.observeSelection(htmlstr, "html_find", selector, "", []string{cleanText})
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

func (cache *domCache) htmlFindHTML(L *lua.LState) int {
	htmlText := L.CheckString(1)
	selector := L.CheckString(2)
	doc, err := cache.document(htmlText)
	if err != nil {
		L.Push(lua.LNil)
		L.Push(lua.LString(err.Error()))
		return 2
	}
	sel := doc.Find(selector).First()
	if sel.Length() == 0 {
		cache.diagnostic(htmlText, "html_find_html", selector, "", "DOM_NO_MATCH")
		L.Push(lua.LNil)
		L.Push(lua.LString("no match for selector: " + selector))
		return 2
	}
	fragment, err := goquery.OuterHtml(sel)
	if err != nil {
		L.Push(lua.LNil)
		L.Push(lua.LString(err.Error()))
		return 2
	}
	cache.observeValue(htmlText, fragment, "selected_html")
	L.Push(lua.LString(fragment))
	return 1
}

func (cache *domCache) htmlFindAllHTML(L *lua.LState) int {
	htmlText := L.CheckString(1)
	selector := L.CheckString(2)
	doc, err := cache.document(htmlText)
	if err != nil {
		L.Push(lua.LNil)
		L.Push(lua.LString(err.Error()))
		return 2
	}
	values := L.NewTable()
	matches := doc.Find(selector)
	if matches.Length() == 0 {
		cache.diagnostic(htmlText, "html_find_all_html", selector, "", "DOM_NO_MATCH")
	}
	matches.Each(func(_ int, sel *goquery.Selection) {
		if fragment, err := goquery.OuterHtml(sel); err == nil {
			cache.observeValue(htmlText, fragment, "selected_html")
			values.Append(lua.LString(fragment))
		}
	})
	L.Push(values)
	return 1
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
	selectedValues := []string{}
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
		cache.observeValue(htmlText, value, "selected_text")
		values.Append(lua.LString(value))
		selectedValues = append(selectedValues, value)
	})
	cache.observeSelection(htmlText, api, selector, attribute, selectedValues)
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
