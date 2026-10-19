package aiapp

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/gin-gonic/gin"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Opt-in visual check server. SQL/API/UI are real; rows are explicitly labeled
// fixtures. No runtime, model, scheduler or Publisher is started.
func TestMonitorWorkbenchPreview(t *testing.T) {
	addr := os.Getenv("GORELAY_WORKBENCH_PREVIEW_ADDR")
	if addr == "" {
		t.Skip("opt-in UI preview only")
	}
	t.Setenv("GORELAY_ADMIN_TOKEN", "monitor-preview")
	s, m, _ := seedMonitorPlan(t, false)
	for i := 0; i < 14; i++ {
		fields, _ := json.Marshal(map[string]any{"id": fmt.Sprintf("fixture-%d", i), "title": "本地回放夹具，非真实购买机会", "amount": 2500 + i, "unit": "CNY", "labels": []string{"zh"}})
		sources := json.RawMessage(`[{"source_id":"fixture","url":"https://example.com/data","fetched_at":"2026-10-10T08:00:00Z","data_version":"fixture"}]`)
		item := MonitorInbox{MonitorID: m.ID, EventKey: fmt.Sprintf("ui-%d", i), KeyHash: fmt.Sprintf("key-%d", i), RunID: m.LastRunID, Fields: fields, Sources: sources, Changes: json.RawMessage(`[]`), Status: "unread", Active: true, Verified: true}
		if err := s.Data.DB.Create(&item).Error; err != nil {
			t.Fatal(err)
		}
	}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterRoutes(router, s)
	web, err := filepath.Abs("../../web")
	if err != nil {
		t.Fatal(err)
	}
	router.Static("/assets", filepath.Join(web, "assets"))
	// RegisterRoutes uses the production relative path. Replace its filesystem
	// resolution by routing this test request to the same actual HTML file.
	root := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/agent" {
			http.ServeFile(w, r, filepath.Join(web, "agent.html"))
			return
		}
		router.ServeHTTP(w, r)
	})
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: root, ReadHeaderTimeout: 5 * time.Second}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	t.Logf("fixture-only preview ready http://%s/agent token=monitor-preview; expires in 180 seconds", listener.Addr())
	select {
	case err = <-done:
		if err != http.ErrServerClosed {
			t.Fatal(err)
		}
	case <-time.After(180 * time.Second):
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err = server.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}
