package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestParseAnalysisQueryRejectsInvalidTime(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodGet, "/api/v1/analysis/snapshots?from=not-time", nil)

	_, _, _, err := parseAnalysisQuery(context)
	if err == nil {
		t.Fatal("expected invalid time error")
	}
}

func TestParseAnalysisQueryClampsOnlyAtRepositoryBoundary(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodGet, "/api/v1/analysis/snapshots?limit=999", nil)

	_, _, limit, err := parseAnalysisQuery(context)
	if err != nil {
		t.Fatalf("parse query: %v", err)
	}
	if limit != 999 {
		t.Fatalf("limit = %d, want handler to preserve the caller value", limit)
	}
}
