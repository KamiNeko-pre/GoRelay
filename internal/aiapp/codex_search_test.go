package aiapp

import (
	"strings"
	"testing"
)

func TestCodexSearchPreservesDiscoverySnippets(t *testing.T) {
	for _, snippet := range []string{"Public JSON endpoint and region parameters described by this search result.", "This official changelog lists public release feeds."} {
		t.Run(snippet, func(t *testing.T) {
			candidate := SearchCandidate{URL: "https://public.example/document", Title: "Source title", Snippet: snippet}
			raw := searchMetadataEvents([]SearchCandidate{candidate}, []SearchCandidate{candidate})
			got, err := parseCodexSearch(strings.NewReader(raw), 5)
			if err != nil || len(got.Candidates) != 1 || got.Candidates[0].Snippet != snippet || got.Candidates[0].Discovery != "codex_cli_web_search" || got.Usage.TotalTokens != 120 {
				t.Fatalf("metadata lost: %+v %v", got, err)
			}
		})
	}
}

func TestCodexSearchRequiresActualSearchAndPreservesCandidateURLs(t *testing.T) {
	for _, target := range []string{"https://example.org/report/2024", "https://go.dev/doc/?v=1&lang=en"} {
		raw := `{"type":"thread.started","thread_id":"search-one"}
{"type":"item.completed","item":{"type":"web_search","query":"public report","results":[{"type":"text_result","url":"` + target + `","title":"Source title"}]}}
{"type":"item.completed","item":{"type":"agent_message","text":"{\"candidates\":[{\"url\":\"` + target + `\",\"title\":\"Source title\"}]}"}}
{"type":"turn.completed","usage":{"input_tokens":123,"output_tokens":20}}`
		got, err := parseCodexSearch(strings.NewReader(raw), 5)
		if err != nil || len(got.Candidates) != 1 || got.Candidates[0].URL != target || got.RequestID != "search-one" || got.Usage == nil || got.Usage.TotalTokens != 143 {
			t.Fatalf("bad real discovery: %+v %v", got, err)
		}
	}
	for _, item := range []string{"", `{"type":"item.completed","item":{"type":"command_execution"}}`, `{"type":"item.completed","item":{"type":"mcp_tool_call"}}`} {
		raw := item + "\n" + `{"type":"item.completed","item":{"type":"agent_message","text":"{\"candidates\":[{\"url\":\"https://example.org/invented\",\"title\":\"Unobserved\"}]}"}}
{"type":"turn.completed"}`
		if _, err := parseCodexSearch(strings.NewReader(raw), 5); err == nil {
			t.Fatal("native execution or unsearched URLs accepted")
		}
	}
}

func TestCodexSearchEmptyResultsAreExplicitAndInferenceStaysToolFree(t *testing.T) {
	raw := `{"type":"item.completed","item":{"type":"web_search","query":"no results"}}
{"type":"item.completed","item":{"type":"agent_message","text":"{\"candidates\":[]}"}}
{"type":"turn.completed"}`
	got, err := parseCodexSearch(strings.NewReader(raw), 5)
	if err != nil || len(got.Candidates) != 0 || got.Usage != nil {
		t.Fatalf("legitimate empty search changed: %+v %v", got, err)
	}
	if _, err := parseCodexInference(strings.NewReader(raw), ChatRequest{}, "test"); err == nil {
		t.Fatal("decision inference gained native search permission")
	}
}
