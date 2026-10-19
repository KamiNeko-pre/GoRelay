package information

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestInformationHTTPEntryExistsAndKeepsAdminBoundary(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("GORELAY_ADMIN_TOKEN", "local-test-token")
	r := gin.New()
	RegisterRoutes(r, nil)
	for _, tc := range []struct {
		method, path, body, token string
		status                    int
	}{
		{"POST", "/api/v1/information/requests", `{"request_key":"q","goal":"collect facts"}`, "", 401},
		{"POST", "/api/v1/information/requests", `{"request_key":"q","goal":"collect facts"}`, "local-test-token", 503},
		{"GET", "/api/v1/information/requests/q", "", "local-test-token", 503},
		{"POST", "/api/v1/information/requests/q/cancel", "", "local-test-token", 503},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer "+tc.token)
		req.Header.Set("Content-Type", "application/json")
		out := httptest.NewRecorder()
		r.ServeHTTP(out, req)
		if out.Code != tc.status {
			t.Fatalf("%s %s: got %d want %d %s", tc.method, tc.path, out.Code, tc.status, out.Body.String())
		}
	}
}
