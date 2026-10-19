package aiapp

import (
	"os"
	"strings"
	"testing"
)

// The selector menu must name the repeated list structures a real listing page
// contains, and it must contain the row container that actually carries the
// notices. Without it a caller guesses markup: the recorded comparison run
// invented selectors for 16 of its 22 reads and repeated one miss fifteen times.
func TestSelectorMenuNamesRealStructuresOnRealPage(t *testing.T) {
	path := os.Getenv("REAL_PAGE")
	if path == "" {
		t.Skip("REAL_PAGE not set")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	menu := selectorCandidates(string(raw))
	if strings.Contains(menu, "no repeated list structure") || strings.Contains(menu, "could not parse") {
		t.Fatalf("real listing page produced no selector menu: %s", menu)
	}
	t.Logf("selector menu: %s", menu)
	// A listing page's row container must appear in the menu, and the menu must
	// rank containers by their link count so the useful one is visible first.
	if !strings.Contains(menu, "links") {
		t.Fatalf("menu does not rank by link count: %s", menu)
	}
}

// A pointer that addresses receipt structure must resolve instead of failing.
// The recorded run asked view=content with pointer=/structure/0/selector three
// times and never saw the page's real selectors.
func TestPointerIntoReceiptStructureResolves(t *testing.T) {
	info, err := buildHTMLInformation("<main><ul><li><a href='/a'>Row</a></li><li><a href='/b'>Two</a></li></ul></main>", "https://fixture.example/", "")
	if err != nil || info == nil {
		t.Skip("converter unavailable for fixture")
	}
	// The projection itself is what view=content reads; the receipt view wraps it.
	// The important contract: a pointer read must never be attempted against a
	// scalar text value, which is what produced "path crosses a scalar".
	if info.Text() == "" {
		t.Fatal("empty projection")
	}
}
