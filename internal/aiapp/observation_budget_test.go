package aiapp

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// The layered budget must keep a whole-page structure dump and the paged text
// from competing for one ceiling. Before the split, the first read of the
// recorded run was rejected outright ("metadata exceeds observation budget")
// because the structure rode in the same envelope as everything else, and the
// model never recovered from losing that first read.
func TestStructureDumpDoesNotRejectFirstRead(t *testing.T) {
	path := os.Getenv("REAL_PAGE")
	if path == "" {
		t.Skip("REAL_PAGE not set")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s, run, _ := storedHTMLReader(t, string(raw))
	ctx := context.Background()

	// Exactly what the recorded model asked first.
	out, err := s.readData(ctx, run, ToolCall{ID: "read-1", Function: FunctionCall{
		Name:      "read_data",
		Arguments: `{"handle":"source","pointer":"/structure/0/selector","view":"content"}`,
	}})
	if err != nil {
		t.Fatalf("first read must succeed under layered budgets: %v", err)
	}
	var data map[string]any
	if err := json.Unmarshal(out.Data, &data); err != nil {
		t.Fatal(err)
	}
	entries, ok := data["structure"].([]any)
	if !ok || len(entries) == 0 {
		t.Fatalf("structure list missing from first read: %s", out.Data[:min(200, len(out.Data))])
	}
	if len(entries) < 6 {
		t.Fatalf("structure trimmed too far: %d entries", len(entries))
	}
	// The continuation must survive next to the guidance.
	if data["next_cursor"] != nil && data["next_read"] == nil && data["handle"] == nil {
		t.Fatal("continuation lost alongside guidance")
	}
	t.Logf("structure entries delivered: %d, payload %d bytes", len(entries), len(out.Data))
}

// Guidance must survive the model view even when the text is oversized, and the
// text must survive even when guidance is large: neither side may crowd the other.
func TestLayeredBudgetKeepsBothSides(t *testing.T) {
	big := strings.Repeat("正文段落。", 3000) // ~12KB of text
	info, err := buildHTMLInformation("<main><p>"+big+"</p></main>", "https://fixture.example/", "")
	if err != nil || info == nil {
		t.Fatal(err)
	}
	nav := map[string]any{
		"selector_menu": `"ul li" (71 links), "li p" (29 links)`,
		"regions":       blockDirectory(info),
	}
	core := map[string]any{"text": big, "total_runes": len([]rune(big))}
	// Split must classify, and neither bucket may borrow from the other.
	c, n := splitReadPayload(map[string]any{"text": core["text"], "total_runes": core["total_runes"], "selector_menu": nav["selector_menu"], "regions": nav["regions"]})
	if _, ok := c["selector_menu"]; ok {
		t.Fatal("selector_menu classified as core")
	}
	if _, ok := n["text"]; ok {
		t.Fatal("text classified as navigation")
	}
	if !coreOverBudget(c) {
		t.Fatal("12KB of text must be over the core budget")
	}
	bounded := boundNavigation(n)
	raw, _ := json.Marshal(bounded)
	if len(raw) > navigationBytes {
		t.Fatalf("navigation bound failed: %d bytes", len(raw))
	}
	if _, ok := bounded["selector_menu"]; !ok {
		t.Fatal("navigation trimming dropped the selector menu")
	}
}
