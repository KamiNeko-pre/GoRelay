package aiapp

import (
	"encoding/json"
	"strings"
	"testing"
)

func searchMetadataEvents(native, proposed []SearchCandidate) string {
	results := make([]map[string]string, 0, len(native))
	for _, item := range native {
		results = append(results, map[string]string{"type": "text_result", "url": item.URL, "title": item.Title, "snippet": item.Snippet})
	}
	search, _ := json.Marshal(map[string]any{"type": "item.completed", "item": map[string]any{"type": "web_search", "results": results}})
	body, _ := json.Marshal(map[string]any{"candidates": proposed})
	message, _ := json.Marshal(map[string]any{"type": "item.completed", "item": map[string]string{"type": "agent_message", "text": string(body)}})
	return string(search) + "\n" + string(message) + "\n" + `{"type":"turn.completed","usage":{"input_tokens":100,"output_tokens":20}}`
}

func TestCodexSearchBindsNativeResultMetadata(t *testing.T) {
	for _, native := range []SearchCandidate{
		{URL: "https://public.example/crawl", Title: "Crawl endpoint", Snippet: "Support and API documentation navigation."},
		{URL: "https://another.example/api/plans", Title: "Plan API", Snippet: "Returns tier configuration and quota units."},
		{URL: "https://empty.example/feed", Title: "Feed", Snippet: ""},
	} {
		t.Run(native.URL, func(t *testing.T) {
			proposed := SearchCandidate{URL: native.URL, Title: "Model title", Snippet: "Each request costs 1 credit; this was not in the search metadata."}
			got, err := parseCodexSearch(strings.NewReader(searchMetadataEvents([]SearchCandidate{native}, []SearchCandidate{proposed})), 5)
			if err != nil || len(got.Candidates) != 1 {
				t.Fatalf("valid observed candidate rejected: %+v %v", got, err)
			}
			item := got.Candidates[0]
			if item.URL != native.URL || item.Title != native.Title || item.Snippet != native.Snippet || item.Discovery != "codex_cli_web_search" {
				t.Fatalf("model overwrote native metadata: %+v", item)
			}
		})
	}
}

func TestCodexSearchRejectsUnobservedCandidateURLs(t *testing.T) {
	native := SearchCandidate{URL: "https://public.example/observed", Title: "Observed result"}
	for _, target := range []string{"https://public.example/invented", "https://another.example/unsearched"} {
		t.Run(target, func(t *testing.T) {
			got, err := parseCodexSearch(strings.NewReader(searchMetadataEvents([]SearchCandidate{native}, []SearchCandidate{{URL: target}})), 5)
			if err == nil || len(got.Candidates) != 0 || got.Usage == nil || got.Usage.TotalTokens != 120 {
				t.Fatalf("unobserved URL admitted or real usage lost: %+v %v", got, err)
			}
		})
	}
}

func TestCodexSearchMissingMetadataDoesNotInventProvenance(t *testing.T) {
	raw := `{"type":"item.completed","item":{"type":"web_search"}}
{"type":"item.completed","item":{"type":"agent_message","text":"{\"candidates\":[{\"url\":\"https://public.example/unknown\"}]}"}}
{"type":"turn.completed","usage":{"input_tokens":100,"output_tokens":20}}`
	got, err := parseCodexSearch(strings.NewReader(raw), 5)
	if err == nil || len(got.Candidates) != 0 || got.Usage == nil || got.Usage.TotalTokens != 120 {
		t.Fatalf("missing receipt became proven discovery: %+v %v", got, err)
	}
}

func TestCodexSearchKeepsModelRankingAndCountWithNativeMetadata(t *testing.T) {
	native := []SearchCandidate{{URL: "https://first.example/a", Title: "First", Snippet: "First metadata"}, {URL: "https://second.example/b", Title: "Second", Snippet: "Second metadata"}}
	proposed := []SearchCandidate{native[1], native[0], native[1]}
	got, err := parseCodexSearch(strings.NewReader(searchMetadataEvents(native, proposed)), 1)
	if err != nil || len(got.Candidates) != 1 || got.Candidates[0].URL != native[1].URL || got.Candidates[0].Snippet != native[1].Snippet {
		t.Fatalf("ranking, count or valid receipt changed: %+v %v", got, err)
	}
}

func TestCodexSearchSavedNativeReceiptReplay(t *testing.T) {
	// Captured from the failed development task. This replays the transport
	// boundary only; it neither fetches pages nor credits the task as successful.
	raw, err := readPrivateReplayFixture(t, "codex-search-metadata-20261011.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseCodexSearch(strings.NewReader(string(raw)), 5)
	if err != nil || len(got.Candidates) != 5 || got.Usage == nil || got.Usage.TotalTokens != 62348 {
		t.Fatalf("real receipt cannot be replayed: %+v %v", got, err)
	}
	if !strings.HasPrefix(got.Candidates[0].Snippet, "dark logo]") {
		t.Fatalf("rewritten pricing answer replaced the original navigation snippet: %q", got.Candidates[0].Snippet)
	}
}
