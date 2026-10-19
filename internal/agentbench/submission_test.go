package agentbench

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestColdReadinessSubmissionDoesNotUsePollTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(16 * time.Second):
			}
			_, _ = w.Write([]byte(`{"id":"cold"}`))
			return
		}
		_, _ = w.Write([]byte(`{"run":{"id":"cold","status":"failed"},"accounting":{"interventions":[]}}`))
	}))
	defer server.Close()
	manifest := Manifest{Status: "frozen", EnvironmentID: "fixture", Cases: []Case{{ID: "cold", Goal: "cold inference readiness", Split: "development"}}}
	rows, err := Execute(context.Background(), ClientConfig{Endpoint: server.URL, Token: "fixture", Batch: "cold", Variant: "code", Repetitions: 1, Timeout: time.Second}, manifest, nil, nil)
	if err != nil || len(rows) != 1 || rows[0].RunID != "cold" {
		t.Fatalf("valid cold admission rejected before Run creation: %v %+v", err, rows)
	}
}

func TestSubmissionFailureIsEmittedAndStopsFurtherTasks(t *testing.T) {
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"error":"model unavailable"}`))
	}))
	defer server.Close()
	manifest := Manifest{Status: "frozen", EnvironmentID: "fixture", Cases: []Case{{ID: "first", Goal: "facts", Split: "development"}, {ID: "next", Goal: "more facts", Split: "development"}}}
	var emitted []Trial
	rows, err := Execute(context.Background(), ClientConfig{Endpoint: server.URL, Token: "fixture", Batch: "failure", Variant: "code", Repetitions: 1}, manifest, nil, func(row Trial) error { emitted = append(emitted, row); return nil })
	if err == nil || posts != 1 || len(rows) != 1 || len(emitted) != 1 || rows[0].Status != "submission_failed" || rows[0].RunID != "" || rows[0].Correct != nil {
		t.Fatalf("failed frozen case disappeared or retried: %v %+v emitted=%d posts=%d", err, rows, len(emitted), posts)
	}
	if summary := Summarize(rows)[0]; summary.Trials != 1 || summary.CorrectTasks != 0 {
		t.Fatal("failure excluded from denominator", summary)
	}
	raw, _ := json.Marshal(rows[0])
	if len(raw) == 0 {
		t.Fatal("failed admission cannot be persisted")
	}
}
