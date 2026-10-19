package aiapp

import (
	"encoding/json"
	"fmt"
	"github.com/gin-gonic/gin"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/testdb"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMonitorAPIUsesNormalAuthenticatedEntry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("GORELAY_ADMIN_TOKEN", "monitor-test")
	db := testdb.Open(t)
	s := NewService(db)
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	RegisterRoutes(router, s)
	request := func(method, path, payload, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res := httptest.NewRecorder()
		router.ServeHTTP(res, req)
		return res
	}
	for _, path := range []string{"/api/v1/agent/monitors", "/api/v1/agent/inbox"} {
		if res := request("GET", path, "", ""); res.Code != 401 {
			t.Fatalf("%s auth=%d", path, res.Code)
		}
	}
	spec := monitorSpec()
	raw, _ := json.Marshal(CreateMonitorRequest{RequestKey: "api-monitor", Goal: spec.Goal, Spec: &spec})
	res := request("POST", "/api/v1/agent/monitors", string(raw), "monitor-test")
	if res.Code != 201 {
		t.Fatalf("create=%d %s", res.Code, res.Body)
	}
	var created Monitor
	if err := json.Unmarshal(res.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/v1/agent/monitors/%d", created.ID)
	res = request("GET", path, "", "monitor-test")
	var view MonitorView
	json.Unmarshal(res.Body.Bytes(), &view)
	if res.Code != 200 || view.Config.Spec == nil || view.Config.Spec.RequiredFields[1].Name != "amount" || view.Enabled {
		t.Fatal("lost dynamic contract or enabled schedule", res.Code, res.Body)
	}
	res = request("POST", path+"/refresh", `{"request_key":"refresh-once"}`, "monitor-test")
	if res.Code != 202 {
		t.Fatal("refresh rejected", res.Code, res.Body)
	}
	var run agentdata.Run
	json.Unmarshal(res.Body.Bytes(), &run)
	res = request("POST", path+"/refresh", `{"request_key":"refresh-once"}`, "monitor-test")
	var replay agentdata.Run
	json.Unmarshal(res.Body.Bytes(), &replay)
	if res.Code != 202 || run.ID != replay.ID {
		t.Fatal("API retry duplicated Run")
	}
	res = request("PATCH", path, `{"enabled":false}`, "monitor-test")
	if res.Code != 200 {
		t.Fatal("pause rejected", res.Code, res.Body)
	}
	res = request("POST", "/api/v1/agent/monitors", `{"request_key":"bad","goal":"g","steam_game":1}`, "monitor-test")
	if res.Code != 400 {
		t.Fatal("unknown business field entered universal API", res.Code)
	}
	res = request("GET", "/api/v1/agent/monitors/not-an-id", "", "monitor-test")
	if res.Code != 400 {
		t.Fatal("invalid monitor id accepted")
	}
	expired := time.Now().Add(-time.Minute)
	item := MonitorInbox{MonitorID: created.ID, EventKey: agentdata.Hash("fixture"), KeyHash: agentdata.Hash("entity"), RunID: run.ID, Fields: json.RawMessage(`{"amount":4500}`), Sources: json.RawMessage(`[]`), Changes: json.RawMessage(`[]`), Status: "unread", Active: true, Verified: true, ValidUntil: &expired}
	if err := db.Create(&item).Error; err != nil {
		t.Fatal(err)
	}
	res = request("GET", "/api/v1/agent/inbox", "", "monitor-test")
	var inbox []MonitorInbox
	json.Unmarshal(res.Body.Bytes(), &inbox)
	if res.Code != 200 || len(inbox) != 1 || inbox[0].Verified {
		t.Fatal("expired item presented as current", res.Body)
	}
	itemPath := fmt.Sprintf("/api/v1/agent/inbox/%d", item.ID)
	res = request("PATCH", itemPath, `{"status":"read","observed_run_id":"old"}`, "monitor-test")
	if res.Code != 409 {
		t.Fatal("acknowledged unseen result", res.Code, res.Body)
	}
	body, _ := json.Marshal(map[string]string{"status": "ignored", "observed_run_id": run.ID})
	res = request("PATCH", itemPath, string(body), "monitor-test")
	if res.Code != 200 {
		t.Fatal("inbox acknowledgement", res.Code, res.Body)
	}
}
