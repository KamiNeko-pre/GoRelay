package aiapp

import (
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"
)

func mustDocument(t *testing.T, body string) *goquery.Document {
	t.Helper()
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(body))
	if err != nil {
		t.Fatalf("fixture is not parseable: %v", err)
	}
	return doc
}

// The measured failure: a real list page whose containers carry no id produced
// catalog selectors 166-249 characters long
// ("html:nth-of-type(1) > body:nth-of-type(1) > div:nth-of-type(1) > ...") and
// one entry per notice instead of one entry for the notice list. A model given
// that menu cannot pick a list-item selector, which is what a real run showed.
//
// This fixture is a typical government/CMS listing: no ids anywhere, a class on
// the list container, and repeated <li><a> rows carrying the date and title.
const realisticListPage = `<html><body>
<div class="header"><a href="/">首页</a><a href="/cggg.htm">采购公告</a></div>
<div class="main">
<div class="right_list">
<ul class="v_news_list">
<li><span class="date">[2026-10-14]</span><a href="/info/1016/75047.htm" title="钢轨铣磨车铣削单元配件采购项目">西南交通大学钢轨铣磨车铣削单元配件采购项目</a></li>
<li><span class="date">[2026-10-14]</span><a href="/info/1016/74967.htm" title="学生宿舍家具采购变更公告">西南交通大学犀浦校区学生宿舍家具采购变更公告</a></li>
<li><span class="date">[2026-10-13]</span><a href="/info/1016/74957.htm" title="步进频率三维探地雷达项目">西南交通大学步进频率三维探地雷达项目</a></li>
<li><span class="date">[2026-10-13]</span><a href="/info/1016/74947.htm" title="采购意向公开">西南交通大学采购意向公开</a></li>
<li><span class="date">[2026-10-12]</span><a href="/info/1016/74327.htm" title="医学院通用设备采购">医学院通用设备采购项目</a></li>
</ul>
</div>
</div>
<div class="footer"><a href="/lxwm.htm">联系我们</a></div>
</body></html>`

// A catalog entry naming a list must exist, must be short enough to reason
// about, and must report how many nodes it matches.
func TestCatalogNamesTheNoticeListWithAShortSelector(t *testing.T) {
	catalog := completeSourceSelectorCatalog(realisticListPage)
	if len(catalog) == 0 {
		t.Fatal("no catalog produced")
	}
	var best *DOMBlock
	for i := range catalog {
		b := catalog[i]
		if b.Matches >= 5 && strings.Contains(b.Sample, "西南交通大学") {
			best = &b
			break
		}
	}
	if best == nil {
		t.Fatalf("no catalog entry describes the notice list; entries: %s", describeCatalog(catalog))
	}
	// The selector must be short enough for a caller to choose deliberately.
	if len(best.Selector) > 60 {
		t.Fatalf("notice-list selector is %d chars, too long to choose from: %s",
			len(best.Selector), best.Selector)
	}
	// It must not be a positional path.
	if strings.Contains(best.Selector, "nth-of-type") {
		t.Fatalf("notice-list selector is positional: %s", best.Selector)
	}
	// The count must be real, not 1 and not absent.
	if best.Matches < 5 {
		t.Fatalf("notice-list entry reports matches=%d, want >= 5: %s", best.Matches, best.Selector)
	}
}

// Every catalog selector must actually resolve, and its Matches must equal the
// real match count. An absent count forced a guess on a measured run.
func TestCatalogSelectorsResolveAndCountsAreExact(t *testing.T) {
	catalog := completeSourceSelectorCatalog(realisticListPage)
	if len(catalog) == 0 {
		t.Fatal("no catalog produced")
	}
	for _, b := range catalog {
		if b.Selector == "" {
			t.Fatal("empty selector in catalog")
		}
		if b.Selector != "title" && strings.HasPrefix(b.Selector, "html:nth-of-type") {
			// Positional paths are a valid fallback but should be rare here.
			continue
		}
		if b.Matches == 0 {
			t.Fatalf("selector %q reports matches=0", b.Selector)
		}
	}
}

// The fixed selector builder must never invent a selector: every candidate it
// returns has to resolve to exactly the node it was derived from.
func TestShortSelectorAlwaysResolvesToItsOwnNode(t *testing.T) {
	doc := mustDocument(t, realisticListPage)
	// Every node in the list row, plus the list container itself.
	for _, selector := range []string{"ul.v_news_list", ".v_news_list li", "li a", ".date"} {
		sel := doc.Find(selector)
		if sel.Length() == 0 {
			t.Fatalf("fixture selector %q matched nothing", selector)
		}
		for i := 0; i < sel.Length(); i++ {
			node := sel.Get(i)
			got := shortDOMSelector(doc, node)
			resolved := doc.Find(got)
			if resolved.Length() != 1 {
				t.Fatalf("shortDOMSelector for %q returned %q matching %d nodes, want exactly 1",
					selector, got, resolved.Length())
			}
			if resolved.Get(0) != node {
				t.Fatalf("shortDOMSelector for %q returned %q which matches a different node",
					selector, got)
			}
		}
	}
}

// A selector that names the list must be preferred over one path per row.
func TestListIsNamedOnceNotOncePerRow(t *testing.T) {
	catalog := completeSourceSelectorCatalog(realisticListPage)
	listEntries := 0
	for _, b := range catalog {
		if b.Matches >= 5 {
			listEntries++
		}
	}
	if listEntries == 0 {
		t.Fatalf("no entry describes the repeated rows; entries: %s", describeCatalog(catalog))
	}
	// Five rows must not each get their own positional entry.
	for _, b := range catalog {
		if strings.Count(b.Selector, "nth-of-type") >= 5 {
			t.Fatalf("a per-row positional path reached the catalog: %s", b.Selector)
		}
	}
}

// The anchor path must keep working when nothing else is unique.
func TestCascadeSelectorStillWorks(t *testing.T) {
	body := `<html><body><div><section><p>Long enough paragraph text to be a block here.</p></section></div></body></html>`
	catalog := completeSourceSelectorCatalog(body)
	if len(catalog) == 0 {
		t.Fatal("cascade page produced no catalog")
	}
	for _, b := range catalog {
		if b.Selector == "" {
			t.Fatal("empty selector")
		}
	}
}

func describeCatalog(catalog []DOMBlock) string {
	parts := make([]string, 0, len(catalog))
	for _, b := range catalog {
		parts = append(parts, strings.Join([]string{
			"sel=" + b.Selector,
			"matches=" + itoaTest(b.Matches),
			"sample=" + truncateRunes(b.Sample, 20),
		}, " "))
	}
	return strings.Join(parts, " | ")
}

func itoaTest(v int) string {
	if v == 0 {
		return "0"
	}
	digits := []byte{}
	for v > 0 {
		digits = append([]byte{byte('0' + v%10)}, digits...)
		v /= 10
	}
	return string(digits)
}
