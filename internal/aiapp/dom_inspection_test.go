package aiapp

import (
	"github.com/PuerkitoBio/goquery"
	"strings"
	"testing"
)

func TestDOMInspectionSelectorsComeFromActualStructure(t *testing.T) {
	for _, body := range []string{`<html><body><div id="article"><p>This paragraph contains sufficiently long source text for structural inspection.</p><p>Second paragraph.</p></div></body></html>`, `<html><body><section><div><p>This is a different document layout containing a long paragraph.</p></div></section></body></html>`} {
		blocks := inspectDOM(body)
		if len(blocks) != 1 {
			t.Fatalf("wrong blocks: %+v", blocks)
		}
		doc, _ := goquery.NewDocumentFromReader(strings.NewReader(body))
		if doc.Find(blocks[0].Selector).Length() != 1 || !strings.Contains(doc.Find(blocks[0].Selector).Text(), blocks[0].Sample) {
			t.Fatal("selector/sample not grounded in DOM")
		}
	}
	if len(inspectDOM(`{"version":"1.0"}`)) != 0 {
		t.Fatal("JSON source gained fabricated HTML structure")
	}
}

func TestDOMInspectionIncludesRepeatedListsAndTables(t *testing.T) {
	for _, body := range []string{
		`<html><head><script>` + strings.Repeat("header-data", 3000) + `</script></head><body><div id="catalog"><a class="item" data-id="a"><span>Alpha</span><span>12</span></a><a class="item" data-id="b"><span>Beta</span><span>18</span></a></div></body></html>`,
		`<html><body><table><tbody><tr><td>Library A</td><td>1.2</td></tr><tr><td>Library B</td><td>2.3</td></tr></tbody></table></body></html>`,
	} {
		blocks := inspectDOM(body)
		if len(blocks) == 0 {
			t.Fatal("a structured list without long paragraphs was invisible")
		}
		doc, _ := goquery.NewDocumentFromReader(strings.NewReader(body))
		for _, b := range blocks {
			selected := doc.Find(b.Selector)
			if selected.Length() == 0 || !strings.Contains(strings.Join(strings.Fields(selected.First().Text()), " "), b.Sample) {
				t.Fatalf("structural observation not grounded: %+v", b)
			}
			if len([]rune(b.Sample)) > 180 || strings.Contains(b.Sample, "header-data") {
				t.Fatal("unbounded or noncontent structural sample")
			}
		}
	}
}

func TestDOMInspectionPrefersDeclaredContentOverNavigation(t *testing.T) {
	body := `<html><body><nav><p>This navigation text is deliberately longer than a paragraph sample.</p>` + strings.Repeat(`<a class="nav-link">Navigation</a>`, 30) + `</nav><main><table><tr><td>Package A</td><td>1.0</td></tr><tr><td>Package B</td><td>2.0</td></tr></table><pre>GET /repos/{owner}/{repo}/releases</pre></main></body></html>`
	blocks := inspectDOM(body)
	foundData, foundCode := false, false
	for _, b := range blocks {
		foundData = foundData || strings.Contains(b.Sample, "Package A")
		foundCode = foundCode || strings.Contains(b.Sample, "GET /repos")
		if strings.Contains(b.Sample, "Navigation") || strings.Contains(b.Sample, "navigation text") {
			t.Fatal("navigation displaced declared main content")
		}
	}
	if !foundData || !foundCode {
		t.Fatalf("declared content invisible: %+v", blocks)
	}
}

func TestDOMStructuralSamplesKeepSelectorsWithoutExecutableNoise(t *testing.T) {
	for _, tag := range []string{"article", "div"} {
		row := `<` + tag + ` class="entry" data-key="a" style="` + strings.Repeat("fill:red;", 500) + `" onclick="window.untrusted()"><svg><path d="` + strings.Repeat("M0 1 ", 500) + `"/></svg><span>Alpha 1 credit</span></` + tag + `>`
		body := `<main><section id="items">` + row + strings.ReplaceAll(row, "Alpha", "Beta") + `</section><pre>return true, {items={}}</pre></main>`
		blocks := inspectDOM(body)
		found := false
		for _, block := range blocks {
			if block.Matches != 2 {
				continue
			}
			found = true
			if strings.Contains(block.HTMLSample, "<svg") || strings.Contains(block.HTMLSample, "fill:red") || strings.Contains(block.HTMLSample, "onclick") || !strings.Contains(block.HTMLSample, "data-key") || !strings.Contains(block.HTMLSample, "Alpha") {
				t.Fatal("DOM sample lost useful structure behind noise", block.HTMLSample)
			}
			doc, _ := goquery.NewDocumentFromReader(strings.NewReader(body))
			if doc.Find(block.Selector).Length() != 2 {
				t.Fatal("cleaning changed original selector identity")
			}
		}
		if !found {
			t.Fatal("repeated structure disappeared")
		}
	}
}
