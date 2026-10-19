package aiapp

import (
	"fmt"
	"github.com/PuerkitoBio/goquery"
	"golang.org/x/net/html"
	"strconv"
	"strings"
)

// Structural samples help the model choose its own Lua selectors. They are
// inspection metadata, never delivered records or RAG evidence.
type DOMBlock struct {
	Selector            string `json:"selector"`
	Paragraphs          int    `json:"paragraphs"`
	Sample              string `json:"sample"`
	Matches             int    `json:"matches,omitempty"`
	HTMLSample          string `json:"html_sample,omitempty"`
	HTMLSampleTruncated bool   `json:"html_sample_truncated,omitempty"`
}

func structuralDOMSample(selection *goquery.Selection) (string, bool) {
	clone := selection.Clone()
	clone.Find("script, style, svg, canvas, iframe, nav, [role=navigation], [hidden], [aria-hidden=true]").Remove()
	clone.Find("*").AddSelection(clone).Each(func(_ int, node *goquery.Selection) {
		attrs := node.Nodes[0].Attr[:0]
		for _, attr := range node.Nodes[0].Attr {
			if attr.Key == "style" || attr.Key == "srcdoc" || strings.HasPrefix(attr.Key, "on") {
				continue
			}
			attrs = append(attrs, attr)
		}
		node.Nodes[0].Attr = attrs
	})
	raw, _ := goquery.OuterHtml(clone)
	sample := []rune(raw)
	return string(sample[:min(800, len(sample))]), len(sample) > 800
}

func inspectDOM(body string) []DOMBlock {
	return inspectDOMWithLimits(body, 6, 12)
}

// Preview limits are presentation policy, not the source's complete inventory.
// A zero limit is used only by the paged, receipt-owned structure reader.
func inspectDOMWithLimits(body string, paragraphLimit, blockLimit int) []DOMBlock {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(body))
	if err != nil {
		return nil
	}
	out := []DOMBlock{}
	seen := map[string]bool{}
	root := doc.Find("main, article, [role=main]")
	if root.Length() == 0 {
		root = doc.Find("body")
	}
	root.Find("p").Each(func(_ int, p *goquery.Selection) {
		if p.Closest("nav, header, footer, [role=navigation]").Length() > 0 {
			return
		}
		text := visibleHTMLText(p.Nodes[0])
		if len([]rune(text)) < 30 || (paragraphLimit > 0 && len(out) >= paragraphLimit) {
			return
		}
		parent := p.Parent()
		selector := shortDOMSelector(doc, parent.Nodes[0])
		if seen[selector] {
			return
		}
		seen[selector] = true
		sample := []rune(text)
		if len(sample) > 180 {
			sample = sample[:180]
		}
		out = append(out, DOMBlock{Selector: selector, Paragraphs: parent.Find("p").Length(), Sample: string(sample)})
	})
	root.Find("pre, table, dl").Each(func(_ int, node *goquery.Selection) {
		if node.Closest("nav, header, footer, [role=navigation]").Length() > 0 {
			return
		}
		selector := shortDOMSelector(doc, node.Nodes[0])
		if seen[selector] || (blockLimit > 0 && len(out) >= blockLimit) {
			return
		}
		text := []rune(visibleHTMLText(node.Nodes[0]))
		if len(text) == 0 {
			return
		}
		if len(text) > 180 {
			text = text[:180]
		}
		sample, truncated := structuralDOMSample(node)
		seen[selector] = true
		out = append(out, DOMBlock{Selector: selector, Sample: string(text), Matches: 1, HTMLSample: sample, HTMLSampleTruncated: truncated})
	})
	// Repeated sibling structures describe catalogs, tables and release lists
	// without requiring article paragraphs or any site-specific selector.
	root.Find("*").AddSelection(root).Each(func(_ int, parent *goquery.Selection) {
		if parent.Closest("nav, header, footer, [role=navigation]").Length() > 0 {
			return
		}
		if blockLimit > 0 && len(out) >= blockLimit {
			return
		}
		groups := map[string]*goquery.Selection{}
		order := []string{}
		parent.ChildrenFiltered("a, div, li, tr, article, section").Each(func(_ int, child *goquery.Selection) {
			if child.Closest("nav, header, footer, [role=navigation]").Length() > 0 {
				return
			}
			key := child.Nodes[0].Data
			if class, ok := child.Attr("class"); ok && class != "" {
				key += "[class=" + strconv.Quote(class) + "]"
			}
			if group, ok := groups[key]; ok {
				groups[key] = group.AddSelection(child)
			} else {
				groups[key] = child
				order = append(order, key)
			}
		})
		for _, key := range order {
			group := groups[key]
			if group.Length() < 2 || (blockLimit > 0 && len(out) >= blockLimit) {
				continue
			}
			selector := groupSelector(doc, parent.Nodes[0], key, group.Length())
			if seen[selector] {
				continue
			}
			text := visibleHTMLText(group.First().Nodes[0])
			if text == "" {
				continue
			}
			sample := []rune(text)
			if len(sample) > 180 {
				sample = sample[:180]
			}
			htmlSample, truncated := structuralDOMSample(group.First())
			seen[selector] = true
			out = append(out, DOMBlock{Selector: selector, Paragraphs: group.First().Find("p").Length(), Sample: string(sample), Matches: group.Length(), HTMLSample: htmlSample, HTMLSampleTruncated: truncated})
		}
	})
	return out
}

