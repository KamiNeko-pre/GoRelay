package probe

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"gorelay/internal/contract"
)

// SummarizeJSON inspects JSON structure and infers record array path, scalar fields, and pagination hints.
func SummarizeJSON(raw []byte) (*JSONSummary, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var val any
	if err := dec.Decode(&val); err != nil {
		return nil, err
	}
	sum := &JSONSummary{}
	switch root := val.(type) {
	case []any:
		sum.RootType = "array"
		sum.ItemsPath = ""
		sum.ItemCount = len(root)
		if first := firstObjectRow(root); first != nil {
			sum.Fields, sum.SampleRow = summarizeObject(first)
		}
		return sum, nil
	case map[string]any:
		sum.RootType = "object"
		sum.PaginationHint = detectPaginationHint(root)
		path, arr := findBestObjectArray(root, "")
		if arr != nil {
			sum.ItemsPath = path
			sum.ItemCount = len(arr)
			if first := firstObjectRow(arr); first != nil {
				sum.Fields, sum.SampleRow = summarizeObject(first)
			}
			return sum, nil
		}
		sum.Fields, sum.SampleRow = summarizeObject(root)
		return sum, nil
	default:
		sum.RootType = fmt.Sprintf("%T", val)
		return sum, nil
	}
}

func findBestObjectArray(node map[string]any, prefix string) (string, []any) {
	keys := make([]string, 0, len(node))
	for k := range node {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var bestPath string
	var bestArr []any
	for _, k := range keys {
		p := prefix + "/" + escapePointer(k)
		switch v := node[k].(type) {
		case []any:
			if len(v) > len(bestArr) && firstObjectRow(v) != nil {
				bestPath, bestArr = p, v
			}
		case map[string]any:
			if subPath, subArr := findBestObjectArray(v, p); len(subArr) > len(bestArr) {
				bestPath, bestArr = subPath, subArr
			}
		}
	}
	return bestPath, bestArr
}

func firstObjectRow(arr []any) map[string]any {
	for _, item := range arr {
		if m, ok := item.(map[string]any); ok {
			return m
		}
	}
	return nil
}

func summarizeObject(obj map[string]any) (map[string]string, map[string]any) {
	fields := make(map[string]string)
	sample := make(map[string]any)
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	limit := 20
	if len(keys) < limit {
		limit = len(keys)
	}
	for _, k := range keys[:limit] {
		v := obj[k]
		fields[k] = inferScalarKind(v)
		switch s := v.(type) {
		case string:
			if len(s) > 160 {
				sample[k] = s[:160] + "..."
			} else {
				sample[k] = s
			}
		case bool, json.Number, float64, int, int64:
			sample[k] = v
		}
	}
	return fields, sample
}

func inferScalarKind(v any) string {
	switch {
	case contract.FieldMatches(v, "integer"):
		return "integer"
	case contract.FieldMatches(v, "number"):
		return "number"
	case contract.FieldMatches(v, "boolean"):
		return "boolean"
	case contract.FieldMatches(v, "string_array"):
		return "string_array"
	case v == nil:
		return "null"
	default:
		if _, ok := v.(map[string]any); ok {
			return "object"
		}
		if _, ok := v.([]any); ok {
			return "array"
		}
		return "string"
	}
}

func detectPaginationHint(root map[string]any) string {
	hints := []string{}
	candidates := []string{"next_cursor", "nextCursor", "cursor", "next_page", "nextPage", "page", "total_pages", "has_more", "next"}
	for _, key := range candidates {
		if _, ok := root[key]; ok {
			hints = append(hints, key)
		}
	}
	if len(hints) == 0 {
		if meta, ok := root["meta"].(map[string]any); ok {
			for _, key := range candidates {
				if _, exists := meta[key]; exists {
					hints = append(hints, "meta."+key)
				}
			}
		}
		if pag, ok := root["pagination"].(map[string]any); ok {
			for _, key := range candidates {
				if _, exists := pag[key]; exists {
					hints = append(hints, "pagination."+key)
				}
			}
		}
	}
	return strings.Join(hints, ", ")
}

func escapePointer(s string) string {
	s = strings.ReplaceAll(s, "~", "~0")
	return strings.ReplaceAll(s, "/", "~1")
}

// ExtractEmbeddedJSON finds inline JSON payloads embedded inside static HTML (<script type="application/json">, __NEXT_DATA__, JSON-LD).
func ExtractEmbeddedJSON(rawHTML string, pageURL string) []CandidateAPI {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(rawHTML))
	if err != nil {
		return nil
	}
	var out []CandidateAPI
	doc.Find(`script#__NEXT_DATA__, script[type="application/json"], script[type="application/ld+json"]`).Each(func(i int, s *goquery.Selection) {
		if len(out) >= 4 {
			return
		}
		text := strings.TrimSpace(s.Text())
		if len(text) < 2 || len(text) > 1<<20 {
			return
		}
		sum, err := SummarizeJSON([]byte(text))
		if err != nil || (len(sum.Fields) == 0 && sum.ItemCount == 0) {
			return
		}
		id, _ := s.Attr("id")
		source := "embedded_json"
		if id != "" {
			source = "embedded_json:#" + id
		}
		out = append(out, CandidateAPI{
			Source:         source,
			Method:         "GET",
			URL:            pageURL,
			ContentType:    "application/json",
			ItemsPath:      sum.ItemsPath,
			PaginationHint: sum.PaginationHint,
			SampleFields:   sum.Fields,
			SampleRow:      sum.SampleRow,
		})
	})
	return out
}

// ExtractDOMSummary returns compact repeated selector candidates and checks whether the page is an empty SPA shell.
func ExtractDOMSummary(rawHTML string) ([]DOMSelectorSample, bool) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(rawHTML))
	if err != nil {
		return nil, false
	}
	doc.Find("script,style,noscript,template,svg").Remove()
	visibleText := strings.Join(strings.Fields(doc.Text()), " ")

	candidates := []string{"article", "main li", "table tbody tr", "ul.list > li", "div.card", "div.item", "[data-testid]", "h2 a", "h3 a", "a[href]"}
	var samples []DOMSelectorSample
	for _, sel := range candidates {
		selNodes := doc.Find(sel)
		cnt := selNodes.Length()
		if cnt < 2 {
			continue
		}
		var texts []string
		var links []string
		selNodes.Slice(0, minInt(3, cnt)).Each(func(_ int, node *goquery.Selection) {
			txt := strings.Join(strings.Fields(node.Text()), " ")
			if len(txt) > 120 {
				txt = txt[:120] + "..."
			}
			if txt != "" {
				texts = append(texts, txt)
			}
			if href, ok := node.Attr("href"); ok && href != "" {
				links = append(links, href)
			} else if href, ok := node.Find("a[href]").First().Attr("href"); ok && href != "" {
				links = append(links, href)
			}
		})
		samples = append(samples, DOMSelectorSample{
			Selector:    sel,
			Count:       cnt,
			SampleTexts: texts,
			SampleLinks: links,
		})
		if len(samples) >= 8 {
			break
		}
	}

	rawLower := strings.ToLower(rawHTML)
	spaShell := len(visibleText) < 200 && (strings.Contains(rawLower, `id="root"`) ||
		strings.Contains(rawLower, `id="app"`) ||
		strings.Contains(rawLower, `id="__next"`) ||
		strings.Contains(rawLower, "enable javascript"))
	return samples, spaShell
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
