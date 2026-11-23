// mcp-check verifies the 6-tool GoRelay v2 Streamable HTTP MCP interface.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type authenticatedTransport struct{ token string }

func (t authenticatedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	cp := r.Clone(r.Context())
	if t.token != "" {
		cp.Header.Set("Authorization", "Bearer "+t.token)
	}
	return http.DefaultTransport.RoundTrip(cp)
}

func main() {
	endpoint := flag.String("url", "http://127.0.0.1:8080/mcp", "Public GoRelay v2 MCP endpoint")
	sourceIDs := flag.String("sources", "", "Optional comma-separated source_ids for evidence search")
	query := flag.String("query", "", "Optional BM25 evidence query")
	flag.Parse()
	if err := check(*endpoint, *sourceIDs, *query); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func check(endpoint, rawSources, query string) error {
	token := os.Getenv("GORELAY_ADMIN_TOKEN")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client := mcp.NewClient(&mcp.Implementation{Name: "gorelay-mcp-check", Version: "2.0.0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   endpoint,
		HTTPClient: &http.Client{Transport: authenticatedTransport{token}},
	}, nil)
	if err != nil {
		return fmt.Errorf("MCP connection: %w", err)
	}
	defer session.Close()

	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		return fmt.Errorf("tool discovery: %w", err)
	}
	fmt.Printf("MCP v2 connected; %d tools visible:\n", len(listed.Tools))
	for _, tool := range listed.Tools {
		fmt.Println("- " + tool.Name)
	}

	listRes, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "gorelay_list_sources",
		Arguments: map[string]any{},
	})
	if err != nil || listRes.IsError {
		return fmt.Errorf("gorelay_list_sources failed: err=%v res=%+v", err, listRes)
	}
	pretty, _ := json.MarshalIndent(listRes.StructuredContent, "", "  ")
	fmt.Println(string(pretty))

	if query != "" && strings.TrimSpace(rawSources) != "" {
		var ids []string
		for _, part := range strings.Split(rawSources, ",") {
			if s := strings.TrimSpace(part); s != "" {
				ids = append(ids, s)
			}
		}
		evRes, err := session.CallTool(ctx, &mcp.CallToolParams{
			Name:      "gorelay_search_evidence",
			Arguments: map[string]any{"source_ids": ids, "query": query},
		})
		if err != nil {
			return fmt.Errorf("search evidence: %w", err)
		}
		evData, _ := json.MarshalIndent(evRes.StructuredContent, "", "  ")
		fmt.Println(string(evData))
		if evRes.IsError {
			return fmt.Errorf("gorelay_search_evidence returned tool error")
		}
	}
	return nil
}
