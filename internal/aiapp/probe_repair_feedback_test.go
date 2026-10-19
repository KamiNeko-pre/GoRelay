package aiapp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func probeFeedbackProxy(t *testing.T, body string) {
	t.Helper()
	probeFeedbackSource(t, func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, body) })
}

func probeFeedbackSource(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	source := httptest.NewServer(handler)
	t.Cleanup(source.Close)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "CONNECT" || r.Host != "8.8.8.8:80" {
			w.WriteHeader(400)
			return
		}
		upstream, err := net.DialTimeout("tcp", strings.TrimPrefix(source.URL, "http://"), time.Second)
		if err != nil {
			w.WriteHeader(502)
			return
		}
		defer upstream.Close()
		conn, buffer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		buffer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		buffer.Flush()
		go func() { io.Copy(upstream, buffer) }()
		io.Copy(conn, upstream)
	}))
	t.Cleanup(proxy.Close)
	t.Setenv("GORELAY_ACQUISITION_PROXY", proxy.URL)
}

func TestProbeMissingFieldsExposeActualSourceStructureOnModelHTTP(t *testing.T) {
	for _, tc := range []struct{ name, body, extract, observed string }{
		{"definition-list", `<main><h2>Observed title</h2><dl><dt>Owner</dt><dd data-field="owner">Public bureau</dd></dl><p>` + strings.Repeat("Long prose stays outside crawler feedback. ", 30) + `</p></main>`, `html_find(body,"h2")`, "dd"},
		{"nested-json", `{"dataset":{"title":"Observed title","organization":{"name":"Public bureau"}},"description":"` + strings.Repeat("Long prose stays outside crawler feedback. ", 30) + `"}`, `json_decode(body).dataset.title`, "organization"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probeFeedbackProxy(t, tc.body)
			s, run, _ := storedHTMLReader(t, tc.body)
			s.Runner = actualRunnerForChain(t)
			run.SpecJSON = `{"goal":"collect title and owner","contract_version":"v2","schema_version":"v1","required_fields":[{"name":"title","type":"string"},{"name":"owner","type":"string"},{"name":"source_url","type":"string"}],"join_keys":["source_url"],"require_published_capability":true}`
			args, _ := json.Marshal(trialArgs{Mode: "probe", Kind: "composition", SourceID: "feedback", Purpose: "test missing detail field", URL: "http://8.8.8.8/detail", ProducedFields: []string{"title", "owner", "source_url"}, Code: `local body=assert(http_get(TARGET_URL)); return true,{items={{title=` + tc.extract + `,source_url=TARGET_URL}}}`})
			call := ToolCall{ID: "missing-owner", Type: "function", Function: FunctionCall{Name: "validate_program", Arguments: string(args)}}
			out, err := s.trial(context.Background(), run, call)
			if err != nil {
				t.Fatal(err)
			}
			visible := modelObservationForRun(run, out, nil)
			var captured ChatRequest
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				json.NewDecoder(r.Body).Decode(&captured)
				json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": Message{Role: "assistant"}, "finish_reason": "stop"}}})
			}))
			defer endpoint.Close()
			provider, _ := NewModelProvider(ProviderConfig{BaseURL: endpoint.URL, Model: "fixture", APIKey: "fixture"})
			_, err = provider.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "assistant", ToolCalls: []ToolCall{call}}, {Role: "tool", ToolCallID: call.ID, Content: string(mustJSON(visible))}}})
			if err != nil {
				t.Fatal(err)
			}
			var receipt struct {
				Data struct {
					Diagnostics []struct {
						Missing   []string        `json:"missing_fields"`
						Verified  bool            `json:"source_verified"`
						Structure json.RawMessage `json:"source_structure"`
					} `json:"field_diagnostics"`
					Next        map[string]any `json:"next_action"`
					Deliverable bool           `json:"delivery_candidate"`
				} `json:"data"`
			}
			if len(captured.Messages) != 2 {
				t.Fatal("missing actual model HTTP request")
			}
			json.Unmarshal([]byte(captured.Messages[1].Content), &receipt)
			if len(receipt.Data.Diagnostics) != 1 || !receipt.Data.Diagnostics[0].Verified || strings.Join(receipt.Data.Diagnostics[0].Missing, ",") != "owner" || !strings.Contains(string(receipt.Data.Diagnostics[0].Structure), tc.observed) {
				t.Fatal("successful partial probe gives no concrete source-bound repair", captured.Messages[1].Content)
			}
			changes, _ := receipt.Data.Next["argument_changes"].(map[string]any)
			if receipt.Data.Deliverable || changes["mode"] != "acquisition" || changes["kind"] != "adapter" || !strings.Contains(fmt.Sprint(receipt.Data.Next["instruction"]), "_evidence") {
				t.Fatal("probe lacks the ordered path from field repair to real delivery", receipt.Data.Next)
			}
			if strings.Contains(captured.Messages[1].Content, "Long prose stays outside") || strings.Contains(captured.Messages[1].Content, "source_evidence") {
				t.Fatal("repair exposed prose/raw source rather than selectors")
			}
			// Its source structure is useful correction evidence, never delivery.
			state, recovery := evidenceRecoveryState()
			json.Unmarshal([]byte(run.SpecJSON), &state.Spec)
			recovery.Baseline = deliveryInformationSignature(state)
			var data any
			json.Unmarshal(out.Data, &data)
			id := appendEvidenceRound(&state, "correcting", call.Function.Name, call.Function.Arguments, data)
			got := advanceDeliveryRecovery(recovery, state, HostProgress{}, true, "round-1")
			if got.Phase != "correcting" || recoveryCounter(t, got, "no_evidence_rounds") != 0 || got.Baseline != deliveryInformationSignature(state) || got.LastModel != id {
				t.Fatal("partial source-bound probe was discarded or promoted to delivery", got)
			}
			state.HostProgress.Recovery = got
			working := pendingProgramCorrections(state)
			if len(working) != 1 || !strings.Contains(working[0].Instruction, "acquisition") || !strings.Contains(working[0].Instruction, "field_diagnostics") {
				t.Fatal("restored successful probe was treated as a publishable candidate", working)
			}
		})
	}
}

