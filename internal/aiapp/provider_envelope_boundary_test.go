package aiapp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProviderEnvelopeRejectsWrongRoleDuplicateIDsAndTruncation(t *testing.T) {
	for _, tc := range []struct {
		message, finish string
		valid           bool
	}{
		{`{"role":"tool","content":"not a model decision"}`, "stop", false},
		{`{"role":"assistant","tool_calls":[{"id":"same","type":"function","function":{"name":"list_sources","arguments":"{}"}},{"id":"same","type":"function","function":{"name":"list_sources","arguments":"{}"}}]}`, "tool_calls", false},
		{`{"role":"assistant","tool_calls":[{"id":"unfinished","type":"function","function":{"name":"list_sources","arguments":"{"}}]}`, "length", false},
		{`{"role":"assistant","tool_calls":[{"id":"one","type":"function","function":{"name":"list_sources","arguments":"{}"}}]}`, "tool_calls", true},
	} {
		remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = fmt.Fprintf(w, `{"choices":[{"message":%s,"finish_reason":%q}],"usage":{"total_tokens":10}}`, tc.message, tc.finish)
		}))
		provider, err := NewModelProvider(ProviderConfig{BaseURL: remote.URL, APIKey: "fixture", Model: "fixture"})
		if err != nil {
			t.Fatal(err)
		}
		out, err := provider.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "fixture"}}})
		remote.Close()
		if tc.valid && err != nil || !tc.valid && !errors.Is(err, ErrInvalidProposal) || out.Usage == nil || out.Usage.TotalTokens != 10 {
			t.Fatal("proposal classification or reported usage lost", tc, out, err)
		}
	}
}
