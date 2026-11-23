package ruleconfig

import (
	"strings"
	"testing"
)

func TestExtractKeepsArticleBlocksAndExcludesNavigation(t *testing.T) {
	html := `<html><nav>Ignore navigation</nav><article><h1>Context</h1><div class="body"><h2>Cancellation</h2><p>Cancel the parent.</p><pre>select {
case &lt;-ctx.Done():
    return
}</pre><p>Release resources.</p><script>ignore()</script></div></article></html>`
	config := Config{TitleSelector: "article h1", BodySelector: "article .body"}
	result, err := Extract(html, config)
	if err != nil {
		t.Fatal(err)
	}
	if result.Title != "Context" || strings.Contains(result.Body, "Ignore navigation") || strings.Contains(result.Body, "ignore()") || !strings.Contains(result.Body, "case <-ctx.Done():\n    return") || !strings.Contains(result.Body, "Release resources.") {
		t.Fatalf("invalid extraction: %#v", result)
	}
}

func TestExtractRejectsAmbiguousOrMissingFields(t *testing.T) {
	cases := []struct {
		html   string
		config Config
	}{
		{`<p>service unavailable</p>`, Config{TitleSelector: "h1", BodySelector: "article"}},
		{`<h1>A</h1><h1>B</h1><article>body</article>`, Config{TitleSelector: "h1", BodySelector: "article"}},
		{`<h1>A</h1><article>one</article><article>two</article>`, Config{TitleSelector: "h1", BodySelector: "article"}},
		{`<h1>A</h1><article><script>not evidence</script></article>`, Config{TitleSelector: "h1", BodySelector: "article"}},
		{`<h1>A</h1><article>body</article>`, Config{TitleSelector: "[", BodySelector: "article"}},
		{`<h1>A</h1><article>body</article>`, Config{TitleSelector: "h1", BodySelector: ""}},
	}
	for index, input := range cases {
		if _, err := Extract(input.html, input.config); err == nil {
			t.Errorf("case %d accepted invalid extraction", index)
		}
	}
}
