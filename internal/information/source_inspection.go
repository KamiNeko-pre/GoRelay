package information

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/PuerkitoBio/goquery"
	"github.com/andybalholm/cascadia"
	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/knowledge"
	"golang.org/x/net/html"
)

func acquisitionResponseMetadata(response acquisition.Response) map[string]any {
	contentType := response.Receipt.Headers["content-type"]
	out := map[string]any{"url": response.Receipt.URL, "status": response.Receipt.Status, "content_type": contentType}
	trimmed := strings.TrimSpace(response.Body)
	if trimmed == "" {
		return out
	}
	var value any
	if json.Unmarshal([]byte(trimmed), &value) == nil {
		out["is_json"] = true
		out["json_paths"] = observedJSONPaths(value, "")
		object, _ := value.(map[string]any)
		if array, ok := value.([]any); ok {
			out["json_array_length"] = len(array)
			for _, item := range array {
				if object, ok = item.(map[string]any); ok {
					break
				}
			}
		}
		if len(object) > 0 {
			keys := make([]string, 0, len(object))
			for key := range object {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			out["json_keys"] = keys
		}
		return out
	}
	lowerCT := strings.ToLower(contentType)
	if isXMLFeedOrDocument(lowerCT, trimmed) {
		out["is_xml"] = true
		if outline := summarizeXMLStructure(trimmed); len(outline) > 0 {
			out["xml_outline"] = outline
		}
		return out
	}
	if strings.Contains(lowerCT, "html") || strings.Contains(lowerCT, "xml") || strings.Contains(trimmed, "<") {
		out["is_html"] = true
		if outline := summarizeHTMLStructure(response); len(outline) > 0 {
			out["html_outline"] = outline
		}
	}
	return out
}

func isXMLFeedOrDocument(lowerCT, trimmed string) bool {
	if strings.Contains(lowerCT, "html") {
		return false
	}
	if strings.Contains(lowerCT, "xml") || strings.Contains(lowerCT, "atom") || strings.Contains(lowerCT, "rss") {
		return true
	}
	lowerHead := strings.ToLower(trimmed)
	if len(lowerHead) > 256 {
		lowerHead = lowerHead[:256]
	}
	return strings.HasPrefix(lowerHead, "<?xml") ||
		strings.HasPrefix(lowerHead, "<feed") ||
		strings.HasPrefix(lowerHead, "<rss") ||
		strings.HasPrefix(lowerHead, "<urlset")
}

func summarizeXMLStructure(raw string) map[string]any {
	dec := xml.NewDecoder(strings.NewReader(raw))
	dec.Strict = false
	var stack []string
	rootTag := ""
	childCounts := map[string]int{}
	childOrder := []string{}
	firstItemTag := ""
	firstItemDepth := -1
	inFirstItem := false
	itemFields := map[string]string{}
	itemFieldOrder := []string{}
	itemLinkAttrs := []string{}
	seenLinkAttr := map[string]bool{}
	var currentField string

	for {
		tok, err := dec.Token()
		if err != nil {
			if err == io.EOF {
				break
			}
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			name := t.Name.Local
			if name == "" {
				continue
			}
			stack = append(stack, name)
			depth := len(stack)
			if depth == 1 && rootTag == "" {
				rootTag = name
			} else if depth == 2 || (depth == 3 && (stack[1] == "channel")) {
				path := strings.Join(stack[1:], "/")
				if childCounts[path] == 0 {
					childOrder = append(childOrder, path)
				}
				childCounts[path]++
				if firstItemTag == "" && (name == "entry" || name == "item" || name == "url" || childCounts[path] == 2) {
					firstItemTag = path
					firstItemDepth = depth
					inFirstItem = childCounts[path] == 1
				}
			} else if inFirstItem && depth == firstItemDepth+1 {
				currentField = name
				if _, exists := itemFields[name]; !exists {
					itemFieldOrder = append(itemFieldOrder, name)
					itemFields[name] = "element"
				}
				for _, attr := range t.Attr {
					an := strings.ToLower(attr.Name.Local)
					if an == "href" || an == "url" || an == "rel" {
						tagAttr := name + "[@" + an + "]"
						if !seenLinkAttr[tagAttr] {
							seenLinkAttr[tagAttr] = true
							itemLinkAttrs = append(itemLinkAttrs, tagAttr)
						}
					}
				}
			}
		case xml.CharData:
			if inFirstItem && currentField != "" {
				if txt := strings.TrimSpace(string(t)); txt != "" {
					kind, _ := classifyOutlineShape(txt)
					if kind != "" {
						itemFields[currentField] = kind
					}
				}
			}
		case xml.EndElement:
			if len(stack) == 0 {
				continue
			}
			if inFirstItem && len(stack) == firstItemDepth {
				inFirstItem = false
				currentField = ""
			} else if inFirstItem && len(stack) == firstItemDepth+1 {
				currentField = ""
			}
			stack = stack[:len(stack)-1]
		}
	}
	if rootTag == "" {
		return nil
	}
	out := map[string]any{"root_tag": rootTag}
	var repeating []map[string]any
	for _, p := range childOrder {
		if childCounts[p] >= 2 {
			repeating = append(repeating, map[string]any{"path": p, "count": childCounts[p]})
		}
	}
	if len(repeating) > 0 {
		out["repeating_elements"] = repeating
	}
	if len(itemFieldOrder) > 0 {
		fields := make(map[string]string, len(itemFieldOrder))
		for _, k := range itemFieldOrder {
			fields[k] = itemFields[k]
		}
		out["item_fields"] = fields
	}
	if len(itemLinkAttrs) > 0 {
		out["item_link_attributes"] = itemLinkAttrs
	}
	return out
}

func summarizeHTMLStructure(response acquisition.Response) map[string]any {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(response.Body))
	if err != nil {
		return nil
	}
	outline := map[string]any{}
	if reason, blocked := browserFallbackReason(response); blocked {
		outline["page_hint"] = reason
	}
	embedded := extractEmbeddedScriptHints(doc)
	if len(embedded) > 0 {
		outline["embedded_json"] = embedded
	}
	if scripts := extractAppScriptSrcs(doc, response.Receipt.URL, 1000); len(scripts) > 0 {
		outline["script_srcs"] = scripts
	}
	if endpoints, epPaths := extractDataEndpointURLs(doc, 1600); len(endpoints) > 0 || len(epPaths) > 0 {
		if len(endpoints) > 0 {
			outline["data_endpoints"] = endpoints
		}
		if len(epPaths) > 0 {
			outline["data_endpoint_paths"] = epPaths
			outline["data_endpoint_paths_hint"] = "Observed configuration fragments, not fetchable URLs. Use the exact URL from a link, documented endpoint, or the code that assembles the request."
		}
	}
	if tmplCount := countUnrenderedTemplateTokens(doc); tmplCount >= 2 {
		outline["unrendered_templates"] = tmplCount
		if _, hasHint := outline["page_hint"]; !hasHint {
			outline["page_hint"] = "static HTML contains unrendered template placeholders; inspect data_endpoints, embedded_json, or script config for structured data"
		}
	}
	doc.Find("script,style,noscript,template,svg,canvas,iframe,[aria-hidden=true],[hidden]").Remove()

	headings := collectOutlineHeadings(doc, 8)
	if len(headings) > 0 {
		outline["headings"] = headings
	}
	if links := collectRelatedPageLinks(doc, response.Receipt.URL, 1600); len(links) > 0 {
		outline["related_links"] = links
	}
	blocks := collectOutlineBlocks(doc, 10)
	if len(blocks) > 0 {
		outline["blocks"] = blocks
	}
	return outline
}

