package aiapp

import (
	"context"
	"go-lua-crawler/internal/knowledge"
	"go-lua-crawler/internal/testdb"
	"testing"
)

type capabilityRequestChat struct{ request ChatRequest }

func (p *capabilityRequestChat) Chat(_ context.Context, request ChatRequest) (ChatResponse, error) {
	p.request = request
	return ChatResponse{Message: Message{Role: "assistant", Content: "fixture observation"}, Usage: &Usage{TotalTokens: 1}}, nil
}

type unavailableCapabilitySearch struct{}

func (unavailableCapabilitySearch) Search(context.Context, string, int) (SearchResponse, error) {
	return SearchResponse{}, ErrProviderQuota
}

func TestModelRequestPublishesOnlyConfiguredExternalTools(t *testing.T) {
	for _, tc := range []struct {
		name              string
		search, retrieval bool
	}{
		{"neither", false, false},
		{"search_only", true, false},
		{"retrieval_only", false, true},
		{"both", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewService(testdb.Open(t))
			if err := s.Migrate(); err != nil {
				t.Fatal(err)
			}
			model := &capabilityRequestChat{}
			s.Model = model
			if tc.search {
				s.Search = unavailableCapabilitySearch{}
			}
			if tc.retrieval {
				s.Retriever = &knowledge.Retriever{}
			}
			r, err := s.Create(context.Background(), CreateRunRequest{RequestKey: tc.name, Goal: "collect public facts"})
			if err != nil {
				t.Fatal(err)
			}
			if err = s.Advance(context.Background(), r.ID); err != nil {
				t.Fatal(err)
			}
			names := map[string]bool{}
			for _, tool := range model.request.Tools {
				names[tool.Function.Name] = true
			}
			if names["discover_sources"] != tc.search || names["search_knowledge"] != tc.retrieval {
				t.Fatalf("advertised unavailable provider: search=%v retrieval=%v tools=%v", tc.search, tc.retrieval, names)
			}
			// No vector provider does not disable SQL evidence reads, source reuse,
			// Lua generation/validation or delivery. A configured provider's quota
			// failure also does not masquerade as missing configuration.
			for _, name := range []string{"read_data", "reuse_knowledge", "list_sources", "inspect_source", "validate_program", "register_source", "submit_acquisition", "finish_task"} {
				if !names[name] {
					t.Fatalf("valid capability removed: %s", name)
				}
			}
			for _, name := range []string{"read_evidence", "read_observation", "read_source_content", "update_task_progress"} {
				if names[name] {
					t.Fatal("new protocol still publishes competing read/checkpoint interfaces", name)
				}
			}
		})
	}
}
