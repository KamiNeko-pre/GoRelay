package aiapp

import (
	"encoding/json"
	"github.com/PuerkitoBio/goquery"
	"strings"
)

// A menu of observed structure, not a guess about business field semantics.
//
// Entries are ranked so the bounded preview keeps the useful ones. A preview
// that truncates by insertion order loses whatever happened to be built last:
// measured on a real listing page the per-row positional paths were added first
// and the single usable list entry ("​.block > li", matches 15) arrived late, so
// the 2 KiB preview carried seven coordinates and no list at all. Rank puts a
// short selector that matches many nodes first, because that is the shape a
// caller needs to enumerate a page.
func sourceSelectorCatalog(body string) []DOMBlock {
	ranked := completeSourceSelectorCatalog(body)
	out := []DOMBlock{}
	for _, b := range ranked {
		candidate := append(append([]DOMBlock{}, out...), b)
		raw, _ := json.Marshal(candidate)
		if len(raw) > 2<<10 {
			break
		}
		out = candidate
	}
	return out
}

// rankCatalog orders entries by usefulness to a caller choosing a selector:
// short names that match several nodes describe a collection; unique short
// names describe a field; long positional paths describe only themselves.
func rankCatalog(in []DOMBlock) []DOMBlock {
	score := func(b DOMBlock) int {
		s := 0
		if len(b.Selector) <= maxUsableSelectorLength && !isPositionalPath(b.Selector) {
			s += 100
		}
		if b.Matches >= 2 {
			s += 50
		}
		if b.Sample != "" {
			s += 5
		}
		s -= len(b.Selector) / 10
		return s
	}
	out := append([]DOMBlock{}, in...)
	// Stable sort by descending score, preserving discovery order within a rank.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && score(out[j]) > score(out[j-1]); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// maxUsableSelectorLength is the point past which a selector stops being a
// description and becomes a coordinate. Measured on a real listing page, the
// usable entries ("​.block > li", matches 15) were 11-17 characters while the
// noise ran 166-290. A caller cannot choose from a menu where the signal is
// outnumbered by coordinates.
const maxUsableSelectorLength = 80

// isPositionalPath reports whether a selector is only a position, carrying no
// tag/class/id name that describes what it selects.
func isPositionalPath(selector string) bool {
	if selector == "" {
		return false
	}
	return !strings.ContainsAny(selector, ".#[") && strings.Contains(selector, ":nth-of-type")
}

// prunePositionalNoise removes positional paths that describe a region already
// described by a shorter selector.
//
// The rule is redundancy, not length alone: a positional path is dropped when
// another entry carries the same sample text under a shorter selector, because
// then it adds no choice. Genuinely unique regions that only a path can address
// are kept. Pagination, not a silent count ceiling, owns inventory size.
func prunePositionalNoise(in []DOMBlock) []DOMBlock {
	shortBySample := map[string]string{}
	for _, b := range in {
		if len(b.Selector) <= maxUsableSelectorLength && !isPositionalPath(b.Selector) {
			key := strings.Join(strings.Fields(b.Sample), " ")
			if key == "" {
				continue
			}
			if existing, ok := shortBySample[key]; !ok || len(b.Selector) < len(existing) {
				shortBySample[key] = b.Selector
			}
		}
	}
	out := make([]DOMBlock, 0, len(in))
	for _, b := range in {
		if isPositionalPath(b.Selector) || len(b.Selector) > maxUsableSelectorLength {
			key := strings.Join(strings.Fields(b.Sample), " ")
			// Redundant with a short entry: the caller already has a way to name
			// this content, so the coordinate is pure noise.
			if key != "" && shortBySample[key] != "" {
				continue
			}
		}
		out = append(out, b)
	}
	if len(out) == 0 {
		return in
	}
	return out
}

// The model-facing preview is bounded; this inventory is not silently cut.
// read_data pages it from the immutable source, retaining exact CSS locators.
func completeSourceSelectorCatalog(body string) []DOMBlock {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(body))
	if err != nil {
		return nil
	}
	out := []DOMBlock{}
	seen := map[string]bool{}
	add := func(b DOMBlock) {
		if b.Selector == "" || seen[b.Selector] {
			return
		}
		b.HTMLSample = ""
		b.Sample = fitObservationText(b.Sample, 180)
		seen[b.Selector] = true
		out = append(out, b)
	}
	doc.Find("h1,h2,h3,h4,h5,h6,title,[role=heading]").Each(func(_ int, n *goquery.Selection) {
		if n.Closest("nav,header,footer,[hidden],[aria-hidden=true]").Length() > 0 {
			return
		}
		text := normalizeEvidenceText(visibleHTMLText(n.Nodes[0]))
		if text == "" {
			return
		}
		selector := shortDOMSelector(doc, n.Nodes[0])
		add(DOMBlock{Selector: selector, Sample: text, Matches: doc.Find(selector).Length()})
	})
	for _, b := range inspectDOMWithLimits(body, 0, 0) {
		add(b)
	}
	// Short metadata (dates, units, labels) often has no paragraph or heading.
	// Expose observed nodes by structure; do not infer a business field name.
	doc.Find("time,[datetime],span,dt,dd,label,th,td,option,a[href],link[rel=next],link[rel=prev]").Each(func(_ int, n *goquery.Selection) {
		if n.Closest("nav,header,footer,script,style,svg,[hidden],[aria-hidden=true]").Length() > 0 {
			return
		}
		text := normalizeEvidenceText(visibleHTMLText(n.Nodes[0]))
		if text == "" {
			return
		}
		// A short, verified selector plus the real match count. The count is what
		// lets a caller tell a list item ("matches 15") from a single label
		// ("matches 1"); leaving it absent forced a guess on a measured run.
		// A node that legitimately repeats (every row's date) is named by its
		// shared class rather than by a positional path per row.
		selector := groupedDOMSelector(doc, n.Nodes[0])
		add(DOMBlock{Selector: selector, Sample: text, Matches: doc.Find(selector).Length()})
	})
	return rankCatalog(prunePositionalNoise(out))
}

func mustCatalogJSON(body string) string {
	raw, _ := json.Marshal(sourceSelectorCatalog(body))
	return string(raw)
}
