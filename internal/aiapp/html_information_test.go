package aiapp

import (
	"encoding/json"
	"strings"
	"testing"

	"go-lua-crawler/internal/acquisition"
)

func TestHTMLModelObservationConvertsBeforeClipping(t *testing.T) {
	for _, body := range []string{
		`<!doctype html><html><head><style>` + strings.Repeat(".noise{fill:red;}", 2000) + `</style></head><body><main><svg><path d="` + strings.Repeat("M0 1 ", 2000) + `"/></svg><h1>Plans</h1><p>Basic costs 1 credit per request, only when search_depth=basic.</p></main></body></html>`,
		`<html><body><nav>` + strings.Repeat(`<a href="/menu">Menu</a>`, 300) + `</nav><article><table><thead><tr><th>Mode</th><th>Credits</th><th>Unit</th><th>Condition</th></tr></thead><tbody><tr><td>Basic</td><td>1</td><td>per request</td><td>search_depth=basic only</td></tr></tbody></table></article></body></html>`,
	} {
		inspection := inspectResponse(acquisition.Response{Receipt: acquisition.Receipt{URL: "https://fixture.example/docs", Status: 200}, Body: body}, acquisition.Stats{})
		raw, _ := json.Marshal(inspection)
		view := modelObservation(ToolResult{CallID: "source", Name: "inspect_source", Data: raw})
		var got sourceInspection
		if err := json.Unmarshal(view.Data, &got); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(got.Source.Body, "<") || !strings.Contains(strings.ToLower(got.Source.Body), "basic") || !strings.Contains(got.Source.Body, "per request") || !strings.Contains(got.Source.Body, "search_depth=basic") {
			t.Fatalf("HTML noise displaced business meaning: %.350s", got.Source.Body)
		}
		if len(view.Data) > observationBytes {
			t.Fatal("view exceeded existing observation limit")
		}
		if inspection.Source.Body != body {
			t.Fatal("projection changed canonical original")
		}
	}
}

func TestHTMLInformationPreservesTableContextJSONAndCode(t *testing.T) {
	body := `<html><head><title>Plans</title><script type="application/ld+json">{"@type":"Offer","price":"0.008","priceCurrency":"USD"}</script><script type="application/json">{"limits":{"requests":10,"scope":"per second"}}</script><script>window.bad = "do not treat JS as data"</script></head><body><main><h1>Search</h1><table id="prices"><caption>Per request, subscriptions excluded</caption><thead><tr><th>Mode</th><th>Credits</th><th>Condition</th></tr></thead><tbody><tr><th>Basic</th><td>1</td><td>only basic</td></tr><tr><th>Advanced</th><td>2</td><td>only advanced</td></tr></tbody></table><p>See <a href="../billing?region=cn">billing conditions</a>.</p><pre>local rows = {}\nreturn true, {items=rows}</pre></main></body></html>`
	view, err := buildHTMLInformation(body, "https://fixture.example/docs/search", "")
	if err != nil || view == nil || view.Version == "" || len(view.Embedded) != 2 {
		t.Fatal("structured view missing", view, err)
	}
	text := view.Text()
	for _, wanted := range []string{"Mode", "Credits", "Condition", "Basic", "Advanced", "Per request, subscriptions excluded", "only advanced", "https://fixture.example/billing?region=cn", "local rows", "embedded_json"} {
		if !strings.Contains(text, wanted) {
			t.Fatal("necessary information lost", wanted, text)
		}
	}
	if strings.Contains(text, "window.bad") || strings.Contains(text, "<script") {
		t.Fatal("executable script became model reading text")
	}
	rows, code := 0, 0
	for _, block := range view.Blocks {
		if block.ID == "" || block.Selector == "" {
			t.Fatal("block cannot be located in original", block)
		}
		if block.Kind == "table_row" {
			rows++
			if !strings.Contains(block.Text, "Credits") || !strings.Contains(block.Text, "subscriptions excluded") {
				t.Fatal("row detached from headers/unit/conditions", block)
			}
		}
		if block.Kind == "code" {
			code++
		}
	}
	if rows != 2 || code != 1 {
		t.Fatal("table or code block count incorrect", rows, code)
	}
	if !strings.Contains(string(view.Embedded[0].Value), "0.008") || !strings.Contains(string(view.Embedded[1].Value), "per second") {
		t.Fatal("embedded JSON discarded before cleaning")
	}
}

func TestHTMLInformationSelectedRegionAndPlainJSONProtection(t *testing.T) {
	body := `<nav>Outside navigation</nav><main><section id="data"><div>Basic <span>1 credit</span> per request</div><ul><li>Only public data</li><li>Per-request billing</li></ul></section><section>Unselected text</section></main>`
	view, err := buildHTMLInformation(body, "https://fixture.example/", "#data")
	if err != nil || view == nil || !strings.Contains(view.Text(), "1 credit") || !strings.Contains(view.Text(), "Only public data") || strings.Contains(view.Text(), "Unselected text") || strings.Contains(view.Text(), "Outside navigation") {
		t.Fatal("selector changed meaning or range", view, err)
	}
	if _, err := buildHTMLInformation(body, "https://fixture.example/", ".absent"); err == nil {
		t.Fatal("missing selector invented a whole-page read")
	}
	for _, original := range []string{`{"body":"<p>markup inside JSON</p>"}`, strings.Repeat("x", 3000)} {
		view, err := buildHTMLInformation(original, "https://fixture.example/", "")
		if err != nil || view != nil {
			t.Fatal("non-HTML response converted", view, err)
		}
	}
}

func TestHTMLInformationParseFailureNeverFallsBackToRawHTML(t *testing.T) {
	for _, tag := range []string{"div", "section"} {
		body := strings.Repeat("<"+tag+">", 520) + "nested document" + strings.Repeat("</"+tag+">", 520)
		if _, err := buildHTMLInformation(body, "https://fixture.example/", ""); err == nil {
			t.Fatal("fixture did not reach actual parser depth boundary")
		}
		original := inspectResponse(acquisition.Response{Body: body, Receipt: acquisition.Receipt{URL: "https://fixture.example/", Status: 200}}, acquisition.Stats{})
		// Older receipts lack a stored conversion error and must fail closed too.
		for _, receipt := range []sourceInspection{original, {Source: original.Source}} {
			raw, _ := json.Marshal(receipt)
			view := modelObservation(ToolResult{CallID: "source", Name: "inspect_source", Data: raw})
			var inspection sourceInspection
			if err := json.Unmarshal(view.Data, &inspection); err != nil {
				t.Fatal(err)
			}
			if inspection.Source.Body != "" {
				t.Fatal("reader failure restored raw HTML to model context")
			}
			var fields map[string]any
			_ = json.Unmarshal(view.Data, &fields)
			if fields["view_error"] == nil || fields["view_error"] == "" {
				t.Fatal("unreadable input hidden as empty successful content")
			}
			if original.Source.Body != body {
				t.Fatal("failed conversion damaged canonical original")
			}
		}
	}
}