func classifyOutlineShape(raw string) (string, int) {
	collapsed := strings.Join(strings.Fields(raw), " ")
	if collapsed == "" {
		return "", 0
	}
	length := len([]rune(collapsed))
	lower := strings.ToLower(collapsed)
	switch {
	case strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://"):
		return "url", length
	case looksLikeDateText(collapsed):
		return "date", length
	case looksLikeNumericText(collapsed):
		return "number", length
	default:
		return "text", length
	}
}

func looksLikeDateText(s string) bool {
	if len(s) > 40 {
		return false
	}
	digits := 0
	hasSep := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= '0' && c <= '9' {
			digits++
		} else if c == '-' || c == '/' {
			hasSep = true
		}
	}
	return hasSep && digits >= 6 && digits <= 8
}

func looksLikeNumericText(s string) bool {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" || len(trimmed) > 24 {
		return false
	}
	digits := 0
	for i := 0; i < len(trimmed); i++ {
		c := trimmed[i]
		if c >= '0' && c <= '9' {
			digits++
		} else if !strings.ContainsRune(".,+-/%$€£¥ ", rune(c)) {
			return false
		}
	}
	return digits > 0
}

func extractEmbeddedScriptHints(doc *goquery.Document) []map[string]any {
	var hints []map[string]any
	seen := map[string]bool{}
	addHint := func(key string, item map[string]any) {
		if key == "" || seen[key] || len(hints) >= 8 {
			return
		}
		seen[key] = true
		hints = append(hints, item)
	}
	doc.Find("script").EachWithBreak(func(idx int, s *goquery.Selection) bool {
		scriptNum := idx + 1
		id, _ := s.Attr("id")
		kind, _ := s.Attr("type")
		switch {
		case id == "__NEXT_DATA__" || id == "__NUXT_DATA__" || strings.Contains(strings.ToLower(kind), "ld+json"):
			sel := `script[type="` + kind + `"]`
			if id != "" {
				sel = "script#" + id
			}
			addHint(sel, map[string]any{"selector": sel, "script_index": scriptNum, "bytes": len(s.Text())})
		default:
			if src, ok := s.Attr("src"); ok && strings.TrimSpace(src) != "" {
				return true
			}
			text := s.Text()
			if len(text) > 0 && len(text) < 200000 {
				if strings.Contains(text, "__INITIAL_STATE__") || strings.Contains(text, "__NUXT__") {
					addHint(fmt.Sprintf("state:%d", scriptNum), map[string]any{"selector": "script", "script_index": scriptNum, "var": "__INITIAL_STATE__", "bytes": len(text)})
				}
				for _, varName := range detectWindowConfigVars(text, 4) {
					fullVar := "window." + varName
					item := map[string]any{
						"selector":     "script",
						"script_index": scriptNum,
						"var":          fullVar,
						"bytes":        len(text),
					}
					if keys := extractInlineJSKeys(text, fullVar, 16); len(keys) > 0 {
						item["keys"] = keys
					}
					addHint("var:"+fullVar, item)
				}
			}
		}
		return len(hints) < 8
	})
	return hints
}

func extractInlineJSKeys(script, varName string, limit int) []string {
	rhs, ok := inlineJSVarObjectSlice(script, varName)
	if !ok {
		return nil
	}
	return scanTopLevelJSObjectKeys(rhs, limit)
}

func inlineJSVarObjectSlice(script, varName string) (string, bool) {
	pos := strings.Index(script, varName)
	if pos < 0 {
		return "", false
	}
	after := strings.TrimLeft(script[pos+len(varName):], " \t\r\n")
	if !strings.HasPrefix(after, "=") || strings.HasPrefix(after, "==") {
		return "", false
	}
	rhs := strings.TrimLeft(after[1:], " \t\r\n")
	if strings.HasPrefix(rhs, "{") {
		return extractBalancedBraceBlock(rhs, 16384)
	}
	end := 0
	for end < len(rhs) && isJSIdentChar(rhs[end]) {
		end++
	}
	if end == 0 {
		return "", false
	}
	ident := rhs[:end]
	for _, kw := range []string{"const ", "let ", "var "} {
		needle := kw + ident
		if idx := strings.LastIndex(script[:pos], needle); idx >= 0 {
			decl := strings.TrimLeft(script[idx+len(needle):pos], " \t\r\n")
			if strings.HasPrefix(decl, "=") {
				val := strings.TrimLeft(decl[1:], " \t\r\n")
				if strings.HasPrefix(val, "{") {
					return extractBalancedBraceBlock(val, 16384)
				}
			}
		}
	}
	return "", false
}

func isJSIdentChar(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '$'
}

func extractBalancedBraceBlock(s string, maxLen int) (string, bool) {
	if !strings.HasPrefix(s, "{") {
		return "", false
	}
	depth := 0
	var quote byte
	escaped := false
	limit := min(len(s), maxLen)
	for i := 0; i < limit; i++ {
		c := s[i]
		if quote != 0 {
			if escaped {
				escaped = false
				continue
			}
			if c == '\\' {
				escaped = true
				continue
			}
			if c == quote {
				quote = 0
			}
			continue
		}
		if c == '\'' || c == '"' || c == '`' {
			quote = c
			continue
		}
		if c == '{' {
			depth++
		} else if c == '}' {
			depth--
			if depth == 0 {
				return s[:i+1], true
			}
		}
	}
	return "", false
}