func domSelector(node *html.Node) string {
	parts := []string{}
	for n := node; n != nil && n.Type == html.ElementNode; n = n.Parent {
		part := n.Data
		id := ""
		for _, a := range n.Attr {
			if a.Key == "id" {
				id = a.Val
				break
			}
		}
		if id != "" {
			parts = append([]string{part + "[id=" + strconv.Quote(id) + "]"}, parts...)
			break
		}
		index := 1
		for prev := n.PrevSibling; prev != nil; prev = prev.PrevSibling {
			if prev.Type == html.ElementNode && prev.Data == n.Data {
				index++
			}
		}
		part += fmt.Sprintf(":nth-of-type(%d)", index)
		parts = append([]string{part}, parts...)
	}
	return strings.Join(parts, " > ")
}

// shortDOMSelector returns the shortest selector that matches exactly this node.
//
// The positional path is exact but unusable as a menu entry: on a measured list
// page every catalog selector ran 166-249 characters
// ("html:nth-of-type(1) > body:nth-of-type(1) > div:nth-of-type(1) > ...") and
// named nothing a model could reason about, because the containers carried no
// id for the path builder to stop at. A model asked to choose a list-item
// selector from such a menu cannot succeed, which is what a real run showed: it
// guessed selector="a", received 140 navigation anchors, and re-read the same
// bytes until the run died.
//
// So the node is described the way an author would: by tag, id, class, or a
// stable attribute, then by a short ancestor anchor plus one step. Every
// candidate is verified to match exactly this node before it is returned, so a
// shorter selector is never a guess. The positional path remains the fallback
// when nothing else is unique.
func shortDOMSelector(doc *goquery.Document, node *html.Node) string {
	if doc == nil || node == nil || node.Type != html.ElementNode {
		return domSelector(node)
	}
	tag := node.Data
	attrs := map[string]string{}
	for _, a := range node.Attr {
		attrs[a.Key] = a.Val
	}
	candidates := []string{tag}
	if id := attrs["id"]; id != "" {
		candidates = append(candidates, "#"+id, "[id="+strconv.Quote(id)+"]")
	}
	if class := attrs["class"]; class != "" {
		candidates = append(candidates, tag+"[class="+strconv.Quote(class)+"]")
		for _, one := range strings.Fields(class) {
			candidates = append(candidates, tag+"."+one, "."+one)
		}
	}
	for _, key := range []string{"role", "itemprop", "type", "name"} {
		if v := attrs[key]; v != "" {
			candidates = append(candidates, tag+"["+key+"="+strconv.Quote(v)+"]")
		}
	}
	for _, c := range candidates {
		if matchesExactlyOne(doc, c, node) {
			return c
		}
	}
	// Anchor on the nearest ancestor that is itself unique, then walk the
	// intermediate tags down to this node. A single anchor + one step is not
	// enough on a real listing: the date span sits inside ".v_news_list > li",
	// so ".v_news_list span" matches all five rows while
	// ".v_news_list > li > span" matches exactly one. Try the shortest step
	// chain first at each depth, and verify every candidate.
	steps := []string{tag}
	for _, key := range []string{"class", "type", "role", "itemprop"} {
		if v := attrs[key]; v != "" {
			steps = append([]string{tag + "[" + key + "=" + strconv.Quote(v) + "]"}, steps...)
		}
	}
	// Collect the ancestor chain, nearest first, up to a bounded depth.
	chain := []*html.Node{}
	for n := node.Parent; n != nil && n.Type == html.ElementNode && len(chain) < 5; n = n.Parent {
		chain = append(chain, n)
	}
	for depth := 1; depth <= len(chain); depth++ {
		// Intermediate tags between the anchor (at depth-1) and this node, in
		// document order, so the candidate reads anchor > ... > node.
		middle := []string{}
		for i := depth - 2; i >= 0; i-- {
			middle = append([]string{chain[i].Data}, middle...)
		}
		for _, anchor := range ancestorAnchors(chain[depth-1]) {
			for _, step := range steps {
				path := append(append([]string{anchor}, middle...), step)
				for _, sep := range []string{" > ", " "} {
					candidate := strings.Join(path, sep)
					if matchesExactlyOne(doc, candidate, node) {
						return candidate
					}
				}
			}
		}
	}
	return domSelector(node)
}

