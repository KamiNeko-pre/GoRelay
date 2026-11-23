// mcp-check exercises the public MCP protocol without a chat model or acquisition.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type authenticatedTransport struct{ token string }

func (t authenticatedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	copy := r.Clone(r.Context())
	copy.Header.Set("Authorization", "Bearer "+t.token)
	return http.DefaultTransport.RoundTrip(copy)
}

func main() {
	endpoint := flag.String("url", "http://127.0.0.1:18109/mcp", "Public GoRelay MCP endpoint")
	query := flag.String("query", "", "Optional saved-evidence query; never starts collection")
	flag.Parse()
	if err := check(*endpoint, *query); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func check(endpoint, query string) error {
	token := os.Getenv("GORELAY_ADMIN_TOKEN")
	if token == "" {
		return fmt.Errorf("GORELAY_ADMIN_TOKEN is required; use the existing GoRelay service token, not a model API key")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "gorelay-mcp-check", Version: "0.1.0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: &http.Client{Transport: authenticatedTransport{token}}}, nil)
	if err != nil {
		return fmt.Errorf("MCP connection: %w", err)
	}
	defer session.Close()
	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		return fmt.Errorf("tool discovery: %w", err)
	}
	fmt.Printf("MCP connected; %d tools visible\n", len(listed.Tools))
	for _, tool := range listed.Tools {
		fmt.Println("- " + tool.Name)
	}
	if query == "" {
		return nil
	}
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "gorelay_search_evidence", Arguments: map[string]any{"query": query}})
	if err != nil {
		return fmt.Errorf("saved-evidence call: %w", err)
	}
	data, err := json.MarshalIndent(result.StructuredContent, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	if result.IsError {
		return fmt.Errorf("saved-evidence tool returned the diagnosis above")
	}
	return nil
}