func scanTopLevelJSObjectKeys(block string, limit int) []string {
	var keys []string
	seen := map[string]bool{}
	depth := 0
	expectKey := false
	i := 0
	for i < len(block) && len(keys) < limit {
		c := block[i]
		if c == '/' && i+1 < len(block) {
			if block[i+1] == '/' {
				if nl := strings.IndexByte(block[i:], '\n'); nl >= 0 {
					i += nl + 1
					continue
				}
				break
			}
			if block[i+1] == '*' {
				if end := strings.Index(block[i+2:], "*/"); end >= 0 {
					i += 2 + end + 2
					continue
				}
				break
			}
		}
		if c == '\'' || c == '"' {
			q := c
			j := i + 1
			for j < len(block) {
				if block[j] == '\\' {
					j += 2
					continue
				}
				if block[j] == q {
					break
				}
				j++
			}
			if j < len(block) {
				k := block[i+1 : j]
				after := strings.TrimLeft(block[j+1:], " \t\r\n")
				if depth == 1 && expectKey && strings.HasPrefix(after, ":") && k != "" && len(k) <= 48 && !seen[k] {
					seen[k] = true
					keys = append(keys, k)
					expectKey = false
				}
				i = j + 1
				continue
			}
		}
		if c == '{' {
			depth++
			if depth == 1 {
				expectKey = true
			}
			i++
			continue
		}
		if c == '}' {
			depth--
			i++
			continue
		}
		if depth == 1 && c == ',' {
			expectKey = true
			i++
			continue
		}
		if depth == 1 && expectKey && isJSIdentChar(c) {
			j := i + 1
			for j < len(block) && (isJSIdentChar(block[j]) || block[j] == '-') {
				j++
			}
			k := block[i:j]
			after := strings.TrimLeft(block[j:], " \t\r\n")
			if strings.HasPrefix(after, ":") && k != "" && len(k) <= 48 && !seen[k] {
				seen[k] = true
				keys = append(keys, k)
				expectKey = false
			}
			i = j
			continue
		}
		if c == ':' && depth == 1 {
			expectKey = false
		}
		i++
	}
	return keys
}

func detectWindowConfigVars(script string, limit int) []string {
	var vars []string
	seen := map[string]bool{}
	idx := strings.Index(script, "window.")
	for idx >= 0 && len(vars) < limit {
		rest := script[idx+len("window."):]
		end := 0
		for end < len(rest) && end < 48 {
			c := rest[end]
			if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '.' || c == '$' {
				end++
				continue
			}
			break
		}
		if end > 0 {
			after := strings.TrimLeft(rest[end:], " \t\r\n")
			if strings.HasPrefix(after, "=") && !strings.HasPrefix(after, "==") {
				rhs := strings.TrimLeft(after[1:], " \t\r\n")
				accept := strings.HasPrefix(rhs, "{") || strings.HasPrefix(rhs, "[")
				if !accept && len(rhs) > 0 && ((rhs[0] >= 'A' && rhs[0] <= 'Z') || rhs[0] == '_') {
					tokEnd := 0
					for tokEnd < len(rhs) && ((rhs[tokEnd] >= 'a' && rhs[tokEnd] <= 'z') || (rhs[tokEnd] >= 'A' && rhs[tokEnd] <= 'Z') || (rhs[tokEnd] >= '0' && rhs[tokEnd] <= '9') || rhs[tokEnd] == '_') {
						tokEnd++
					}
					tok := rhs[:tokEnd]
					if tok != "" && tok != "undefined" && tok != "null" && tok != "true" && tok != "false" {
						accept = true
					}
				}
				if accept {
					name := rest[:end]
					if !seen[name] {
						seen[name] = true
						vars = append(vars, name)
					}
				}
			}
		}
		next := strings.Index(rest, "window.")
		if next < 0 {
			break
		}
		idx += len("window.") + next
	}
	return vars
}

func looksLikeOpaqueToken(p string) bool {
	if slash := strings.LastIndexByte(p, '/'); slash >= 0 && len(p)-slash-1 >= 24 {
		leaf := p[slash+1:]
		return !strings.Contains(leaf, "-") && !strings.Contains(leaf, "_")
	}
	return len(p) >= 28 && !strings.Contains(p, "-")
}

func packStringsByByteBudget(items []string, budgetBytes int) []string {
	if len(items) == 0 {
		return nil
	}
	out := make([]string, 0, len(items))
	used := 2
	for _, s := range items {
		cost := len(s) + 3
		if len(out) > 0 && budgetBytes > 0 && used+cost > budgetBytes {
			break
		}
		used += cost
		out = append(out, s)
	}
	return out
}

var inlineScriptJSRefPattern = regexp.MustCompile(`(?i)["']((?:https?://|/|\./|\.\./)?[A-Za-z0-9_./-]+\.js(?:\?[^"'<>\s]*)?)["']`)
var inlineDynamicBaseHrefPattern = regexp.MustCompile(`(?i)['"](/[A-Za-z0-9_.-]+/)['"]`)

func resolveDocumentBaseURL(doc *goquery.Document, baseRaw string) *url.URL {
	base, err := url.Parse(baseRaw)
	if err != nil || base.Hostname() == "" {
		return nil
	}
	if href, exists := doc.Find("base[href]").First().Attr("href"); exists {
		if ref, err := url.Parse(strings.TrimSpace(href)); err == nil {
			resolved := base.ResolveReference(ref)
			if resolved.Hostname() != "" {
				return resolved
			}
		}
	}
	var dynamicBase *url.URL
	doc.Find("script:not([src])").EachWithBreak(func(_ int, s *goquery.Selection) bool {
		body := s.Text()
		if !strings.Contains(strings.ToLower(body), "<base") {
			return true
		}
		for _, m := range inlineDynamicBaseHrefPattern.FindAllStringSubmatch(body, 4) {
			if len(m) >= 2 && m[1] != "/" {
				if ref, err := url.Parse(m[1]); err == nil {
					dynamicBase = base.ResolveReference(ref)
					return false
				}
			}
		}
		return true
	})
	if dynamicBase != nil {
		return dynamicBase
	}
	return base
}