// ancestorAnchors returns short candidate anchors for an ancestor, shortest
// first. None is required to be unique on its own: the combined selector is
// what must match exactly one node.
func ancestorAnchors(node *html.Node) []string {
	if node == nil || node.Type != html.ElementNode {
		return nil
	}
	tag := node.Data
	out := []string{}
	attrs := map[string]string{}
	for _, a := range node.Attr {
		attrs[a.Key] = a.Val
	}
	if id := attrs["id"]; id != "" {
		out = append(out, "#"+id)
	}
	if class := attrs["class"]; class != "" {
		for _, one := range strings.Fields(class) {
			out = append(out, "."+one, tag+"."+one)
		}
	}
	out = append(out, tag)
	return out
}

// matchesExactlyOne reports whether selector resolves to exactly this node.
func matchesExactlyOne(doc *goquery.Document, selector string, node *html.Node) bool {
	if selector == "" || node == nil {
		return false
	}
	sel := doc.Find(selector)
	return sel.Length() == 1 && sel.Get(0) == node
}

// groupedDOMSelector describes a node that legitimately repeats, such as the
// date span inside every list row.
//
// shortDOMSelector requires uniqueness, so a repeated node can only be named by
// a positional path — but a positional path per row is exactly the noise that
// made a measured menu unusable. For a repeated node the useful description is
// the shared name ("span.date") together with how many nodes carry it: that
// tells a caller "this is the date of each row", which is actionable, while
// "…> li:nth-of-type(3) > span:nth-of-type(1)" is not. The positional path
// remains the fallback when no shared name exists.
func groupedDOMSelector(doc *goquery.Document, node *html.Node) string {
	if doc == nil || node == nil || node.Type != html.ElementNode {
		return domSelector(node)
	}
	tag := node.Data
	attrs := map[string]string{}
	for _, a := range node.Attr {
		attrs[a.Key] = a.Val
	}
	shared := []string{}
	if class := attrs["class"]; class != "" {
		for _, one := range strings.Fields(class) {
			shared = append(shared, tag+"."+one, "."+one)
		}
	}
	for _, key := range []string{"itemprop", "role", "type"} {
		if v := attrs[key]; v != "" {
			shared = append(shared, tag+"["+key+"="+strconv.Quote(v)+"]")
		}
	}
	// Prefer the shortest shared name that matches this node among others. The
	// rank keeps the choice deterministic: the candidate with the most matches
	// while still being short describes the group best.
	best := ""
	bestMatches := 0
	for _, candidate := range shared {
		sel := doc.Find(candidate)
		n := sel.Length()
		if n < 2 {
			continue
		}
		contains := false
		sel.EachWithBreak(func(_ int, s *goquery.Selection) bool {
			if s.Get(0) == node {
				contains = true
				return false
			}
			return true
		})
		if !contains {
			continue
		}
		if best == "" || len(candidate) < len(best) || (len(candidate) == len(best) && n > bestMatches) {
			best, bestMatches = candidate, n
		}
	}
	if best != "" {
		return best
	}
	return shortDOMSelector(doc, node)
}
func groupSelector(doc *goquery.Document, parent *html.Node, key string, want int) string {
	if doc != nil && parent != nil {
		for _, anchor := range ancestorAnchors(parent) {
			for _, sep := range []string{" > ", " "} {
				candidate := anchor + sep + key
				if doc.Find(candidate).Length() == want && want > 0 {
					return candidate
				}
			}
		}
	}
	return domSelector(parent) + " > " + key
}
