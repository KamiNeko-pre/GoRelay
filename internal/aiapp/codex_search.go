package aiapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
)

const searchInstructions = `Search the public web for the supplied query and return up to count relevant candidates as schema-conforming JSON (url, title, and short snippet from search metadata or empty). Execute web search directly; do not fabricate URLs, open full pages, run shell commands, read files, or delegate work.`
const searchSchema = `{"type":"object","properties":{"candidates":{"type":"array","items":{"type":"object","properties":{"url":{"type":"string"},"title":{"type":"string"},"snippet":{"type":"string"}},"required":["url","title","snippet"],"additionalProperties":false}}},"required":["candidates"],"additionalProperties":false}`

type CodexSearch struct{ Transport *CodexProvider }

func (p *CodexSearch) Search(ctx context.Context, query string, count int) (SearchResponse, error) {
	if strings.TrimSpace(query) == "" || count < 1 {
		return SearchResponse{}, fmt.Errorf("search requires a nonempty query and a positive count")
	}
	payload, err := json.Marshal(map[string]any{"query": query, "count": count})
	if err != nil {
		return SearchResponse{}, err
	}
	events, prefix, duration, runErr := p.Transport.invoke(ctx, payload, searchInstructions, searchSchema, true)
	out, parseErr := parseCodexSearch(bytes.NewReader(events), count)
	out.DurationMS, out.Model = duration, "codex-cli/"+p.Transport.Model
	if prefix != "" {
		raw, _ := json.Marshal(out)
		_ = os.WriteFile(prefix+".response.json", raw, 0600)
	}
	return out, codexResultError(parseErr, runErr)
}

func parseCodexSearch(reader io.Reader, count int) (SearchResponse, error) {
	var captured bytes.Buffer
	turn, text, searches, err := parseCodexTurn(io.TeeReader(reader, &captured), true)
	out := SearchResponse{Candidates: []SearchCandidate{}, RequestID: turn.RequestID, Usage: turn.Usage}
	if err != nil {
		return out, err
	}
	if searches == 0 {
		return out, fmt.Errorf("search transport did not execute native web search")
	}
	var body struct {
		Candidates []SearchCandidate `json:"candidates"`
	}
	if err := json.Unmarshal([]byte(text), &body); err != nil {
		return out, fmt.Errorf("invalid structured search results: %w", err)
	}
	// The model proposes a ranking, not discovery provenance. Bind selected
	// URLs to completed native search receipts; its rewritten text is untrusted.
	native := map[string]SearchCandidate{}
	decoder := json.NewDecoder(bytes.NewReader(captured.Bytes()))
	for {
		var event struct {
			Type string `json:"type"`
			Item struct {
				Type    string            `json:"type"`
				Results []SearchCandidate `json:"results"`
			} `json:"item"`
		}
		if err := decoder.Decode(&event); err == io.EOF {
			break
		} else if err != nil {
			return out, fmt.Errorf("invalid native search metadata: %w", err)
		}
		if event.Type != "item.completed" || event.Item.Type != "web_search" {
			continue
		}
		for _, result := range event.Item.Results {
			if _, exists := native[result.URL]; !exists {
				native[result.URL] = result
			}
		}
	}
	seen := map[string]bool{}
	candidates := []SearchCandidate{}
	for _, c := range body.Candidates {
		u, err := url.Parse(c.URL)
		if err != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
			return out, fmt.Errorf("search candidate must have an absolute HTTP(S) URL")
		}
		observed, exists := native[c.URL]
		if !exists {
			return out, fmt.Errorf("search candidate has no native result metadata: %s", c.URL)
		}
		if seen[c.URL] || len(candidates) >= count {
			continue
		}
		seen[c.URL] = true
		candidates = append(candidates, SearchCandidate{URL: observed.URL, Title: observed.Title, Snippet: observed.Snippet, Discovery: "codex_cli_web_search"})
	}
	out.Candidates = candidates
	return out, nil
}
