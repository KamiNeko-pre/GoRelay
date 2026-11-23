package information

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"go-lua-crawler/internal/aiapp"
)

func TestSnapshotReadSessionDoesNotAuthorizeWrites(t *testing.T) {
	t.Setenv("GORELAY_ADMIN_TOKEN", "test-service-token")
	router := gin.New()
	router.GET("/api/v1/information/evidence/example", evidenceAccess, func(c *gin.Context) { c.Status(http.StatusOK) })
	router.POST("/write", aiapp.RequireAdmin, func(c *gin.Context) { c.Status(http.StatusOK) })
	call := func(method, path, token string, cookie *http.Cookie) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, path, nil)
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		if cookie != nil {
			request.AddCookie(cookie)
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		return response
	}
	if got := call("GET", "/api/v1/information/evidence/example", "", nil).Code; got != http.StatusUnauthorized {
		t.Fatalf("private snapshot without credentials: %d", got)
	}
	response := call("GET", "/api/v1/information/evidence/example", "test-service-token", nil)
	cookies := response.Result().Cookies()
	if response.Code != http.StatusOK || len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].Value == "test-service-token" || cookies[0].Path != "/api/v1/information/evidence/" {
		t.Fatalf("read-only browser session: %d, %v", response.Code, cookies)
	}
	if got := call("GET", "/api/v1/information/evidence/example", "", cookies[0]).Code; got != http.StatusOK {
		t.Fatalf("next citation tab needs a second credential prompt: %d", got)
	}
	if got := call("POST", "/write", "", cookies[0]).Code; got != http.StatusUnauthorized {
		t.Fatalf("evidence read session authorized a write: %d", got)
	}
}
