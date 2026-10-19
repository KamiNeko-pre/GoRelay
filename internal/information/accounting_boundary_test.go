package information

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/knowledge"
	"go-lua-crawler/internal/task"
)

func TestRequestAccountingIncludesUpperChildAndNativeWorkWithoutReexecution(t *testing.T) {
	s, _ := retrievalService(t)
	ctx := context.Background()
	child, err := s.Crawler.Create(ctx, aiapp.CreateRunRequest{Goal: "collect public facts", RequestKey: "accounting-child"})
	if err != nil {
		t.Fatal(err)
	}
	r := Request{ID: agentdata.Token(), RequestKey: "accounting-request", Stage: "completed", ChildRunID: child.ID, InputJSON: "{}"}
	if err = s.DB.Create(&r).Error; err != nil {
		t.Fatal(err)
	}
	for i, purpose := range []string{"choose_route", "answer_information", "failed_route", "unknown_route"} {
		usage := &aiapp.Usage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120, PromptTokensDetail: &aiapp.PromptDetails{CachedTokens: 30}}
		stage := "succeeded"
		if i == 2 {
			stage = "failed"
		}
		if i == 3 {
			usage = nil
			stage = "reserved"
		}
		input := aiapp.ChatRequest{Messages: []aiapp.Message{{Role: "user", Content: purpose}}, Tools: []aiapp.ToolDefinition{{Type: "function", Function: aiapp.FunctionDefinition{Name: purpose, Parameters: json.RawMessage(`{"type":"object"}`)}}}}
		step := Step{RequestID: r.ID, Key: purpose, Kind: "route_model", Stage: stage, InputHash: purpose, InputJSON: string(mustMarshal(input)), ResultJSON: string(mustMarshal(aiapp.ChatResponse{Model: "reported-model", Usage: usage}))}
		if err = s.DB.Create(&step).Error; err != nil {
			t.Fatal(err)
		}
	}
	op := agentdata.Operation{RunID: child.ID, Key: "child-model", Kind: "model", Stage: "succeeded", UsageJSON: `{"prompt_tokens":50,"completion_tokens":10,"total_tokens":60}`, ResultJSON: `{"model":"child-model"}`}
	if err = s.DB.Create(&op).Error; err != nil {
		t.Fatal(err)
	}
	native := task.CrawlTask{RequestKey: "information-" + r.ID + "-77", Target: "information-api", URL: "https://example.test", Status: task.StatusSucceeded}
	if err = s.DB.Create(&native).Error; err != nil {
		t.Fatal(err)
	}
	work := knowledge.IndexWork{DocID: "native-doc", Epoch: 1, Generation: "native-generation", Fingerprint: "test", OriginTaskID: native.ID, Status: "pending"}
	if err = s.DB.Create(&work).Error; err != nil {
		t.Fatal(err)
	}
	attempt := knowledge.IndexAttempt{TaskID: native.ID, WorkID: work.ID, Attempt: 1, StartedAt: time.Now(), MetricsJSON: `{"embedding_calls":1,"usage":{"prompt_tokens":40,"total_tokens":40}}`, Status: "failed"}
	if err = s.DB.Create(&attempt).Error; err != nil {
		t.Fatal(err)
	}
	for reads := 0; reads < 2; reads++ {
		got, err := s.Accounting(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.ModelCalls) != 5 || got.RunAccounting == nil || got.RunAccounting.IndexSettled || len(got.RunAccounting.IndexWork) != 1 || len(got.RunAccounting.IndexAttempts) != 1 {
			t.Fatalf("incomplete accounting: %+v", got)
		}
		var reported int64
		unknown := 0
		seen := map[string]bool{}
		for _, call := range got.ModelCalls {
			if seen[call.ID] || call.ParentID == "" {
				t.Fatalf("invalid ownership: %+v", call)
			}
			seen[call.ID] = true
			if call.Usage == nil {
				unknown++
			} else {
				reported += call.Usage.TotalTokens
			}
			if call.Scope == "information" && (call.PromptHash == "" || call.ToolSchemaHash == "") {
				t.Fatal("missing actual request hashes")
			}
		}
		if reported != 420 || unknown != 1 {
			t.Fatalf("reported=%d unknown=%d", reported, unknown)
		}
	}
	gin.SetMode(gin.TestMode)
	t.Setenv("GORELAY_ADMIN_TOKEN", "accounting-token")
	router := gin.New()
	RegisterRoutes(router, s)
	req := httptest.NewRequest("GET", "/api/v1/information/requests/"+r.ID, nil)
	req.Header.Set("Authorization", "Bearer accounting-token")
	out := httptest.NewRecorder()
	router.ServeHTTP(out, req)
	if out.Code != 200 {
		t.Fatalf("GET %d: %s", out.Code, out.Body.String())
	}
	var view struct{ Accounting *RequestAccounting }
	if err = json.Unmarshal(out.Body.Bytes(), &view); err != nil || view.Accounting == nil || len(view.Accounting.ModelCalls) != 5 {
		t.Fatalf("missing HTTP accounting: %v %s", err, out.Body.String())
	}
	var count int64
	s.DB.Model(&Step{}).Where("request_id = ?", r.ID).Count(&count)
	if count != 4 {
		t.Fatalf("read changed execution ledger: %d", count)
	}
}

func TestRequestAccountingNativeOnlyAndMalformedUsageRemainExplicit(t *testing.T) {
	s, _ := retrievalService(t)
	r := Request{ID: agentdata.Token(), RequestKey: "native-accounting", Stage: "completed", InputJSON: "{}"}
	if err := s.DB.Create(&r).Error; err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{r.ID, agentdata.Token()} {
		work := knowledge.IndexWork{DocID: owner, Epoch: 1, Generation: owner, Fingerprint: "test", OriginRequestID: owner, Status: "ready"}
		if err := s.DB.Create(&work).Error; err != nil {
			t.Fatal(err)
		}
		if err := s.DB.Create(&knowledge.IndexAttempt{WorkID: work.ID, Status: "succeeded", StartedAt: time.Now()}).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, step := range []Step{
		{Key: "search", Kind: "search_api", Stage: "failed", ResultJSON: `{"model":"search-model","usage":{"prompt_tokens":5,"total_tokens":5}}`},
		{Key: "broken", Kind: "route_model", Stage: "failed", InputJSON: "{}", ResultJSON: "broken-json"},
		{Key: "rag", Kind: "post_acquisition_rag", Stage: "failed", ResultJSON: `{"retrieval_metrics":{"embedding_calls":1,"usage":{"prompt_tokens":7,"total_tokens":7}}}`},
	} {
		step.RequestID = r.ID
		if err := s.DB.Create(&step).Error; err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Accounting(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.ModelCalls) != 2 || len(got.SearchCalls) != 1 || len(got.Retrievals) != 1 || got.ChildRunID != "" || !got.RunAccounting.IndexSettled || len(got.RunAccounting.IndexWork) != 1 || len(got.RunAccounting.IndexAttempts) != 1 {
		t.Fatalf("native accounting: %+v", got)
	}
	unknown := 0
	for _, call := range got.ModelCalls {
		if call.Usage == nil {
			unknown++
			if call.Error == "" {
				t.Fatal("corrupt response was not diagnosed")
			}
		}
	}
	if unknown != 1 {
		t.Fatalf("unknown=%d", unknown)
	}
}