func extractAppScriptSrcs(doc *goquery.Document, baseRaw string, budgetBytes int) []string {
	base := resolveDocumentBaseURL(doc, baseRaw)
	if base == nil || base.Hostname() == "" {
		return nil
	}
	seen := map[string]bool{}
	var sameHostApp, sameHostOther, crossHost []string
	recordCandidate := func(raw string) {
		raw = strings.TrimSpace(raw)
		if raw == "" || strings.HasPrefix(raw, "data:") || strings.HasPrefix(raw, "javascript:") {
			return
		}
		// Strip trailing unclosed template/query concatenation noise like "?v="
		raw = strings.TrimRight(raw, "?&=")
		ref, err := url.Parse(raw)
		if err != nil {
			return
		}
		resolved := base.ResolveReference(ref)
		resolved.Fragment = ""
		if resolved.Scheme != "http" && resolved.Scheme != "https" {
			return
		}
		clean := resolved.String()
		if seen[clean] {
			return
		}
		seen[clean] = true
		lower := strings.ToLower(clean)
		isNoise := strings.Contains(lower, "gtm.js") || strings.Contains(lower, "analytics") ||
			strings.Contains(lower, "sentry") || strings.Contains(lower, "gtag") ||
			strings.Contains(lower, "onetrack") || strings.Contains(lower, "baidu.com") ||
			strings.Contains(lower, "hm.js")
		if strings.EqualFold(resolved.Hostname(), base.Hostname()) {
			if !isNoise {
				sameHostApp = append(sameHostApp, clean)
			} else {
				sameHostOther = append(sameHostOther, clean)
			}
			return
		}
		if !isNoise {
			crossHost = append(crossHost, clean)
		}
	}
	doc.Find("script[src]").Each(func(_ int, s *goquery.Selection) {
		if raw, exists := s.Attr("src"); exists {
			recordCandidate(raw)
		}
	})
	doc.Find("script:not([src])").Each(func(_ int, s *goquery.Selection) {
		body := s.Text()
		if len(body) == 0 || !strings.Contains(strings.ToLower(body), ".js") {
			return
		}
		if len(body) > 32768 {
			body = body[:32768]
		}
		for _, m := range inlineScriptJSRefPattern.FindAllStringSubmatch(body, 24) {
			if len(m) >= 2 {
				cand := strings.TrimSpace(m[1])
				// Require either a path separator or explicit URL scheme so bare module names like "a.js" aren't bogus relative paths unless clearly script loaders.
				if strings.Contains(cand, "/") || strings.Contains(strings.ToLower(body), "document.write") || strings.Contains(strings.ToLower(body), ".src") {
					recordCandidate(cand)
				}
			}
		}
	})
	ordered := make([]string, 0, len(sameHostApp)+len(sameHostOther)+len(crossHost))
	ordered = append(ordered, sameHostApp...)
	ordered = append(ordered, crossHost...)
	ordered = append(ordered, sameHostOther...)
	return packStringsByByteBudget(ordered, budgetBytes)
}

func extractDataEndpointURLs(doc *goquery.Document, budgetBytes int) ([]string, []string) {
	seenURLs := map[string]bool{}
	seenPaths := map[string]bool{}
	var urls []string
	var readablePaths []string
	var opaquePaths []string
	doc.Find("*").Each(func(_ int, s *goquery.Selection) {
		if len(s.Nodes) == 0 {
			return
		}
		for _, attr := range s.Nodes[0].Attr {
			key := strings.ToLower(attr.Key)
			if !strings.HasPrefix(key, "data-") {
				continue
			}
			if strings.Contains(key, "paths") {
				var arr []string
				if json.Unmarshal([]byte(attr.Val), &arr) == nil {
					for _, item := range arr {
						p := strings.TrimSpace(item)
						if p == "" || len(p) > 64 || strings.ContainsAny(p, " \t\r\n") || seenPaths[p] {
							continue
						}
						seenPaths[p] = true
						if looksLikeOpaqueToken(p) {
							opaquePaths = append(opaquePaths, p)
						} else {
							readablePaths = append(readablePaths, p)
						}
					}
				}
				continue
			}
			if !strings.Contains(key, "endpoint") && !strings.Contains(key, "api") &&
				!strings.Contains(key, "prefix") && !strings.Contains(key, "fetch") &&
				!strings.Contains(key, "json") && !strings.Contains(key, "feed") &&
				!strings.Contains(key, "url") && !strings.Contains(key, "config") {
				continue
			}
			val := strings.TrimSpace(attr.Val)
			lowerVal := strings.ToLower(val)
			if !strings.HasPrefix(lowerVal, "https://") && !strings.HasPrefix(lowerVal, "http://") {
				continue
			}
			if idx := strings.IndexAny(lowerVal, "?#"); idx > 0 {
				lowerVal = lowerVal[:idx]
			}
			if strings.HasSuffix(lowerVal, ".js") || strings.HasSuffix(lowerVal, ".css") ||
				strings.HasSuffix(lowerVal, ".png") || strings.HasSuffix(lowerVal, ".jpg") ||
				strings.HasSuffix(lowerVal, ".svg") || strings.HasSuffix(lowerVal, ".woff2") {
				continue
			}
			if !seenURLs[val] {
				seenURLs[val] = true
				urls = append(urls, val)
			}
		}
	})
	allPaths := append(readablePaths, opaquePaths...)
	return packStringsByByteBudget(urls, budgetBytes/2), packStringsByByteBudget(allPaths, budgetBytes)
}

func isTrackingQueryParam(key string) bool {
	k := strings.ToLower(strings.TrimSpace(key))
	return strings.HasPrefix(k, "utm_") || k == "nc" || k == "loc" || k == "referrer" || k == "ref" || k == "trk" || k == "sc_channel" || k == "fbclid" || k == "gclid"
}

func canonicalLinkPathKey(u *url.URL) string {
	if u == nil {
		return ""
	}
	q := u.Query()
	for k := range q {
		if isTrackingQueryParam(k) {
			q.Del(k)
		}
	}
	p := strings.TrimRight(u.Path, "/")
	if p == "" {
		p = "/"
	}
	if enc := q.Encode(); enc != "" {
		return p + "?" + enc
	}
	return p
}

func sectionPrefixForPath(path string) string {
	trimmed := strings.TrimRight(path, "/")
	if idx := strings.LastIndexByte(trimmed, '/'); idx > 0 {
		return trimmed[:idx+1]
	}
	return ""
}

