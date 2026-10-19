package aiapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go-lua-crawler/internal/agentdata"
)

func TestCrawlerRawAttributesReachModelWithSafeNavigation(t *testing.T) {
	for _, href := range []string{"../../files/notice.pdf?edition=final&lang=zh", "./export.csv?compressed=0"} {
		t.Run(href, func(t *testing.T) {
			body := `<main><ul><li><a data-kind="resource" href="` + strings.ReplaceAll(href, "&", "&amp;") + `">Resource</a></li></ul><a href="javascript:untrusted()" onclick="untrusted()">No navigation</a></main>`
			s, run, _ := storedHTMLReader(t, body)
			args, _ := json.Marshal(map[string]any{"handle": "source", "view": "dom", "selector": "a"})
			call := ToolCall{ID: "raw-attributes", Type: "function", Function: FunctionCall{Name: "read_data", Arguments: string(args)}}
			out, err := s.readData(context.Background(), run, call)
			if err != nil {
				t.Fatal(err)
			}
			out = modelObservationForRun(run, out, nil)
			raw, _ := json.Marshal(out)
			var captured ChatRequest
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
					t.Error(err)
				}
				json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": Message{Role: "assistant"}, "finish_reason": "stop"}}})
			}))
			defer server.Close()
			provider, err := NewModelProvider(ProviderConfig{BaseURL: server.URL, Model: "fixture", APIKey: "fixture"})
			if err != nil {
				t.Fatal(err)
			}
			_, err = provider.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "system", Content: runSystemPrompt(run)}, {Role: "assistant", ToolCalls: []ToolCall{call}}, {Role: "tool", ToolCallID: call.ID, Content: string(raw)}}})
			if err != nil {
				t.Fatal(err)
			}
			var visible struct {
				Data struct {
					Nodes []struct {
						Attributes    map[string]string `json:"attributes"`
						RawAttributes map[string]string `json:"raw_attributes"`
					} `json:"nodes"`
				} `json:"data"`
			}
			if err := json.Unmarshal([]byte(captured.Messages[2].Content), &visible); err != nil {
				t.Fatal(err)
			}
			if len(visible.Data.Nodes) != 2 || visible.Data.Nodes[0].RawAttributes["href"] != href || visible.Data.Nodes[0].RawAttributes["data-kind"] != "resource" {
				t.Fatalf("raw crawler facts lost at model HTTP boundary: %s", captured.Messages[2].Content)
			}
			if visible.Data.Nodes[0].Attributes["href"] != resolveAgainst("https://fixture.example/docs", href) || visible.Data.Nodes[1].Attributes["href"] != "" || visible.Data.Nodes[1].RawAttributes["onclick"] != "" {
				t.Fatal("navigation safety or executable-attribute boundary changed", visible.Data.Nodes)
			}
			if !strings.Contains(captured.Messages[0].Content, "raw_attributes") || !strings.Contains(captured.Messages[0].Content, "unresolved") {
				t.Fatal("model still has to guess attribute semantics")
			}
			rows := boundedUnifiedRead(t, s, run, map[string]any{"handle": "source", "view": "dom", "selector": "li"})
			nested := rows["nodes"].([]any)[0].(map[string]any)["links"].([]any)[0].(map[string]any)
			if nested["raw_href"] != href || nested["href"] != resolveAgainst("https://fixture.example/docs", href) {
				t.Fatal("nested link lost original or navigation identity", nested)
			}
		})
	}
}

func TestProbeDeclarationsUseExecutionBudgetInsteadOfFieldCount(t *testing.T) {
	runner := actualRunnerForChain(t)
	for _, count := range []int{19, 33} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			s, run, _ := storedHTMLReader(t, `<h2>Observed resource</h2>`)
			s.Runner = runner
			run.SpecJSON = `{"goal":"collect fields","contract_version":"v2","schema_version":"v1","required_fields":[{"name":"value","type":"string"}],"join_keys":["value"]}`
			fields := []string{}
			for n := 1; n <= count; n++ {
				fields = append(fields, fmt.Sprintf("field_%d", n))
			}
			args, _ := json.Marshal(trialArgs{Mode: "probe", Kind: "composition", SourceID: "probe-width", Purpose: "inspect declared source attributes", URL: "https://fixture.example/docs", ProducedFields: fields, Code: fmt.Sprintf(`local row={}; for i=1,%d do row["field_"..i]="value_"..i end; return true,{items={row}}`, count)})
			out, err := s.trial(context.Background(), run, ToolCall{ID: "probe-wide", Function: FunctionCall{Name: "validate_program", Arguments: string(args)}})
			if err != nil {
				t.Fatal("budget-admitted structured probe rejected", count, err)
			}
			var result struct {
				Items            []map[string]any `json:"items"`
				IntermediateOnly bool             `json:"intermediate_only"`
			}
			if err := json.Unmarshal(out.Data, &result); err != nil || len(result.Items) != 1 || len(result.Items[0]) != count || !result.IntermediateOnly {
				t.Fatal("probe observations lost or became delivery", string(out.Data), err)
			}
		})
	}
}

