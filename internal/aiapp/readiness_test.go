package aiapp

import (
	"encoding/json"
	"github.com/gin-gonic/gin"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/testdb"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNormalEntryRejectsUnavailableModelBeforeCreatingRun(t *testing.T) {
	for _, code := range []int{401, 503, 200} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			chatCalls, probes := 0, 0
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/chat/completions" || r.Method != http.MethodPost {
					t.Error("readiness did not use normal inference transport")
					w.WriteHeader(500)
					return
				}
				probes++
				chatCalls++
				if r.Header.Get("Authorization") != "Bearer fixture" {
					t.Error("auth header missing")
				}
				w.WriteHeader(code)
				if code == 200 {
					w.Write([]byte(`{"model":"fixture-model","choices":[{"message":{"role":"assistant","content":"ready"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`))
				}
			}))
			defer endpoint.Close()
			t.Setenv("GORELAY_AI_BACKEND", "")
			t.Setenv("GORELAY_AI_BASE_URL", endpoint.URL)
			t.Setenv("GORELAY_AI_MODEL", "fixture-model")
			t.Setenv("GORELAY_AI_API_KEY", "fixture")
			t.Setenv("GORELAY_ADMIN_TOKEN", "fixture-admin")
			s := NewService(testdb.Open(t))
			if err := s.Migrate(); err != nil {
				t.Fatal(err)
			}
			s.Configure()
			s.RequireSpendPolicy = false // No monetary experiment in this fixture.
			router := gin.New()
			RegisterRoutes(router, s)
			for i := 0; i < 2; i++ {
				req := httptest.NewRequest("POST", "/api/v1/agent/runs", strings.NewReader(`{"goal":"facts","request_key":"readiness"}`))
				req.Header.Set("Authorization", "Bearer fixture-admin")
				req.Header.Set("Content-Type", "application/json")
				res := httptest.NewRecorder()
				router.ServeHTTP(res, req)
				want := 503
				if code == 200 {
					want = 202
				}
				if res.Code != want {
					t.Fatalf("request accepted without readiness: status=%d want=%d body=%s", res.Code, want, res.Body.String())
				}
			}
			var count int64
			s.Data.DB.Model(&agentdata.Run{}).Count(&count)
			if code != 200 && count != 0 {
				t.Fatalf("unavailable model created %d runs", count)
			}
			if probes != 1 || chatCalls != 1 {
				t.Fatalf("probe not cached: probes=%d chat=%d", probes, chatCalls)
			}
		})
	}
}

func TestStatusExposesConfigurationFailure(t *testing.T) {
	t.Setenv("GORELAY_AI_BACKEND", "")
	t.Setenv("GORELAY_AI_BASE_URL", "invalid")
	t.Setenv("GORELAY_AI_MODEL", "")
	t.Setenv("GORELAY_AI_API_KEY", "")
	s := NewService(nil)
	s.Configure()
	router := gin.New()
	RegisterRoutes(router, s)
	res := httptest.NewRecorder()
	router.ServeHTTP(res, httptest.NewRequest("GET", "/api/v1/agent/status", nil))
	var data struct {
		Configuration map[string]string `json:"configuration_errors"`
	}
	json.Unmarshal(res.Body.Bytes(), &data)
	if data.Configuration["chat"] == "" {
		t.Fatal("configuration failure was silently swallowed")
	}
}