func collectRelatedPageLinks(doc *goquery.Document, baseRaw string, budgetBytes int) []string {
	base, err := url.Parse(baseRaw)
	if err != nil || base.Hostname() == "" {
		return nil
	}
	baseCanonical := canonicalLinkPathKey(base)
	parentPrefix := sectionPrefixForPath(base.Path)

	type linkCandidate struct {
		url   string
		tier  int // lower is higher structural priority
		order int
	}
	byKey := map[string]linkCandidate{}
	order := 0
	doc.Find("a[href]").Each(func(_ int, a *goquery.Selection) {
		if inChromeNoiseRegion(a) {
			return
		}
		href, ok := a.Attr("href")
		if !ok {
			return
		}
		href = strings.TrimSpace(href)
		if href == "" || strings.HasPrefix(href, "#") || strings.HasPrefix(strings.ToLower(href), "javascript:") || strings.HasPrefix(strings.ToLower(href), "mailto:") {
			return
		}
		ref, err := url.Parse(href)
		if err != nil {
			return
		}
		resolved := base.ResolveReference(ref)
		resolved.Fragment = ""
		if (resolved.Scheme != "http" && resolved.Scheme != "https") || !strings.EqualFold(resolved.Hostname(), base.Hostname()) {
			return
		}
		if resolved.Path == "" || resolved.Path == "/" || canonicalLinkPathKey(resolved) == baseCanonical {
			return
		}
		lower := strings.ToLower(resolved.Path)
		for _, skip := range []string{".css", ".js", ".png", ".jpg", ".jpeg", ".svg", ".gif", ".ico", ".woff", ".pdf", ".zip", "/login", "/signin", "/sign-in", "/sign-up", "/signup", "/contact"} {
			if strings.Contains(lower, skip) {
				return
			}
		}

		sameSection := parentPrefix != "" && strings.HasPrefix(resolved.Path, parentPrefix)
		inBodyProse := a.ParentsFiltered("main,article,[role=main],p,td,th,li,blockquote").Length() > 0
		isMarketingHub := false
		for _, marketing := range []string{"/blog/", "/news/", "/press", "/partners", "/careers", "/about", "/legal", "/privacy", "/terms", "/cookies", "/events", "/webinars", "/customers", "/industries/", "/solutions/"} {
			if strings.Contains(lower, marketing) {
				isMarketingHub = true
				break
			}
		}
		tier := 2
		switch {
		case isMarketingHub || resolved.RawQuery != "":
			tier = 3
		case sameSection && inBodyProse:
			tier = 0
		case sameSection || inBodyProse:
			tier = 1
		}

		key := canonicalLinkPathKey(resolved)
		clean := resolved.String()
		if existing, exists := byKey[key]; !exists || tier < existing.tier {
			order++
			ord := order
			if exists {
				ord = existing.order
			}
			byKey[key] = linkCandidate{url: clean, tier: tier, order: ord}
		}
	})

	ranked := make([]linkCandidate, 0, len(byKey))
	for _, cand := range byKey {
		ranked = append(ranked, cand)
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].tier != ranked[j].tier {
			return ranked[i].tier < ranked[j].tier
		}
		return ranked[i].order < ranked[j].order
	})
	ordered := make([]string, len(ranked))
	for i, cand := range ranked {
		ordered[i] = cand.url
	}
	return packStringsByByteBudget(ordered, budgetBytes)
}

func countUnrenderedTemplateTokens(doc *goquery.Document) int {
	countIn := func(s string) int {
		hits := 0
		for i := 0; i < len(s); i++ {
			if s[i] == '{' {
				if i+1 < len(s) && s[i+1] == '{' {
					if end := strings.Index(s[i+2:], "}}"); end > 0 && end < 80 {
						inner := strings.TrimSpace(s[i+2 : i+2+end])
						if inner != "" && !strings.ContainsAny(inner, "<>\n") {
							hits++
						}
						i += 2 + end + 1
						continue
					}
				} else if end := strings.IndexByte(s[i+1:], '}'); end > 4 && end < 120 {
					inner := s[i+1 : i+1+end]
					if bang := strings.IndexByte(inner, '!'); bang > 0 && !strings.ContainsAny(inner[:bang], " \t\n<>\"'") {
						hits++
					}
					i += end + 1
				}
			}
		}
		return hits
	}
	total := 0
	doc.Find("body table, body [data-token-prefix], body [data-template]").EachWithBreak(func(_ int, s *goquery.Selection) bool {
		for _, attr := range s.Nodes[0].Attr {
			if strings.Contains(strings.ToLower(attr.Key), "markup") || strings.Contains(strings.ToLower(attr.Key), "template") {
				total += countIn(attr.Val)
			}
		}
		total += countIn(s.Text())
		return total < 20
	})
	return total
}

var chromeNoiseTokens = []string{
	"footer", "mobile-menu", "submenu", "megamenu", "mega-menu", "utility-links",
	"navbar", "nav-menu", "nav-drawer", "top-nav", "topnav", "subnav",
	"breadcrumb", "cookie-banner", "locale-switcher", "language-selector",
}

func hasChromeClassOrID(s *goquery.Selection) bool {
	if s.Is("html,body,main,article,[role=main]") {
		return false
	}
	id, _ := s.Attr("id")
	cls, _ := s.Attr("class")
	combined := strings.ToLower(id + " " + cls)
	if strings.TrimSpace(combined) == "" {
		return false
	}
	for _, tok := range chromeNoiseTokens {
		if strings.Contains(combined, tok) {
			return true
		}
	}
	return false
}

func inChromeNoiseRegion(s *goquery.Selection) bool {
	if s.Is("nav,header,footer,[role=navigation],[role=banner],[role=contentinfo]") ||
		s.ParentsFiltered("nav,header,footer,[role=navigation],[role=banner],[role=contentinfo]").Length() > 0 {
		return true
	}
	cur := s
	for depth := 0; depth < 10 && cur != nil && cur.Length() > 0; depth++ {
		if hasChromeClassOrID(cur) {
			return true
		}
		cur = cur.Parent()
	}
	return false
}

func collectOutlineHeadings(doc *goquery.Document, limit int) []map[string]any {
	var headings []map[string]any
	seen := map[string]bool{}
	doc.Find("main h1, main h2, main h3, main h4, article h1, article h2, article h3, article h4, h1, h2, h3, h4").EachWithBreak(func(_ int, s *goquery.Selection) bool {
		if inChromeNoiseRegion(s) && s.Is("h2, h3, h4") {
			return true
		}
		shape, length := classifyOutlineShape(s.Text())
		if shape == "" {
			return true
		}
		sel := conciseElementSelector(s)
		if sel == "" || seen[sel] {
			return true
		}
		seen[sel] = true
		headings = append(headings, map[string]any{
			"selector":   sel,
			"count":      doc.Find(sel).Length(),
			"shape":      shape,
			"text_chars": length,
		})
		return len(headings) < limit
	})
	return headings
}

func safeCSSIdent(token string) bool {
	if token == "" {
		return false
	}
	for i := 0; i < len(token); i++ {
		c := token[i]
		isAlpha := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' || c == '-'
		isDigit := c >= '0' && c <= '9'
		if i == 0 && !isAlpha {
			return false
		}
		if !isAlpha && !isDigit {
			return false
		}
	}
	return true
}

func primaryClassSuffix(s *goquery.Selection) string {
	raw, ok := s.Attr("class")
	if !ok {
		return ""
	}
	var out []string
	for _, tok := range strings.Fields(raw) {
		if safeCSSIdent(tok) && !strings.HasPrefix(tok, "js-") {
			out = append(out, "."+tok)
			if len(out) == 2 {
				break
			}
		}
	}
	return strings.Join(out, "")
}

func conciseElementSelector(s *goquery.Selection) string {
	if s.Length() == 0 || s.Nodes[0] == nil {
		return ""
	}
	tag := s.Nodes[0].Data
	if id, ok := s.Attr("id"); ok && safeCSSIdent(id) {
		return tag + "#" + id
	}
	return tag + primaryClassSuffix(s)
}

