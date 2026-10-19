package aiapp

import (
	"bytes"
	"encoding/json"
	"github.com/gin-gonic/gin"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/testdb"
	"net/http/httptest"
	"testing"
)

func TestNormalEntryRecordsApprovalWithoutChangingAuthority(t *testing.T) {
	t.Setenv("GORELAY_ADMIN_TOKEN", "fixture-admin")
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	RegisterRoutes(router, s)
	call := func(path string, input any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(input)
		r := httptest.NewRequest("POST", path, bytes.NewReader(raw))
		r.Header.Set("Authorization", "Bearer fixture-admin")
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}
	created := call("/api/v1/agent/runs", map[string]string{"request_key": "approval-level", "goal": "fixture facts"})
	if created.Code != 202 {
		t.Fatal(created.Code, created.Body.String())
	}
	var run agentdata.Run
	if err := json.Unmarshal(created.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/agent/runs/" + run.ID + "/interventions"
	approval := HumanIntervention{Key: "approve-1", Kind: "approval", Reason: "approved the Agent proposal; supplied no new information"}
	first := call(path, approval)
	if first.Code != 200 {
		t.Fatalf("approval audit rejected: %d %s", first.Code, first.Body.String())
	}
	var a, b HumanIntervention
	_ = json.Unmarshal(first.Body.Bytes(), &a)
	replay := call(path, approval)
	_ = json.Unmarshal(replay.Body.Bytes(), &b)
	if replay.Code != 200 || a.ID != b.ID {
		t.Fatal("approval replay duplicated the event")
	}
	approval.Kind = "source_hint"
	if changed := call(path, approval); changed.Code != 409 {
		t.Fatal("approval can be relabeled as a hint under the same key")
	}
	approval.Key = "hint-1"
	if sibling := call(path, approval); sibling.Code != 200 {
		t.Fatal("existing hint receipt regressed")
	}
	approval.Key = "bad"
	approval.Kind = "goal_input"
	if initial := call(path, approval); initial.Code != 400 {
		t.Fatal("initial input is an intervention")
	}
	current, err := s.Data.Run(t.Context(), run.ID)
	if err != nil || current.SpecJSON != run.SpecJSON || current.InputJSON != run.InputJSON || current.BudgetJSON != run.BudgetJSON || current.Status != run.Status {
		t.Fatal("recording approval changed authority or task state", err)
	}
}
