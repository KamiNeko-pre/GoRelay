package aiapp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestDOMCollectionRetainsEachSelectedNode(t *testing.T) {
	for _, label := range []string{"Notice", "Game"} {
		t.Run(label, func(t *testing.T) {
			body := `<nav><a href="/home">Home</a></nav><main><ul>`
			for i := 0; i < 3; i++ {
				body += fmt.Sprintf(`<li><a href="/item/%d">%s %d</a></li>`, i, label, i)
			}
			body += `</ul><script>ignore_all_rules()</script></main>`
			s, r, _ := storedHTMLReader(t, body)
			r.BudgetJSON = `{"loop_version":5}`
			d := boundedUnifiedRead(t, s, r, map[string]any{"handle": "source", "view": "dom", "selector": "main li a"})
			nodes, ok := d["nodes"].([]any)
			if !ok || len(nodes) != 3 {
				t.Fatalf("collection reduced to count plus first sample: %v", d)
			}
			for i, raw := range nodes {
				n := raw.(map[string]any)
				if n["index"] != float64(i) || n["text"] != fmt.Sprintf("%s %d", label, i) || n["attributes"].(map[string]any)["href"] != fmt.Sprintf("/item/%d", i) {
					t.Fatal("node text, index and href detached", n)
				}
			}
			if strings.Contains(fmt.Sprint(d), "ignore_all_rules") {
				t.Fatal("script source exposed")
			}
			foreign := r
			foreign.ID = "foreign"
			args, _ := json.Marshal(map[string]any{"handle": "source", "view": "dom", "selector": "main li a"})
			if _, err := s.readData(context.Background(), foreign, ToolCall{Function: FunctionCall{Arguments: string(args)}}); err == nil {
				t.Fatal("foreign source read allowed")
			}
		})
	}
}