func TestProbeMissingLastRowIsNotHiddenByPreview(t *testing.T) {
	s, run, _ := storedHTMLReader(t, `<h2>Preview boundary</h2>`)
	s.Runner = actualRunnerForChain(t)
	run.SpecJSON = `{"goal":"collect title","schema_version":"v1","required_fields":[{"name":"title","type":"string"}],"join_keys":["title"]}`
	args, _ := json.Marshal(trialArgs{Mode: "probe", Kind: "composition", SourceID: "preview", Purpose: "test all rows", URL: "https://fixture.example/docs", ProducedFields: []string{"title"}, Code: `local rows={};for i=1,21 do rows[i]={title="title"..i} end;rows[21]={source_url=TARGET_URL};return true,{items=rows}`})
	out, err := s.trial(context.Background(), run, ToolCall{ID: "last-row", Function: FunctionCall{Name: "validate_program", Arguments: string(args)}})
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	json.Unmarshal(out.Data, &data)
	if data["delivery_candidate"] != false {
		t.Fatal("twenty-row preview incorrectly certifies the entire probe", string(out.Data))
	}
	// An arbitrary program URL/output cannot produce host source evidence.
	state, recovery := evidenceRecoveryState()
	appendEvidenceRound(&state, "correcting", "validate_program", string(args), data)
	recovery = advanceDeliveryRecovery(recovery, state, HostProgress{}, false, "")
	if recoveryCounter(t, recovery, "no_evidence_rounds") != 1 {
		t.Fatal("unfetched model output manufactured evidence", recovery)
	}
}
