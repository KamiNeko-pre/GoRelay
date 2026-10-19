package agentbench

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestBenchmarkClientStopsAtPendingCoreAndNeverSendsGold(t *testing.T) {
	posts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("missing token")
		}
		if r.Method == "POST" {
			posts++
			var raw map[string]any
			json.NewDecoder(r.Body).Decode(&raw)
			if len(raw) != 2 || raw["goal"] != "collect" {
				t.Error("gold leaked into request")
			}
			w.Write([]byte(`{"id":"r1"}`))
			return
		}
		w.Write([]byte(`{"run":{"id":"r1","status":"blocked_core"},"operations":[],"result":{"records":[]},"loop":{"rounds":[]}}`))
	}))
	defer srv.Close()
	m := Manifest{Status: "frozen", Mode: "fixture", EnvironmentID: "test", Cases: []Case{{ID: "a", Goal: "collect", Split: "development"}, {ID: "b", Goal: "next", Split: "development"}}}
	rows, err := Execute(context.Background(), ClientConfig{Endpoint: srv.URL, Token: "test-token", Batch: "test", Variant: "code", Repetitions: 1, Timeout: time.Second}, m, map[string]GoldCase{"a": {Verification: "human_confirmed", Rows: []map[string]any{{"secret_gold": "do not send"}}}}, nil)
	if err != nil || len(rows) != 1 || rows[0].Correct != nil || posts != 1 {
		t.Fatalf("core gap hidden or experiment continued: %v %+v", err, rows)
	}
}
