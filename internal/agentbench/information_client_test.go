package agentbench

import (
	"context"
	"encoding/json"
	"go-lua-crawler/internal/information"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestInformationBenchmarkUsesApplicationEntryAndRefreshOnlyReads(t *testing.T) {
	posts, gets := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture" {
			t.Error("missing API authentication")
		}
		if r.URL.Path == "/api/v1/information/status" {
			w.Write([]byte(`{"experiment_policy":{"action_mode":"code","disable_capability_reuse":false,"disable_knowledge_reuse":false}}`))
			return
		}
		if r.URL.Path == "/api/v1/information/requests" && r.Method == "POST" {
			posts++
			var input map[string]any
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
			}
			if input["goal"] != "collect observed values" || input["timeout_seconds"] != float64(420) || len(input) != 5 {
				t.Errorf("wrong public request or gold leaked: %+v", input)
			}
			w.WriteHeader(202)
			w.Write([]byte(`{"request":{"id":"information-request","experiment":{"action_mode":"code","disable_capability_reuse":false,"disable_knowledge_reuse":false}}}`))
			return
		}
		if r.Method != "GET" || r.URL.Path != "/api/v1/information/requests/information-request" {
			t.Errorf("unexpected call: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(400)
			return
		}
		gets++
		w.Write([]byte(`{"request":{"id":"information-request","stage":"completed"},"result":{"records":[]},"accounting":{"request_id":"information-request","experiment":{"action_mode":"code","disable_capability_reuse":false,"disable_knowledge_reuse":false},"model_calls":[{"id":"route","purpose":"choose_route","usage":{"prompt_tokens":100,"total_tokens":100}},{"id":"answer","purpose":"answer_information","usage":{"prompt_tokens":50,"completion_tokens":10,"total_tokens":60}}],"search_calls":[],"child_operations":[],"run_accounting":{"index_settled":true}}}`))
	}))
	defer server.Close()
	seconds := int64(420)
	m := Manifest{EntryPoint: "information", Status: "frozen", EnvironmentID: "new-fixture", Cases: []Case{{ID: "native", Goal: "collect observed values", Split: "development", SourceScope: []string{"source.example"}, EntryURLs: []string{"https://source.example"}, TimeoutSeconds: &seconds}}}
	m.ExpectedInformationPolicy = &information.ExperimentPolicy{ActionMode: "code"}
	cfg := ClientConfig{Endpoint: server.URL, Token: "fixture", Batch: "info", Variant: "full", Repetitions: 1, Timeout: time.Second}
	rows, err := Execute(context.Background(), cfg, m, map[string]GoldCase{"native": {Verification: "assistant_prepared", Rows: []map[string]any{{"private_gold": "never send"}}}}, nil)
	if err != nil || len(rows) != 1 || rows[0].RequestID != "information-request" || rows[0].RunID != "" || rows[0].InformationAccounting == nil || rows[0].Correct == nil || *rows[0].Correct {
		t.Fatalf("native request/scoring lost: %+v %v", rows, err)
	}
	if got := Summarize(rows)[0]; got.ModelAttempts != 2 || got.LLMReportedTokens != 160 {
		t.Fatalf("upper usage omitted %+v", got)
	}
	rows, err = RefreshAccounting(context.Background(), cfg, rows)
	if err != nil || posts != 1 || gets != 2 || rows[0].Status != "completed" || rows[0].Correct == nil || *rows[0].Correct {
		t.Fatalf("refresh reran task or changed judgment %+v %v", rows, err)
	}
	m.ExpectedInformationPolicy.DisableCapabilityReuse = true
	if _, err = Execute(context.Background(), cfg, m, nil, nil); err == nil || posts != 1 {
		t.Fatal("mismatched server policy admitted a paid experiment")
	}
}

func TestMissingInformationLedgerCannotClaimZeroTokensPerCorrectTask(t *testing.T) {
	good := true
	s := Summarize([]Trial{{RequestID: "request", CaseID: "case", Status: "completed", Correct: &good}})[0]
	if s.TokensPerCorrectTask != nil || s.MissingRequestAccountingRuns != 1 {
		t.Fatalf("missing ledger treated as free: %+v", s)
	}
}
