package aiapp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHTTPReadinessRequiresTheAgentToolProtocol(t *testing.T) {
	for _, tc := range []struct {
		name, message string
		ready         bool
	}{
		{"tool_only", `{"role":"assistant","content":null,"tool_calls":[{"id":"check-1","type":"function","function":{"name":"gorelay_readiness","arguments":"{\"ready\":true}"}}]}`, true},
		{"text_with_tool", `{"role":"assistant","content":"Checking","tool_calls":[{"id":"check-2","type":"function","function":{"name":"gorelay_readiness","arguments":"{\"ready\":true}"}}]}`, true},
		{"text_only", `{"role":"assistant","content":"ready"}`, false},
		{"wrong_function", `{"role":"assistant","tool_calls":[{"id":"check-3","type":"function","function":{"name":"execute_shell","arguments":"{\"ready\":true}"}}]}`, false},
		{"wrong_arguments", `{"role":"assistant","tool_calls":[{"id":"check-4","type":"function","function":{"name":"gorelay_readiness","arguments":"{\"ready\":false}"}}]}`, false},
		{"unknown_argument", `{"role":"assistant","tool_calls":[{"id":"check-5","type":"function","function":{"name":"gorelay_readiness","arguments":"{\"ready\":true,\"command\":\"run\"}"}}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"choices":[{"message":%s,"finish_reason":"tool_calls"}]}`, tc.message)
			}))
			defer srv.Close()
			p, err := NewModelProvider(ProviderConfig{BaseURL: srv.URL, APIKey: "fixture", Model: "fixture", Timeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			r, err := p.Preflight(context.Background())
			if r.Ready != tc.ready || (err == nil) != tc.ready {
				t.Fatalf("ready=%v error=%v, want %v", r.Ready, err, tc.ready)
			}
		})
	}
}