func validOutlineSelector(doc *goquery.Document, sel string) (*goquery.Selection, bool) {
	if strings.TrimSpace(sel) == "" {
		return nil, false
	}
	if _, err := cascadia.Compile(sel); err != nil {
		return nil, false
	}
	matches := doc.Find(sel)
	return matches, matches.Length() > 0
}

type outlineBlockCandidate struct {
	score int
	sel   string
	data  map[string]any
}

func collectOutlineBlocks(doc *goquery.Document, limit int) []map[string]any {
	candidates := []outlineBlockCandidate{}
	seenSel := map[string]bool{}
	seenFirstNode := map[*html.Node]bool{}

	// 1. Tables: real row selector + column structure.
	singleRowTables := 0
	doc.Find("table").Each(func(idx int, tbl *goquery.Selection) {
		if inChromeNoiseRegion(tbl) {
			return
		}
		base := conciseElementSelector(tbl)
		rowSel := base + " tbody tr"
		rows, ok := validOutlineSelector(doc, rowSel)
		if !ok || rows.Length() == 0 {
			rowSel = base + " tr"
			rows, ok = validOutlineSelector(doc, rowSel)
		}
		if !ok || rows.Length() == 0 {
			return
		}
		if seenSel[rowSel] {
			if rows.Length() == 1 {
				singleRowTables++
				if singleRowTables > 1 {
					return
				}
			}
			rowSel = fmt.Sprintf("table:nth-of-type(%d) tr", idx+1)
			rows, ok = validOutlineSelector(doc, rowSel)
			if !ok || seenSel[rowSel] {
				return
			}
		}
		thCount := tbl.Find("thead th, tr:first-child th").Length()
		sampleRow := rows.First()
		if sampleRow.Find("td").Length() == 0 && rows.Length() > 1 {
			sampleRow = rows.Eq(1)
		}
		fields := inspectContainerFields(sampleRow)
		if len(fields) == 0 && thCount == 0 {
			return
		}
		seenSel[rowSel] = true
		block := map[string]any{
			"kind":     "table",
			"selector": rowSel,
			"count":    rows.Length(),
			"fields":   fields,
		}
		if thCount > 0 {
			block["header_columns"] = thCount
			var headers []string
			tbl.Find("thead th, tr:first-child th").EachWithBreak(func(_ int, th *goquery.Selection) bool {
				txt := strings.Join(strings.Fields(th.Text()), " ")
				if txt != "" {
					headers = append(headers, truncateRunes(txt, 24))
				}
				return len(headers) < 8
			})
			if len(headers) > 0 {
				block["header_names"] = headers
			}
		}
		score := 80 + min(rows.Length(), 30) + len(fields)*5
		if tbl.ParentsFiltered("main,article,[role=main]").Length() > 0 {
			score += 30
		}
		candidates = append(candidates, outlineBlockCandidate{score: score, sel: rowSel, data: block})
	})

	// 2. Repeated sibling blocks (lists, cards, articles, release groups).
	roots := doc.Find("main, article, [role=main]")
	if roots.Length() == 0 {
		roots = doc.Find("body")
	}
	skipChildTags := map[string]bool{"script": true, "style": true, "br": true, "hr": true, "meta": true, "link": true, "option": true}
	roots.Find("*").AddBack().Each(func(_ int, parent *goquery.Selection) {
		if inChromeNoiseRegion(parent) || parent.Is("table,thead,tbody,tr,select") {
			return
		}
		children := parent.Children()
		if children.Length() < 2 {
			return
		}
		groups := map[string][]*goquery.Selection{}
		order := []string{}
		children.Each(func(_ int, child *goquery.Selection) {
			if len(child.Nodes) == 0 {
				return
			}
			tag := child.Nodes[0].Data
			if skipChildTags[tag] || child.Is("nav,header,footer") {
				return
			}
			key := tag + primaryClassSuffix(child)
			if _, exists := groups[key]; !exists {
				order = append(order, key)
			}
			groups[key] = append(groups[key], child)
		})
		parentSel := conciseElementSelector(parent)
		for _, childKey := range order {
			group := groups[childKey]
			if len(group) < 2 {
				continue
			}
			if len(group[0].Nodes) > 0 && seenFirstNode[group[0].Nodes[0]] {
				continue
			}
			sel := parentSel + " > " + childKey
			matches, ok := validOutlineSelector(doc, sel)
			if !ok || matches.Length() < 2 {
				continue
			}
			if strings.Contains(childKey, ".") {
				if alt, altOK := validOutlineSelector(doc, childKey); altOK && alt.Length() == matches.Length() {
					sel = childKey
				}
			}
			if seenSel[sel] {
				continue
			}
			if shape, _ := classifyOutlineShape(matches.First().Text()); shape == "" {
				continue
			}
			fields := inspectContainerFields(matches.First())
			if len(fields) == 0 {
				continue
			}
			seenSel[sel] = true
			if len(group[0].Nodes) > 0 {
				seenFirstNode[group[0].Nodes[0]] = true
			}
			score := 40 + min(matches.Length(), 30) + len(fields)*6
			if parent.ParentsFiltered("main,article,[role=main]").Length() > 0 || parent.Is("main,article,[role=main]") {
				score += 25
			}
			if matches.First().Find("a[href],time,h1,h2,h3,h4").Length() > 0 {
				score += 15
			}
			candidates = append(candidates, outlineBlockCandidate{
				score: score,
				sel:   sel,
				data: map[string]any{
					"kind":     "list",
					"selector": sel,
					"count":    matches.Length(),
					"fields":   fields,
				},
			})
		}
	})

	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].score > candidates[j].score
	})

	// 3. Fallback structural sections when few repeated/table structures exist.
	if len(candidates) < 2 {
		for _, fallbackSel := range []string{"main", "article", "[role=main]", "section", "div.content", "#content"} {
			matches, ok := validOutlineSelector(doc, fallbackSel)
			if !ok || seenSel[fallbackSel] {
				continue
			}
			first := matches.First()
			if inChromeNoiseRegion(first) {
				continue
			}
			fields := inspectContainerFields(first)
			if len(fields) == 0 {
				continue
			}
			seenSel[fallbackSel] = true
			candidates = append(candidates, outlineBlockCandidate{
				score: 10 + len(fields)*4,
				sel:   fallbackSel,
				data: map[string]any{
					"kind":     "section",
					"selector": fallbackSel,
					"count":    matches.Length(),
					"fields":   fields,
				},
			})
			if len(candidates) >= limit {
				break
			}
		}
	}

	out := make([]map[string]any, 0, min(limit, len(candidates)))
	for i := 0; i < len(candidates) && i < limit; i++ {
		out = append(out, candidates[i].data)
	}
	return out
}

