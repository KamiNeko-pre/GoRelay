package aiapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/gin-gonic/gin"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/knowledge"
	"go-lua-crawler/internal/testdb"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func testSpendPolicy(maxTokens int64) *agentdata.SpendBudget {
	rate := int64(1000000)
	return &agentdata.SpendBudget{Policy: agentdata.SpendPolicy{MaxTokens: maxTokens, MaxMicrounits: 1000000, Currency: "fixture", PriceDate: "2026-10-09", InputOverhead: 1024, Models: map[string]agentdata.SpendRate{"fixture": {Input: &rate, Output: &rate}}, SearchPerCall: &rate}}
}

func setRunSpend(t *testing.T, s *Service, r *agentdata.Run, spend *agentdata.SpendBudget) {
	t.Helper()
	var b BudgetSnapshot
	if err := json.Unmarshal([]byte(r.BudgetJSON), &b); err != nil {
		t.Fatal(err)
	}
	b.Spend = spend
	raw, _ := json.Marshal(b)
	if err := s.Data.DB.Model(r).Update("budget_json", string(raw)).Error; err != nil {
		t.Fatal(err)
	}
	r.BudgetJSON = string(raw)
}

func TestSpendAdmissionRejectsCLIWithoutLaunchingIt(t *testing.T) {
	_, err := quoteModel(&CodexProvider{}, ChatRequest{}, testSpendPolicy(100000))
	if !errors.Is(err, agentdata.ErrSpendAdmission) {
		t.Fatal("unbounded CLI falsely treated as bounded", err)
	}
}

func TestSpendAdmissionSearchAndRetrievalPrecedeExternalDispatch(t *testing.T) {
	t.Setenv("GORELAY_EXPERIMENT_UNLIMITED_BUDGET", "0")
	for _, name := range []string{"discover_sources", "search_knowledge"} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(500) }))
			defer remote.Close()
			s := NewService(testdb.Open(t))
			if err := s.Migrate(); err != nil {
				t.Fatal(err)
			}
			s.Search, _ = NewBraveSearch(SearchConfig{Endpoint: remote.URL, APIKey: "fixture"})
			embed, _ := NewModelProvider(ProviderConfig{BaseURL: remote.URL, APIKey: "fixture", Model: "fixture"})
			s.Retriever = &knowledge.Retriever{Store: s.Knowledge, Embed: EmbeddingAdapter{Provider: embed}}
			r, err := s.Create(context.Background(), CreateRunRequest{RequestKey: "tool-spend", Goal: "collect supporting evidence"})
			if err != nil {
				t.Fatal(err)
			}
			setRunSpend(t, s, &r, testSpendPolicy(10))
			if name == "search_knowledge" {
				// This is a legacy RAG admission test, not a route offered to
				// new crawler tasks. Keep its original protocol explicit.
				var b BudgetSnapshot
				json.Unmarshal([]byte(r.BudgetJSON), &b)
				b.LoopVersion, b.PromptPolicy = 4, nil
				r.BudgetJSON = string(mustJSON(b))
				if err := s.Data.DB.Model(&r).Update("budget_json", r.BudgetJSON).Error; err != nil {
					t.Fatal(err)
				}
			}
			if name == "discover_sources" {
				var b BudgetSnapshot
				json.Unmarshal([]byte(r.BudgetJSON), &b)
				b.Spend.Policy.MaxMicrounits = 1
				raw, _ := json.Marshal(b)
				s.Data.DB.Model(&r).Update("budget_json", string(raw))
				r.BudgetJSON = string(raw)
			}
			r, err = s.Data.Claim(context.Background(), r.ID, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.executeDurableTool(context.Background(), r, ToolCall{ID: "guarded", Type: "function", Function: FunctionCall{Name: name, Arguments: `{"query":"public evidence"}`}})
			if !errors.Is(err, agentdata.ErrSpendAdmission) || calls != 0 {
				t.Fatalf("external tool dispatched before admission: %v %d", err, calls)
			}
		})
	}
}