func TestBudgetManagedProbeDoesNotAdvertiseLegacyCountLimit(t *testing.T) {
	s, run, _ := storedHTMLReader(t, `<h2>Observed resource</h2>`)
	s.Runner = actualRunnerForChain(t)
	run.SpecJSON = `{"goal":"collect fields","contract_version":"v2","schema_version":"v1","required_fields":[{"name":"value","type":"string"}],"join_keys":["value"]}`
	for n := 0; n < probeAttemptLimit+1; n++ {
		raw, _ := json.Marshal(ToolResult{Name: "validate_program", CallID: fmt.Sprint(n), Data: json.RawMessage(`{"mode":"probe"}`)})
		if err := s.Data.DB.Create(&agentdata.Operation{RunID: run.ID, Key: fmt.Sprintf("tool:probe-%d", n), Kind: "validate_program", Stage: "succeeded", ResultJSON: string(raw)}).Error; err != nil {
			t.Fatal(err)
		}
	}
	args, _ := json.Marshal(trialArgs{Mode: "probe", Kind: "composition", SourceID: "probe-budget", Purpose: "inspect resource metadata", URL: "https://fixture.example/docs", ProducedFields: []string{"observed"}, Code: `return true,{items={{observed="new structural observation"}}}`})
	out, err := s.trial(context.Background(), run, ToolCall{ID: "new-probe", Function: FunctionCall{Name: "validate_program", Arguments: string(args)}})
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	json.Unmarshal(out.Data, &data)
	if _, exists := data["probes_remaining"]; exists || strings.Contains(string(out.Data), "budget is spent") || data["next_action"] != nil {
		t.Fatal("healthy admitted probe told to stop by an unenforced legacy counter", string(out.Data))
	}
}

func TestInvalidProbeDeclarationGivesAnActualTypedRepairOpportunity(t *testing.T) {
	for _, fields := range [][]string{nil, {"href", "href"}, {strings.Repeat("x", 65)}} {
		s := efficiencyState()
		s.Budget.Unlimited = true
		err := probeFieldGuard(TaskSpec{}, fields)
		var invalid *ArgumentDecodeError
		if !errors.As(err, &invalid) || !strings.HasPrefix(invalid.Field, "produced_fields") {
			t.Fatalf("invalid declaration lacks exact argument correction: %+v %v", fields, err)
		}
		call := ToolCall{ID: "invalid-probe-declaration", Function: FunctionCall{Name: "validate_program", Arguments: `{"mode":"probe","code":"return true,{items={}}","kind":"composition"}`}}
		model := modelRound("probe-diagnosis", call)
		model.ID, model.RunID = 10, s.Run.ID
		var response ChatResponse
		json.Unmarshal([]byte(model.ResultJSON), &response)
		response.DeliveryRecoveryPhase = "correcting"
		raw, _ := json.Marshal(response)
		model.ResultJSON = string(raw)
		failure := toolFailureReceipt(call, ToolResult{}, err)
		raw, _ = json.Marshal(failure)
		failed := agentdata.Operation{ID: 11, RunID: s.Run.ID, Key: "tool:" + call.ID, Kind: "validate_program", Stage: "failed", ResultJSON: string(raw)}
		s.Operations = []agentdata.Operation{model, failed}
		prior := DeliveryRecovery{Phase: "correcting", LastModel: 8, IdleModels: deliveryIdleRoundLimit - 1, Baseline: deliveryInformationSignature(s), ReadBaseline: agentdata.Hash(consumedReadRanges(s))}
		host := HostProgress{NoProgressModelRounds: 7}
		got := advanceDeliveryRecovery(prior, s, host, false, "")
		if got.Phase != "correcting" || got.IdleModels != prior.IdleModels {
			t.Fatal("first concrete probe diagnosis lost its repair opportunity", got, failure)
		}
		call.ID = "same-defect-another-call"
		response.Message.ToolCalls = []ToolCall{call}
		raw, _ = json.Marshal(response)
		model.ID, model.ResultJSON = 12, string(raw)
		failure.CallID = call.ID
		raw, _ = json.Marshal(failure)
		failed.ID, failed.Key, failed.ResultJSON = 13, "tool:"+call.ID, string(raw)
		s.Operations = append(s.Operations, model, failed)
		got = advanceDeliveryRecovery(got, s, host, false, "")
		if got.Phase != "correcting" || got.NoEvidenceRounds != 1 {
			t.Fatal("first repeated probe defect was counted as two rounds", got)
		}
		call.ID = "same-defect-third-call"
		response.Message.ToolCalls = []ToolCall{call}
		model.ID, model.ResultJSON = 14, string(mustJSON(response))
		failure.CallID = call.ID
		failed.ID, failed.Key, failed.ResultJSON = 15, "tool:"+call.ID, string(mustJSON(failure))
		s.Operations = append(s.Operations, model, failed)
		if repeated := advanceDeliveryRecovery(got, s, host, false, ""); repeated.Phase != "concluding" || repeated.NoEvidenceRounds != 2 {
			t.Fatal("renaming the same probe defect survived two evidence-free rounds", repeated)
		}
		s.Operations = s.Operations[:2]
		s.Budget.WorkDeadline = time.Now().Add(-time.Second)
		if expired := advanceDeliveryRecovery(prior, s, host, false, ""); expired.Phase != "concluding" {
			t.Fatal("typed diagnosis extended a deadline", expired)
		}
	}
}
