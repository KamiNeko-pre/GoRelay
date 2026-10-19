package aiapp

import (
	"context"
	"encoding/json"
	"github.com/gin-gonic/gin"
	"go-lua-crawler/internal/agentdata"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStoppedDeliveryKeepsUnanalysedSourcesSeparate(t *testing.T) {
	for _, reason := range []string{"cancelled", "provider unavailable"} {
		call := ToolCall{ID: "observed", Function: FunctionCall{Name: "inspect_source"}}
		receipt, _ := toolData(call, map[string]any{"source": map[string]any{"receipt": map[string]any{"url": "https://example.org/page", "status": 200, "method": "GET"}, "body": "not an accepted fact"}})
		raw, _ := json.Marshal(receipt)
		ops := []agentdata.Operation{{Kind: "inspect_source", Stage: "succeeded", ResultJSON: string(raw)}}
		r := TaskResult{}
		attachStoppedDelivery(&r, reason, ops)
		if !strings.Contains(r.Summary, "https://example.org/page") || len(r.Records) != 0 || r.Answer != nil || !strings.Contains(r.Summary, "尚未") {
			t.Fatalf("unanalysed material lost or promoted: %+v", r)
		}
		r = TaskResult{}
		ops[0].Stage = "failed"
		attachStoppedDelivery(&r, reason, ops)
		if strings.Contains(r.Summary, "https://example.org/page") {
			t.Fatal("failed fetch advertised as acquired")
		}
	}
}

func TestCancelEndpointPersistsReadableSourceSnapshot(t *testing.T) {
	s, r, _ := storedHTMLReader(t, "<main>Observed source material</main>")
	gin.SetMode(gin.TestMode)
	t.Setenv("GORELAY_ADMIN_TOKEN", "test-cancel")
	router := gin.New()
	RegisterRoutes(router, s)
	req := httptest.NewRequest("POST", "/api/v1/agent/runs/"+r.ID+"/cancel", nil)
	req.Header.Set("Authorization", "Bearer test-cancel")
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)
	if res.Code != 200 {
		t.Fatalf("cancel: %d %s", res.Code, res.Body.String())
	}
	saved, err := s.Data.Run(context.Background(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Status != "cancelled" || !strings.Contains(saved.ResultJSON, "https://fixture.example/docs") {
		t.Fatalf("cancelled without preserved material: %+v", saved)
	}
}