func inspectContainerFields(node *goquery.Selection) []map[string]any {
	fields := []map[string]any{}
	seenSel := map[string]bool{}
	addField := func(selKey, shape string, textChars int, attrs ...string) {
		if len(fields) >= 8 || selKey == "" || seenSel[selKey] {
			return
		}
		seenSel[selKey] = true
		entry := map[string]any{
			"selector": selKey,
			"shape":    shape,
		}
		if textChars > 0 {
			entry["text_chars"] = textChars
		}
		if len(attrs) > 0 {
			entry["attrs"] = attrs
		}
		fields = append(fields, entry)
	}

	if tds := node.Find("td"); tds.Length() > 0 {
		tds.EachWithBreak(func(i int, td *goquery.Selection) bool {
			sel := "td:nth-child(" + strconv.Itoa(i+1) + ")"
			if shape, n := classifyOutlineShape(td.Text()); shape != "" {
				var attrs []string
				if a := td.Find("a[href]").First(); a.Length() > 0 {
					attrs = append(attrs, "a[href]")
				}
				addField(sel, shape, n, attrs...)
			} else if a := td.Find("a[href]").First(); a.Length() > 0 {
				addField(sel+" a", "url", 0, "href")
			}
			return len(fields) < 8
		})
		return fields
	}

	for _, sel := range []string{"h1", "h2", "h3", "h4", "time", "a", "p", "span", "strong", "code"} {
		el := node.Find(sel).First()
		if el.Length() == 0 {
			continue
		}
		exactSel := conciseElementSelector(el)
		if exactSel == "" {
			exactSel = sel
		}
		shape, n := classifyOutlineShape(el.Text())
		var attrs []string
		if sel == "a" {
			if href, ok := el.Attr("href"); ok && strings.TrimSpace(href) != "" {
				attrs = append(attrs, "href")
			}
		} else if sel == "time" {
			if dt, ok := el.Attr("datetime"); ok && strings.TrimSpace(dt) != "" {
				attrs = append(attrs, "datetime")
				shape = "date"
			}
		}
		if shape != "" || len(attrs) > 0 {
			if shape == "" {
				shape = "url"
			}
			addField(exactSel, shape, n, attrs...)
		}
	}
	if len(fields) == 0 {
		if shape, n := classifyOutlineShape(node.Text()); shape != "" {
			var attrs []string
			if href, ok := node.Attr("href"); ok && strings.TrimSpace(href) != "" {
				attrs = append(attrs, "href")
			}
			addField(".", shape, n, attrs...)
		}
	}
	return fields
}

type jsonPathShape struct {
	Path       string          `json:"pointer"`
	Type       string          `json:"type"`
	KeyCount   int             `json:"key_count,omitempty"`
	SampleKeys []string        `json:"sample_keys,omitempty"`
	ItemPaths  []jsonPathShape `json:"sample_item_paths,omitempty"`
}

func observedJSONPaths(value any, path string) []jsonPathShape {
	budget := 60
	return observedJSONPathsBounded(value, path, 0, &budget)
}

func observedJSONPathsBounded(value any, path string, depth int, budget *int) []jsonPathShape {
	if *budget <= 0 {
		return nil
	}
	switch node := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(node))
		for key := range node {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		entry := jsonPathShape{Path: path, Type: "object"}
		*budget--
		if depth >= 5 || len(keys) == 0 {
			if len(keys) > 0 {
				entry.KeyCount = len(keys)
			}
			return []jsonPathShape{entry}
		}
		if len(keys) > 12 {
			entry.KeyCount = len(keys)
			limit := min(6, len(keys))
			entry.SampleKeys = append([]string(nil), keys[:limit]...)
			sampleVal := node[keys[0]]
			for _, k := range keys {
				if m, ok := node[k].(map[string]any); ok && len(m) > 0 {
					sampleVal = m
					break
				}
			}
			if *budget > 1 {
				subBudget := min(*budget, 15)
				*budget -= subBudget
				entry.ItemPaths = observedJSONPathsBounded(sampleVal, "", depth+1, &subBudget)
			}
			return []jsonPathShape{entry}
		}
		paths := []jsonPathShape{entry}
		for _, key := range keys {
			if *budget <= 0 {
				break
			}
			token := strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
			paths = append(paths, observedJSONPathsBounded(node[key], path+"/"+token, depth+1, budget)...)
		}
		return paths
	case []any:
		shape := jsonPathShape{Path: path, Type: "array"}
		*budget--
		if len(node) > 0 && depth < 5 && *budget > 0 {
			item := node[0]
			for _, candidate := range node {
				if _, ok := candidate.(map[string]any); ok {
					item = candidate
					break
				}
			}
			shape.ItemPaths = observedJSONPathsBounded(item, "", depth+1, budget)
		}
		return []jsonPathShape{shape}
	case string:
		*budget--
		return []jsonPathShape{{Path: path, Type: "string"}}
	case float64:
		*budget--
		return []jsonPathShape{{Path: path, Type: "number"}}
	case bool:
		*budget--
		return []jsonPathShape{{Path: path, Type: "boolean"}}
	default:
		*budget--
		return []jsonPathShape{{Path: path, Type: "null"}}
	}
}

// Saved internal policy is historical evidence, not authority to revive old caps.
func informationPromptPolicy(_ Request) (*aiapp.PromptPolicy, error) {
	return aiapp.ConfiguredPromptPolicy()
}

func (h *informationHost) attachAcquisitionExcerpts(ctx context.Context, raw string, data any) (any, error) {
	var args struct {
		Queries []string `json:"excerpt_queries"`
	}
	if err := json.Unmarshal([]byte(raw), &args); err != nil || len(collectExcerptQueries("", args.Queries)) == 0 {
		return data, nil
	}
	var out map[string]any
	switch saved := data.(type) {
	case map[string]any:
		out = saved
	case json.RawMessage:
		decoder := json.NewDecoder(strings.NewReader(string(saved)))
		decoder.UseNumber()
		if err := decoder.Decode(&out); err != nil {
			return data, err
		}
	default:
		return data, nil
	}
	var refs []StoredReference
	if err := json.Unmarshal(mustMarshal(out["references"]), &refs); err != nil {
		return data, err
	}
	if len(refs) == 0 {
		return data, nil
	}
	q := MaterialReadQuery{Kind: "excerpts", Queries: args.Queries, exactReferences: refs}
	for _, ref := range refs {
		q.DocumentIDs = append(q.DocumentIDs, ref.DocID)
	}
	excerpts, err := h.service.QueryTaskDocumentExcerpts(ctx, h.request.ID, h.input.SourceScope, q)
	if err != nil {
		return data, err
	}
	out["evidence"] = excerpts
	return out, nil
}

