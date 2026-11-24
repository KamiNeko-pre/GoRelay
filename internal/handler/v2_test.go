package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestV2HandlerGracefullyRejectsUnavailableService(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := V2Handler{}
	r := gin.New()
	r.GET("/api/v1/connectors", h.ListConnectors)
	r.GET("/api/v1/metrics/summary", h.MetricsSummary)
	r.GET("/api/v1/drift-events", h.ListDriftEvents)
	r.GET("/api/v1/sources/:id/export", h.ExportSource)
	r.GET("/api/v1/jobs/:id", h.GetJob)
	r.GET("/api/v1/jobs/:id/export", h.ExportJob)
	r.GET("/api/v1/evidence", h.ReadEvidence)

	for _, path := range []string{
		"/api/v1/connectors",
		"/api/v1/metrics/summary",
		"/api/v1/drift-events",
		"/api/v1/sources/demo/export",
		"/api/v1/jobs/1",
		"/api/v1/jobs/1/export",
		"/api/v1/evidence?ref=abc",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s returned %d, want 503", path, w.Code)
		}
	}
}
