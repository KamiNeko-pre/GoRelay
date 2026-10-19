package aiapp

import (
	"github.com/gin-gonic/gin"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAgentAPIBoundariesWithoutDatabase(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("GORELAY_ADMIN_TOKEN", "test-admin")
	r := gin.New()
	RegisterRoutes(r, nil)
	cases := []struct {
		method, path, body, token string
		status                    int
	}{{"GET", "/api/v1/agent/status", "", "", 200}, {"POST", "/api/v1/agent/runs", `{"request_key":"k","goal":"goal"}`, "", 401}, {"POST", "/api/v1/agent/runs", `{}`, "test-admin", 400}, {"POST", "/api/v1/agent/runs", `{"request_key":"k","goal":"goal"}`, "test-admin", 503}, {"POST", "/api/v1/knowledge/query", `{"question":"q"}`, "test-admin", 503}}
	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		if tc.token != "" {
			req.Header.Set("Authorization", "Bearer "+tc.token)
		}
		res := httptest.NewRecorder()
		r.ServeHTTP(res, req)
		if res.Code != tc.status {
			t.Errorf("%s %s = %d want %d: %s", tc.method, tc.path, res.Code, tc.status, res.Body.String())
		}
	}
	_ = http.MethodGet
}
