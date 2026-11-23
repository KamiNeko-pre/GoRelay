package agentbench

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAccountingRefreshDoesNotCreateRunsOrChangeOriginalJudgment(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/api/v1/agent/runs/run" {
			t.Error("refresh created a new task")
			w.WriteHeader(400)
			return
		}
		fmt.Fprint(w, `{"run":{"id":"run"},"operations":[],"accounting":{"index_settled":true,"interventions":[{"kind":"source_hint"}]}}`)
	}))
	defer server.Close()
	bad := false
	rows, err := RefreshAccounting(context.Background(), ClientConfig{Endpoint: server.URL, Token: "fixture"}, []Trial{{RunID: "run", Status: "failed", Correct: &bad}})
	if err != nil || len(rows) != 1 || rows[0].Correct == nil || *rows[0].Correct || rows[0].Status != "failed" || rows[0].Accounting == nil || !rows[0].Accounting.IndexSettled || rows[0].HumanInterventions != 1 {
		t.Fatalf("refresh lost judgment/late cost: %+v %v", rows, err)
	}
	if rows[0].InterventionAssessment.Level != "L2" || rows[0].InterventionAssessment.EventsByLevel["L2"] != 1 {
		t.Fatal("late accounting lost intervention level", rows[0].InterventionAssessment)
	}
}
