package aiapp

import (
	"context"
	"encoding/json"
	"errors"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/testdb"
	"strings"
	"testing"
	"time"
)

func TestContractRejectionRevisesDistinctBusinessParameters(t *testing.T) {
	for _, tc := range []struct {
		name, initial, corrected, field string
		expected                        any
	}{
		{"quantity", `{"required_fields":[{"name":"name","type":"string"}],"min_records":1,"join_keys":["name"]}`, `{"required_fields":[{"name":"name","type":"string"}],"min_records":6,"join_keys":["name"]}`, "min_records", 6},
		{"field", `{"required_fields":[{"name":"title","type":"string"}],"min_records":1,"join_keys":["title"]}`, `{"required_fields":[{"name":"name","type":"string"}],"min_records":1,"join_keys":["name"]}`, "required_fields", []map[string]string{{"name": "name", "type": "string"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewService(testdb.Open(t))
			if err := s.Migrate(); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			r, err := s.Create(ctx, CreateRunRequest{RequestKey: tc.name, Goal: "collect the caller requested facts"})
			if err != nil {
				t.Fatal(err)
			}
			owned, err := s.Data.Claim(ctx, r.ID, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			call := ToolCall{ID: "original", Type: "function", Function: FunctionCall{Name: "plan_task", Arguments: tc.initial}}
			receipt, err := s.executeDurableTool(ctx, owned, call)
			if err != nil || receipt.Error != "" {
				t.Fatal(receipt, err)
			}
			if err = s.Data.SaveRun(ctx, r.ID, owned.Token, map[string]any{}); err != nil {
				t.Fatal(err)
			}
			r, _ = s.Data.Run(ctx, r.ID)
			hash := agentdata.Hash(json.RawMessage(r.ContractProposalJSON))
			request := ContractRejection{ProposalHash: hash, Reason: "proposal does not satisfy original caller scope", Corrections: map[string]any{tc.field: tc.expected}}
			bad := request
			bad.ProposalHash = "wrong"
			if _, err = s.RejectContract(ctx, r.ID, bad); !errors.Is(err, agentdata.ErrConflict) {
				t.Fatal("wrong hash accepted", err)
			}
			if err = s.Data.DB.Model(&agentdata.Run{}).Where("id = ?", r.ID).Update("lease_until", time.Now().Add(time.Minute)).Error; err != nil {
				t.Fatal(err)
			}
			if _, err = s.RejectContract(ctx, r.ID, request); !errors.Is(err, agentdata.ErrConflict) {
				t.Fatal("active lease bypassed", err)
			}
			s.Data.DB.Model(&agentdata.Run{}).Where("id = ?", r.ID).Update("lease_until", nil)
			revised, err := s.RejectContract(ctx, r.ID, request)
			if err != nil {
				t.Fatal(err)
			}
			if revised.ContractProposalJSON != "" || revised.SpecJSON != "" || revised.Status != "planning" || revised.InputJSON != r.InputJSON || !strings.Contains(revised.PlanJSON, "CONTRACT_PROPOSAL_REJECTED") || !strings.Contains(revised.PlanJSON, tc.field) {
				t.Fatal("revision feedback or immutable input lost", revised)
			}
			state, err := s.state(ctx, revised)
			if err != nil {
				t.Fatal(err)
			}
			projected, err := CompactContext(state, state.Messages)
			if err != nil {
				t.Fatal(err)
			}
			projection, _ := json.Marshal(projected)
			if !strings.Contains(string(projection), "CONTRACT_PROPOSAL_REJECTED") {
				t.Fatal("caller corrections discarded before model request")
			}
			// A same-hash proposal reintroduced by old code is still rejected; a different
			// new proposal below remains protected against stale caller retries.
			if err = s.Data.DB.Model(&agentdata.Run{}).Where("id = ?", r.ID).Updates(map[string]any{"contract_proposal_json": r.ContractProposalJSON, "status": "needs_input"}).Error; err != nil {
				t.Fatal(err)
			}
			if again, err := s.RejectContract(ctx, r.ID, request); err != nil || again.ContractProposalJSON != "" {
				t.Fatal("same rejected business contract resurrected", again.Status, err)
			}
			if _, err = s.RejectContract(ctx, r.ID, request); err != nil {
				t.Fatal("caller retry not idempotent", err)
			}
			owned, err = s.Data.Claim(ctx, r.ID, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			call.ID = "corrected"
			call.Function.Arguments = tc.corrected
			receipt, err = s.executeDurableTool(ctx, owned, call)
			if err != nil || receipt.Error != "" {
				t.Fatal("corrected parameters blocked", receipt, err)
			}
			if err = s.Data.SaveRun(ctx, r.ID, owned.Token, map[string]any{}); err != nil {
				t.Fatal(err)
			}
			next, _ := s.Data.Run(ctx, r.ID)
			if next.ContractProposalJSON == "" || next.ContractProposalJSON == r.ContractProposalJSON {
				t.Fatal("new distinct proposal missing")
			}
			if _, err = s.RejectContract(ctx, r.ID, request); err != nil {
				t.Fatal(err)
			}
			unchanged, _ := s.Data.Run(ctx, r.ID)
			if unchanged.ContractProposalJSON != next.ContractProposalJSON {
				t.Fatal("stale retry cleared new proposal")
			}
			accepted, err := s.ApproveContract(ctx, r.ID, agentdata.Hash(json.RawMessage(next.ContractProposalJSON)))
			if err != nil {
				t.Fatal(err)
			}
			request.ProposalHash = agentdata.Hash(json.RawMessage(accepted.ContractProposalJSON))
			if _, err = s.RejectContract(ctx, r.ID, request); !errors.Is(err, agentdata.ErrConflict) {
				t.Fatal("approved scope mutated", err)
			}
			accounting, err := s.Accounting(ctx, accepted)
			if err != nil || len(accounting.Interventions) != 2 || AssessInterventions(&accounting).Level != "L2" {
				t.Fatal("caller feedback attribution lost", accounting.Interventions, err)
			}
			var traces []agentdata.Trace
			s.Data.DB.Where("run_id = ? AND stage = ?", r.ID, "contract_rejected").Find(&traces)
			if len(traces) != 1 || !strings.Contains(traces[0].DataJSON, "previous_proposal") {
				t.Fatal("archive missing", traces)
			}
		})
	}
}
