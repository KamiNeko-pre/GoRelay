package ruleconfig

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestGoBlogConfigSelectsArticleAndRejectsAmbiguity(t *testing.T) {
	raw, err := os.ReadFile("../../configs/knowledge/go-blog.json")
	if err != nil {
		t.Fatal(err)
	}
	var config Config
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		html    string
		title   string
		body    string
		wantErr bool
	}{
		{"context_page_structure", `<div class="Article" data-slug="/blog/context"><h1>Go Concurrency Patterns: Context</h1><p>Done returns a channel.</p></div><div class="Article prevnext"><h1 class="small">NAVIGATION ONLY</h1></div>`, "Go Concurrency Patterns: Context", "Done returns a channel", false},
		{"different_article_with_navigation", `<div class="Article" data-slug="/blog/pipelines"><h1>Pipelines</h1><p>Close output channels after sending.</p></div><div class="Article prevnext"><h1 class="small">NAVIGATION ONLY</h1></div>`, "Pipelines", "Close output channels", false},
		{"single_article_baseline", `<div class="Article" data-slug="/blog/example"><h1>Example</h1><p>Useful body.</p></div>`, "Example", "Useful body.", false},
		{"two_true_articles_remain_invalid", `<div class="Article" data-slug="/blog/a"><h1>A</h1><p>One</p></div><div class="Article" data-slug="/blog/b"><h1>B</h1><p>Two</p></div>`, "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := Extract(tc.html, config)
			if tc.wantErr {
				if err == nil {
					t.Fatal("accepted ambiguous article containers")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if result.Title != tc.title || !strings.Contains(result.Body, tc.body) || strings.Contains(result.Body, "NAVIGATION ONLY") {
				t.Fatalf("wrong extracted article: %#v", result)
			}
		})
	}
}