func (h *informationHost) observeBatch(ctx context.Context, rawURLs []string, single bool) (map[string]any, error) {
	urls := make([]string, 0, len(rawURLs))
	seen := make(map[string]bool)
	for _, raw := range rawURLs {
		u := strings.TrimSpace(raw)
		if u == "" {
			continue
		}
		if strings.HasPrefix(u, "//") {
			u = "https:" + u
		}
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			u = "https://" + u
		}
		if !urlInScope(u, h.input.SourceScope) {
			return nil, actionFailure("ASSET_SCOPE_INVALID", fmt.Errorf("proposed URL is outside source scope or malformed"), "Use observed public HTTP(S) URLs within source_scope.")
		}
		parsed, err := url.Parse(u)
		if err != nil || parsed.User != nil || parsed.Hostname() == "" {
			return nil, actionFailure("ASSET_SCOPE_INVALID", fmt.Errorf("proposed URL is malformed"), "Use an absolute public HTTP(S) URL without credentials.")
		}
		parsed.Host = strings.ToLower(parsed.Host)
		parsed.Fragment = ""
		if parsed.Path == "" {
			parsed.Path = "/"
		}
		if target := parsed.String(); !seen[target] {
			seen[target] = true
			urls = append(urls, target)
		}
	}
	if len(urls) == 0 {
		return nil, actionFailure("SOURCE_URLS_REQUIRED", fmt.Errorf("inspect requires at least one public URL"), "Provide inspect_url or inspect_urls within source_scope.")
	}
	if single {
		return h.inspectSource(ctx, urls[0])
	}

	results := make([]map[string]any, len(urls))
	failures := make([]error, len(urls))
	var wg sync.WaitGroup
	for i, target := range urls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], failures[i] = h.inspectSource(ctx, target)
		}()
	}
	wg.Wait()

	var refs []StoredReference
	var browserApproved []string
	var lastErr error
	successes := 0
	for i, entry := range results {
		if err := failures[i]; err != nil {
			lastErr = err
			if entry == nil {
				entry = map[string]any{"url": urls[i], "error": err.Error()}
				results[i] = entry
			}
		} else {
			successes++
		}
		if ref := inspectionReference(entry); ref != nil {
			refs = append(refs, *ref)
			entry["source_ref"] = *ref
		}
		delete(entry, "references")
		if entry["browser_approved"] == true {
			if target, ok := entry["url"].(string); ok {
				browserApproved = append(browserApproved, target)
			}
		}
	}
	out := map[string]any{"status": "inspected", "sources": results, "references": refs}
	if len(browserApproved) > 0 {
		out["browser_approved_urls"] = browserApproved
	}
	if lastErr != nil {
		out["status"] = "partial"
		if successes == 0 {
			out["status"] = "failed"
			failure := actionFailure("ACQUISITION_FETCH_FAILED", lastErr, "Read each source's status and recovery advice. Use other available sources while a blocked or rate-limited source recovers.")
			failure.Details = map[string]any{"sources": results, "browser_approved_urls": browserApproved}
			return out, failure
		}
	}
	return out, nil
}

func inspectionReference(entry map[string]any) *StoredReference {
	raw, ok := entry["references"]
	if !ok {
		return nil
	}
	var refs []StoredReference
	if json.Unmarshal(mustMarshal(raw), &refs) != nil || len(refs) == 0 {
		return nil
	}
	return &refs[0]
}

func (h *informationHost) inspectSource(ctx context.Context, target string) (map[string]any, error) {
	// One durable fetch receipt belongs to each normalized URL. The outer
	// tool receipt still owns the single/batch presentation.
	ctx = context.WithValue(ctx, acquisitionReceiptKey{}, nil)
	r := &h.request
	stepArgs := map[string]any{"url": target, "scope": h.input.SourceScope}
	step, fresh, err := h.service.reserveStep(ctx, *r, "inspect_api_candidate", stepArgs)
	if err != nil {
		return nil, err
	}
	if !fresh {
		if step.Stage == "reserved" {
			return nil, ErrUnknownStep
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(step.ResultJSON), &out); err != nil {
			return nil, err
		}
		return out, restoredDomainFailure(step)
	}
	fetcher, err := h.service.NewFetcher(h.input.SourceScope)
	if err != nil {
		return nil, actionFailure("ACQUISITION_FETCHER_UNAVAILABLE", err, "Check the source transport configuration or use another available source.")
	}
	defer fetcher.Close()

	opts := acquisition.RequestOptions{Method: "GET", URL: target}
	snapshots, err := h.snapshotsForRequests(ctx, fetcher, []acquisition.RequestOptions{opts})
	if err != nil {
		return nil, err
	}
	resp, fErr := fetcher.FetchWithSnapshots(ctx, opts, snapshots.responses)
	if fErr == nil && (resp.Receipt.Status < 200 || resp.Receipt.Status >= 300) {
		fErr = fmt.Errorf("source HTTP status %d", resp.Receipt.Status)
	}
	if reason, blocked := browserSourceDiagnosis(resp); fErr == nil && blocked {
		fErr = fmt.Errorf("%s", reason)
	}
	out := acquisitionResponseMetadata(resp)
	if out["url"] == "" {
		out["url"] = target
	}
	var cause error
	if fErr != nil {
		out["error"] = fErr.Error()
		out["headers"] = resp.Receipt.DiagnosticHeaders()
		addRateLimitAdvice(out, resp.Receipt)
		failure := actionFailure("ACQUISITION_FETCH_FAILED", fErr, "Inspect the status and headers to repair the source request or continue with another source.")
		failure.Details = map[string]any{"http": resp.Receipt}
		if advice, ok := out["next_step"].(string); ok {
			failure.Steps = []string{advice}
		}
		if approvedURL, ok := h.service.maybeApproveBrowserFromFetch(ctx, r, step, h.input.SourceScope, resp); ok {
			approved := []string{approvedURL}
			out["browser_approved"] = true
			out["browser_approved_urls"] = approved
			failure.Details["browser_approved_urls"] = approved
			failure.Steps = []string{"Use browser_extract with an approved URL in browser_approved_urls or continue with another source."}
		}
		cause = failure
	} else if out["is_json"] == true {
		out["next_step"] = "Use observed json_paths for acquire_sources(api=...) or extract with run_lua(source_ref=...) without url/targets."
	} else if out["is_html"] == true || out["is_xml"] == true {
		out["next_step"] = snapshotLinkAdvice
	}
	var captures []acquisition.Response
	if resp.Receipt.URL != "" && !resp.Receipt.FetchedAt.IsZero() {
		captures = append(captures, resp)
	}
	saved := map[int]knowledge.Document{}
	if snapshots.bind(resp, saved, 0) {
		out["reused_snapshot"] = true
	}
	if err := h.commitAcquisition(ctx, step, nil, captures, out, cause, saved); err != nil {
		return nil, err
	}
	return out, cause
}
