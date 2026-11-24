package router

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestLegacySingularTaskRouteIsNotRegistered(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := SetupRouter()
	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/v1/task"},
		{http.MethodPost, "/api/v1/tasks"},
		{http.MethodGet, "/api/v1/tasks"},
		{http.MethodGet, "/api/v1/agent/runs"},
	} {
		request := httptest.NewRequest(tc.method, tc.path, nil)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusNotFound {
			t.Fatalf("%s %s status = %d, want %d", tc.method, tc.path, response.Code, http.StatusNotFound)
		}
	}
}

func TestHealthzAndAuthGate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("GORELAY_ADMIN_TOKEN", "test-secret")
	r := SetupRouter()

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	out := httptest.NewRecorder()
	r.ServeHTTP(out, req)
	if out.Code != http.StatusOK {
		t.Fatalf("/healthz returned %d want 200", out.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/connectors", nil)
	out = httptest.NewRecorder()
	r.ServeHTTP(out, req)
	if out.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without bearer token, got %d", out.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/sources/demo/export?format=csv", nil)
	out = httptest.NewRecorder()
	r.ServeHTTP(out, req)
	if out.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 on export without bearer token, got %d", out.Code)
	}
}
