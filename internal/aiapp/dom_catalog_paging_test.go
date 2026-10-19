package aiapp

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/PuerkitoBio/goquery"
	"go-lua-crawler/internal/agentdata"
	"os"
	"strings"
	"testing"
)

func TestDOMCatalogCanReachObservedStructureBeyondPreview(t *testing.T) {
	listing, _ := os.ReadFile("testdata/dom-catalog-listing-20261014.html")
	detail, _ := os.ReadFile("testdata/dom-catalog-detail-20261014.html")
	var sibling strings.Builder
	sibling.WriteString("<html><body>")
	for i := 0; i < 25; i++ {
		fmt.Fprintf(&sibling, "<section><h5>Section %d</h5></section>", i)
	}
	sibling.WriteString(`<div id="offers"><a class="offer" href="/one">One</a><a class="offer" href="/two">Two</a></div><span class="stamp">2026-10-14</span></body></html>`)
	for _, tc := range []struct {
		name, body string
		match      func(*goquery.Document, DOMBlock) bool
	}{
		{"recorded_list", string(listing), func(d *goquery.Document, b DOMBlock) bool {
			return b.Matches >= 3 && d.Find(b.Selector).First().Find(`a[href]`).Length() > 0
		}},
		{"recorded_date", string(detail), func(d *goquery.Document, b DOMBlock) bool {
			return strings.Contains(d.Find(b.Selector).Text(), "日期：") && len([]rune(d.Find(b.Selector).Text())) < 200
		}},
		{"different_layout", sibling.String(), func(d *goquery.Document, b DOMBlock) bool {
			return b.Matches == 2 && d.Find(b.Selector).First().AttrOr("href", "") != ""
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.body == "" {
				t.Fatal("source fixture missing")
			}
			s, run, _ := storedHTMLReader(t, tc.body)
			doc, _ := goquery.NewDocumentFromReader(strings.NewReader(tc.body))
			args := dataReadArgs{Handle: "source", View: "dom", Limit: 3}
			seen := map[string]bool{}
			found := false
			for page := 0; page < 1000; page++ {
				raw, _ := json.Marshal(args)
				out, err := s.readData(context.Background(), run, ToolCall{ID: fmt.Sprint(page), Function: FunctionCall{Name: "read_data", Arguments: string(raw)}})
				if err != nil {
					t.Fatal(err)
				}
				var data struct {
					Catalog []DOMBlock    `json:"selector_catalog"`
					Next    *dataReadArgs `json:"next_read"`
					Total   int           `json:"total_selectors"`
				}
				if err = json.Unmarshal(out.Data, &data); err != nil {
					t.Fatal(err)
				}
				if data.Total == 0 {
					t.Fatal("inventory does not identify completeness or continuation", string(out.Data))
				}
				for _, b := range data.Catalog {
					if seen[b.Selector] {
						t.Fatal("pagination repeated selector")
					}
					seen[b.Selector] = true
					if doc.Find(b.Selector).Length() == 0 {
						t.Fatal("fabricated selector", b.Selector)
					}
					found = found || tc.match(doc, b)
				}
				if data.Next == nil {
					if len(seen) != data.Total {
						t.Fatal("catalog dropped entries", len(seen), data.Total)
					}
					break
				}
				if data.Next.Offset <= args.Offset || data.Next.Handle != "source" || data.Next.View != "dom" {
					t.Fatal("invalid continuation", data.Next)
				}
				args = *data.Next
			}
			if !found {
				t.Fatal("requested observed structure is inaccessible")
			}
		})
	}
}

func TestDOMCatalogContinuationSurvivesModelBoundary(t *testing.T) {
	body, _ := os.ReadFile("testdata/dom-catalog-listing-20261014.html")
	s, run, _ := storedHTMLReader(t, string(body))
	args := dataReadArgs{Handle: "source", View: "dom", Limit: 3}
	raw, _ := json.Marshal(args)
	call := ToolCall{ID: "catalog-page", Type: "function", Function: FunctionCall{Name: "read_data", Arguments: string(raw)}}
	result, err := s.readData(context.Background(), run, call)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(result)
	if err = s.Data.DB.Create(&agentdata.Operation{RunID: run.ID, Key: "tool:" + call.ID, Kind: "read_data", Stage: "succeeded", ResultJSON: string(encoded)}).Error; err != nil {
		t.Fatal(err)
	}
	state := decisionState()
	state.Run = run
	state.Run.BudgetJSON = `{"loop_version":5}`
	state.Budget.LoopVersion = 5
	state.Operations, _ = s.Data.Operations(context.Background(), run.ID)
	state.Messages = []Message{{Role: "assistant", ReasoningContent: "I will follow the saved structure continuation.", ToolCalls: []ToolCall{call}}, {Role: "tool", ToolCallID: call.ID, Content: string(encoded)}}
	messages, err := CompactContext(state, state.Messages)
	if err != nil {
		t.Fatal(err)
	}
	found, thinking := false, false
	for _, m := range messages {
		if m.Role == "assistant" && m.ReasoningContent != "" {
			thinking = true
		}
		if m.Role == "tool" && m.ToolCallID == call.ID {
			var got ToolResult
			json.Unmarshal([]byte(m.Content), &got)
			if !sameContractJSON(got.Data, result.Data) {
				t.Fatal("current paged structure rewritten before model", string(got.Data))
			}
			found = true
		}
	}
	if !found || !thinking {
		t.Fatal("structure/continuation missing at actual model boundary", found, thinking)
	}
}

func TestDOMNodeCollectionCanContinueWithoutGuessingSelectors(t *testing.T) {
	var body strings.Builder
	body.WriteString(`<main id="data">`)
	for i := 0; i < 90; i++ {
		fmt.Fprintf(&body, `<a class="item" href="/item/%d">Item %d</a>`, i, i)
	}
	body.WriteString(`</main>`)
	s, run, _ := storedHTMLReader(t, body.String())
	args := dataReadArgs{Handle: "source", View: "dom", Selector: "a.item", Limit: 7}
	seen := 0
	for page := 0; page < 30; page++ {
		raw, _ := json.Marshal(args)
		out, err := s.readData(context.Background(), run, ToolCall{ID: fmt.Sprint(page), Function: FunctionCall{Name: "read_data", Arguments: string(raw)}})
		if err != nil {
			t.Fatal(err)
		}
		var data struct {
			Nodes []struct {
				Index      int               `json:"index"`
				Attributes map[string]string `json:"attributes"`
			} `json:"nodes"`
			Next *dataReadArgs `json:"next_read"`
		}
		json.Unmarshal(out.Data, &data)
		for _, n := range data.Nodes {
			if n.Index != seen || n.Attributes["href"] != fmt.Sprintf("/item/%d", seen) {
				t.Fatal("node/attribute alignment lost", n)
			}
			seen++
		}
		if data.Next == nil {
			break
		}
		args = *data.Next
	}
	if seen != 90 {
		t.Fatal("matched nodes silently inaccessible", seen)
	}
}
