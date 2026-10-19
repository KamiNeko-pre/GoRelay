package aiapp

import (
	"context"
	"encoding/json"
	"errors"
	"go-lua-crawler/internal/agentdata"
	"gorm.io/gorm"
	"strings"
	"testing"
	"time"
)

func TestDeliveryCancellationSnapshotIsAtomic(t *testing.T) {
	s, r, _ := storedHTMLReader(t, "<main>saved material</main>")
	const hook = "test:reject-delivery-save"
	s.Data.DB.Callback().Update().Before("gorm:update").Register(hook, func(tx *gorm.DB) {
		if m, ok := tx.Statement.Dest.(map[string]any); ok {
			if _, exists := m["result_json"]; exists {
				tx.AddError(errors.New("injected snapshot write failure"))
			}
		}
	})
	err := s.CancelWithDelivery(context.Background(), r.ID)
	s.Data.DB.Callback().Update().Remove(hook)
	if err == nil {
		t.Fatal("expected failed snapshot write")
	}
	got, _ := s.Data.Run(context.Background(), r.ID)
	if got.Cancelled || got.Status == "cancelled" {
		t.Fatal("cancel committed without delivery snapshot")
	}
	if err = s.CancelWithDelivery(context.Background(), r.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Data.Run(context.Background(), r.ID)
	if !got.Cancelled || got.ResultJSON == "" {
		t.Fatal("retry did not commit both cancellation and snapshot")
	}
}
func TestDeliveryStopProjectionIsIdempotentAndNamesHandles(t *testing.T) {
	r := TaskResult{}
	reply, _ := toolData(ToolCall{ID: "page-handle", Function: FunctionCall{Name: "inspect_source"}}, map[string]any{"source": map[string]any{"receipt": map[string]any{"url": "https://source.example/page", "status": 200, "method": "GET", "fetched_at": time.Now()}, "body": "saved"}})
	raw, _ := json.Marshal(reply)
	ops := []agentdata.Operation{{Kind: "inspect_source", Stage: "succeeded", ResultJSON: string(raw)}}
	attachStoppedDelivery(&r, "stop", ops)
	first := string(mustJSON(r))
	attachStoppedDelivery(&r, "stop", ops)
	if first != string(mustJSON(r)) {
		t.Fatal("rebuilding stop delivery duplicated/changed output")
	}
	if !strings.Contains(first, "page-handle") {
		t.Fatal("no actual reread handle")
	}
}

func TestDeliveryExpiredRunKeepsReadableSnapshot(t *testing.T) {
	s, r, _ := storedHTMLReader(t, "<main>saved material</main>")
	if err := s.Data.DB.Model(&agentdata.Run{}).Where("id = ?", r.ID).Update("deadline", time.Now().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.Advance(context.Background(), r.ID); err != nil {
		t.Fatalf("expired run cannot reach delivery: %v", err)
	}
	got, err := s.Data.Run(context.Background(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "expired" || !strings.Contains(got.ResultJSON, "fixture.example/docs") {
		t.Fatalf("expiry discarded acquired evidence: status=%s result=%s", got.Status, got.ResultJSON)
	}
}