func TestSpendAdmissionModelReplayNeedsNoProviderQuote(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"model":"fixture","choices":[{"message":{"role":"assistant","content":"saved fixture"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`))
	}))
	defer remote.Close()
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	s.Model, _ = NewModelProvider(ProviderConfig{BaseURL: remote.URL, APIKey: "fixture", Model: "fixture"})
	r, err := s.Create(context.Background(), CreateRunRequest{RequestKey: "guarded-replay", Goal: "public evidence"})
	if err != nil {
		t.Fatal(err)
	}
	setRunSpend(t, s, &r, testSpendPolicy(1000000))
	r, err = s.Data.Claim(context.Background(), r.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	state := decisionState()
	state.Run = r
	json.Unmarshal([]byte(r.BudgetJSON), &state.Budget)
	if err = s.think(context.Background(), state, 0, &LoopRound{RoundID: agentdata.Token(), StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	state.Run, err = s.Data.Claim(context.Background(), r.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	json.Unmarshal([]byte(state.Run.BudgetJSON), &state.Budget)
	restored := NewService(s.Data.DB)
	restored.Model = &CodexProvider{} // Cannot launch or quote; committed response must still replay.
	if err = restored.think(context.Background(), state, 0, &LoopRound{RoundID: agentdata.Token(), StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	stored, _ := s.Data.Run(context.Background(), r.ID)
	var b BudgetSnapshot
	json.Unmarshal([]byte(stored.BudgetJSON), &b)
	if b.Spent.ModelCalls != 1 || b.Spend.ReservedTokens != state.Budget.Spend.ReservedTokens {
		t.Fatal("replay charged another request")
	}
}

func TestSpendAdmissionRejectsBeforeNativeModelDispatch(t *testing.T) {
	for _, goal := range []string{"compare public versions", "explain historical changes"} {
		t.Run(goal, func(t *testing.T) {
			calls := 0
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Write([]byte(`{"model":"fixture","choices":[{"message":{"role":"assistant","content":"fixture"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`))
			}))
			defer remote.Close()
			s := NewService(testdb.Open(t))
			if err := s.Migrate(); err != nil {
				t.Fatal(err)
			}
			s.Model, _ = NewModelProvider(ProviderConfig{BaseURL: remote.URL, APIKey: "fixture", Model: "fixture"})
			r, err := s.Create(context.Background(), CreateRunRequest{RequestKey: "spend-preflight", Goal: goal})
			if err != nil {
				t.Fatal(err)
			}
			var b map[string]any
			json.Unmarshal([]byte(r.BudgetJSON), &b)
			b["spend"] = map[string]any{"policy": map[string]any{"max_total_tokens": 10, "max_microunits": 1000000, "currency": "fixture", "price_date": "2026-10-09", "input_overhead_tokens": 1024, "models": map[string]any{"fixture": map[string]any{"input_per_million_microunits": 1000000, "output_per_million_microunits": 1000000}}}}
			raw, _ := json.Marshal(b)
			if err = s.Data.DB.Model(&r).Update("budget_json", string(raw)).Error; err != nil {
				t.Fatal(err)
			}
			_ = s.Advance(context.Background(), r.ID)
			stored, err := s.Data.Run(context.Background(), r.ID)
			if err != nil {
				t.Fatal(err)
			}
			json.Unmarshal([]byte(stored.BudgetJSON), &b)
			if calls != 0 {
				t.Fatalf("token admission happened after a model request: %d", calls)
			}
		})
	}
}

func TestNativeAPICostPolicyGateUsesNormalTaskEntry(t *testing.T) {
	t.Setenv("GORELAY_SPEND_POLICY_PATH", "")
	t.Setenv("GORELAY_ADMIN_TOKEN", "fixture-admin")
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	s.RequireSpendPolicy = true
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterRoutes(router, s)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/agent/runs", bytes.NewBufferString(`{"request_key":"native-no-policy","goal":"collect public evidence"}`))
	request.Header.Set("Authorization", "Bearer fixture-admin")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != 400 || !bytes.Contains(response.Body.Bytes(), []byte("GORELAY_SPEND_POLICY_PATH")) {
		t.Fatalf("native task entered without funded policy: %d %s", response.Code, response.Body.String())
	}
	var count int64
	if err := s.Data.DB.Model(&agentdata.Run{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("unfunded native task was scheduled")
	}
}
