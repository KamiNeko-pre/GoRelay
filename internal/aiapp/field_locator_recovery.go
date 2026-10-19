package aiapp

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/engine"
)

// Diagnosis must add verifiable information rather than merely repeat a rule.
// Never infer a field's meaning, manufacture a value or relax its failed check.
func verifiedLocatorSuggestion(result engine.ProgramResult, sourceURL string, value any) *fieldLocator {
	if value == nil {
		return nil
	}
	if text, ok := value.(string); ok && (strings.TrimSpace(text) == "" || len([]rune(text)) > 180) {
		return nil
	}
	for _, source := range result.SourceEvidence {
		if (source.Receipt.URL != sourceURL && source.Receipt.RequestedURL != sourceURL) || source.Receipt.Method != "GET" || source.Receipt.Status < 200 || source.Receipt.Status >= 300 {
			continue
		}
		if loc := uniqueSourceLocator(source, value); loc != nil {
			actual, _, err := evidenceValue(source, *loc)
			if err == nil && matchingFieldValue(actual, value) {
				return loc
			}
		}
	}
	// If the primary source cannot prove the value, a unique exact match in a
	// different host-captured response is an actionable qualified locator.
	var found *fieldLocator
	for _, source := range result.SourceEvidence {
		if !observedCrawlerSource(source) || (source.Receipt.URL == sourceURL || source.Receipt.RequestedURL == sourceURL) {
			continue
		}
		loc := uniqueSourceLocator(source, value)
		if loc == nil {
			continue
		}
		actual, _, err := evidenceValue(source, *loc)
		if err != nil || !matchingFieldValue(actual, value) {
			continue
		}
		if found != nil {
			return nil
		}
		loc.SourceURL = source.Receipt.URL
		found = loc
	}
	return found
}

func matchingFieldValue(left, right any) bool {
	if text, ok := left.(string); ok {
		left = normalizeEvidenceText(text)
	}
	if text, ok := right.(string); ok {
		right = normalizeEvidenceText(text)
	}
	return canonicalDeliveryValue(left) == canonicalDeliveryValue(right)
}

func uniqueSourceLocator(source acquisition.Response, value any) *fieldLocator {
	var root any
	decoder := json.NewDecoder(strings.NewReader(source.Body))
	decoder.UseNumber()
	if decoder.Decode(&root) == nil {
		var found *fieldLocator
		count := 0
		var walk func(any, string)
		walk = func(node any, path string) {
			if count > 1 {
				return
			}
			switch v := node.(type) {
			case map[string]any:
				keys := make([]string, 0, len(v))
				for key := range v {
					keys = append(keys, key)
				}
				sort.Strings(keys)
				for _, key := range keys {
					p := path + "/" + pointerKey(key)
					if matchingFieldValue(key, value) {
						found = &fieldLocator{JSONKeyPointer: &p}
						count++
					}
					walk(v[key], p)
				}
			case []any:
				for i, child := range v {
					raw, _ := json.Marshal(i)
					walk(child, path+"/"+string(raw))
				}
			default:
				if matchingFieldValue(v, value) {
					p := path
					found = &fieldLocator{JSONPointer: &p}
					count++
				}
			}
		}
		walk(root, "")
		if count == 1 {
			return found
		}
		return nil
	}
	text, ok := value.(string)
	if !ok {
		return nil
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(source.Body))
	if err != nil {
		return nil
	}
	doc.Find("script,style,noscript,template").Remove()
	var found *fieldLocator
	count := 0
	doc.Find("*").EachWithBreak(func(_ int, node *goquery.Selection) bool {
		if !matchingFieldValue(node.Text(), text) {
			return true
		}
		// Wrapping elements with the same text are one value, not ambiguous
		// alternative fields. Prefer the smallest element that holds it.
		wrapped := false
		node.Children().EachWithBreak(func(_ int, child *goquery.Selection) bool {
			wrapped = matchingFieldValue(child.Text(), text)
			return !wrapped
		})
		if wrapped {
			return true
		}
		selector := shortDOMSelector(doc, node.Nodes[0])
		if selector == "" || len(selector) > 512 {
			return true
		}
		found = &fieldLocator{Selector: selector}
		count++
		return count < 2
	})
	if count == 1 {
		return found
	}
	return nil
}
