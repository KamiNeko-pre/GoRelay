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
	request := httptest.NewRequest(http.MethodPost, "/api/v1/task", nil)
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNotFound)
	}
}

func TestUnavailableInformationFrontendKeepsCrawlerRoutesAvailable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("GORELAY_ADMIN_TOKEN", "local-test-token")
	r := SetupRouterWithInformation(nil, nil)
	for _, tc := range []struct {
		path   string
		status int
	}{
		{"/healthz", 200},
		{"/api/v1/agent/status", 200},
		{"/api/v1/information/requests/unavailable", 503},
	} {
		req := httptest.NewRequest("GET", tc.path, nil)
		req.Header.Set("Authorization", "Bearer local-test-token")
		out := httptest.NewRecorder()
		r.ServeHTTP(out, req)
		if out.Code != tc.status {
			t.Fatalf("%s returned %d want %d", tc.path, out.Code, tc.status)
		}
	}
}
