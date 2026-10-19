package aiapp

import (
	"context"
	"encoding/json"
	"github.com/gin-gonic/gin"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/testdb"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestContractConfirmationNaturalProposalWaitsWithoutMoreInference(t *testing.T) {
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	model := &loopChatFixture{}
	s.Model = model
	ctx := context.Background()
	r, err := s.Create(ctx, CreateRunRequest{RequestKey: "confirmation", Goal: "collect release facts"})
	if err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 2; n++ {
		if err := s.Advance(ctx, r.ID); err != nil {
			t.Fatal(err)
		}
	}
	r, err = s.Data.Run(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(r)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(encoded, &fields)
	var proposal string
	_ = json.Unmarshal(fields["contract_proposal_json"], &proposal)
	if r.Status != "needs_input" || r.SpecJSON != "" || proposal == "" {
		t.Fatal("AI silently confirmed its own interpretation", r.Status, r.SpecJSON, proposal)
	}
	if err := s.Advance(ctx, r.ID); err == nil || model.calls != 1 {
		t.Fatal("waiting for user spent inference", err, model.calls)
	}
	gin.SetMode(gin.TestMode)
	t.Setenv("GORELAY_ADMIN_TOKEN", "confirmation-test")
	router := gin.New()
	RegisterRoutes(router, s)
	approve := func(hash string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{"proposal_hash": hash})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/runs/"+r.ID+"/contract/approve", strings.NewReader(string(body)))
		req.Header.Set("Authorization", "Bearer confirmation-test")
		out := httptest.NewRecorder()
		router.ServeHTTP(out, req)
		return out
	}
	if out := approve("wrong-proposal"); out.Code != http.StatusConflict {
		t.Fatal("wrong proposal accepted", out.Code, out.Body.String())
	}
	hash := agentdata.Hash(json.RawMessage(proposal))
	for n := 0; n < 2; n++ {
		if out := approve(hash); out.Code != http.StatusOK {
			t.Fatal("normal approval failed", out.Code, out.Body.String())
		}
	}
	accepted, err := s.Data.Run(ctx, r.ID)
	if err != nil || accepted.Status != "planning" || !sameContractJSON([]byte(accepted.SpecJSON), []byte(proposal)) || accepted.InputJSON != r.InputJSON {
		t.Fatal("approval changed original goal or failed to accept frozen proposal", err)
	}
	accounting, err := s.Accounting(ctx, accepted)
	if err != nil || len(accounting.Interventions) != 1 || AssessInterventions(&accounting).Level != "L1" {
		t.Fatal("approval not counted once as L1", accounting.Interventions, err)
	}
}

func TestContractConfirmationCallerContractHasIndependentAcceptanceRecord(t *testing.T) {
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	r, err := s.Create(context.Background(), admissionRequest("caller-confirmed"))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(r)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	var acceptance string
	_ = json.Unmarshal(fields["contract_acceptance_json"], &acceptance)
	var record struct {
		Origin       string `json:"origin"`
		ContractHash string `json:"contract_hash"`
	}
	if err := json.Unmarshal([]byte(acceptance), &record); err != nil || record.Origin != "caller" || record.ContractHash != agentdata.Hash(json.RawMessage(r.SpecJSON)) {
		t.Fatal("missing independent host acceptance", acceptance, err)
	}
}

func TestContractConfirmationProposalCrashDoesNotRemainSchedulable(t *testing.T) {
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r, err := s.Create(ctx, CreateRunRequest{RequestKey: "proposal-crash", Goal: "collect release facts"})
	if err != nil {
		t.Fatal(err)
	}
	r, err = s.Data.Claim(ctx, r.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	call := ToolCall{ID: "plan", Type: "function", Function: FunctionCall{Name: "plan_task", Arguments: `{"required_fields":[{"name":"name","type":"string"}]}`}}
	if _, err = s.executeDurableTool(ctx, r, call); err != nil {
		t.Fatal(err)
	}
	// Crash after proposal persistence, before SaveRun releases the lease.
	if err = s.Data.DB.Model(&agentdata.Run{}).Where("id = ?", r.ID).Updates(map[string]any{"lease_until": nil, "token": ""}).Error; err != nil {
		t.Fatal(err)
	}
	pending, err := s.Data.Pending(ctx, 10)
	if err != nil || len(pending) != 0 {
		t.Fatal("unconfirmed proposal entered scheduler after crash", len(pending), err)
	}
	if _, err = s.Data.Claim(ctx, r.ID, time.Minute); err == nil {
		t.Fatal("manual lease claim bypassed scope confirmation")
	}
}
