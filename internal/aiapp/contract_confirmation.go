package aiapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go-lua-crawler/internal/agentdata"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"strings"
	"time"
)

var ErrContractConfirmation = errors.New("contract proposal requires caller confirmation")

func contractConfirmationState(r agentdata.Run) string {
	if contractAwaitingConfirmation(r) {
		return "awaiting_confirmation"
	}
	if r.SpecJSON == "" {
		return "not_proposed"
	}
	var acceptance agentdata.ContractAcceptance
	if json.Unmarshal([]byte(r.ContractAcceptanceJSON), &acceptance) != nil || acceptance.ContractHash != agentdata.Hash(json.RawMessage(r.SpecJSON)) {
		return "unconfirmed"
	}
	if acceptance.Origin == "caller" {
		return "caller_confirmed"
	}
	if acceptance.Origin == "l1" {
		return "l1_confirmed"
	}
	return "unconfirmed"
}

func contractView(r agentdata.Run) map[string]any {
	view := map[string]any{"scope_confirmation": contractConfirmationState(r)}
	if r.ContractProposalJSON != "" {
		view["proposal"] = json.RawMessage(r.ContractProposalJSON)
		view["proposal_hash"] = agentdata.Hash(json.RawMessage(r.ContractProposalJSON))
	}
	return view
}

func contractAwaitingConfirmation(r agentdata.Run) bool {
	return r.SpecJSON == "" && r.ContractProposalJSON != ""
}

func (s *Service) proposeContract(ctx context.Context, r agentdata.Run, spec TaskSpec, call ToolCall) (ToolResult, error) {
	raw, _ := json.Marshal(spec)
	err := s.Data.WithOwnedRun(ctx, r.ID, r.Token, func(store *agentdata.Store) error {
		current, err := store.Run(ctx, r.ID)
		if err != nil {
			return err
		}
		if current.SpecJSON != "" {
			if !sameContractJSON([]byte(current.SpecJSON), raw) {
				return fmt.Errorf("task spec is frozen; changes require a new run")
			}
			return nil
		}
		if current.ContractProposalJSON != "" && !sameContractJSON([]byte(current.ContractProposalJSON), raw) {
			return fmt.Errorf("contract proposal is pending; it cannot be silently replaced")
		}
		return store.DB.Model(&agentdata.Run{}).Where("id = ?", r.ID).Updates(map[string]any{"contract_proposal_json": string(raw), "status": "needs_input", "last_error": ErrContractConfirmation.Error()}).Error
	})
	if err != nil {
		return ToolResult{}, err
	}
	return toolData(call, map[string]any{"proposal": spec, "proposal_hash": agentdata.Hash(json.RawMessage(raw)), "scope_confirmation": "awaiting_confirmation"})
}

// Approval takes only an exact proposal hash. It cannot provide new task facts.
func (s *Service) ApproveContract(ctx context.Context, id, hash string) (agentdata.Run, error) {
	var out agentdata.Run
	err := s.Data.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&out, "id = ?", id).Error; err != nil {
			return err
		}
		if out.Cancelled || out.IsTerminal() || out.Expired(time.Now()) {
			return agentdata.ErrConflict
		}
		if out.ContractProposalJSON == "" || hash != agentdata.Hash(json.RawMessage(out.ContractProposalJSON)) {
			return fmt.Errorf("%w: proposal hash does not match", agentdata.ErrConflict)
		}
		if out.SpecJSON != "" {
			if !sameContractJSON([]byte(out.SpecJSON), []byte(out.ContractProposalJSON)) {
				return agentdata.ErrConflict
			}
			return nil
		}
		if out.LeaseUntil != nil && out.LeaseUntil.After(time.Now()) {
			return fmt.Errorf("%w: proposal is still being recorded", agentdata.ErrConflict)
		}
		var spec TaskSpec
		if err := json.Unmarshal([]byte(out.ContractProposalJSON), &spec); err != nil {
			return err
		}
		if err := spec.Validate(); err != nil {
			return err
		}
		if spec.Goal != out.Goal {
			return agentdata.ErrConflict
		}
		out.SpecJSON = out.ContractProposalJSON
		out.ContractAcceptanceJSON = agentdata.ContractAcceptanceJSON(out.SpecJSON, "l1", out.InputJSON, time.Now())
		out.Status = "planning"
		if err := tx.Model(&agentdata.Run{}).Where("id = ?", id).Updates(map[string]any{"spec_json": out.SpecJSON, "contract_acceptance_json": out.ContractAcceptanceJSON, "status": out.Status, "last_error": "", "next_wake_at": nil}).Error; err != nil {
			return err
		}
		scoped := *s
		scoped.Data = agentdata.NewStore(tx)
		_, err := scoped.RecordIntervention(ctx, out, HumanIntervention{Key: "contract:" + hash, Kind: "approval", Reason: "Approved the exact Agent contract proposal without supplying new information."})
		return err
	})
	return out, err
}

// ContractRejection is caller feedback about an unapproved proposal. It cannot
// mutate an accepted contract or supply records, code, or source selectors.
type ContractRejection struct {
	ProposalHash string         `json:"proposal_hash"`
	Reason       string         `json:"reason"`
	Corrections  map[string]any `json:"corrections"`
}

func (s *Service) RejectContract(ctx context.Context, id string, request ContractRejection) (agentdata.Run, error) {
	var out agentdata.Run
	if request.ProposalHash == "" || strings.TrimSpace(request.Reason) == "" || len(request.Corrections) == 0 {
		return out, fmt.Errorf("proposal_hash, reason and scope corrections required")
	}
	for field := range request.Corrections {
		if strings.TrimSpace(field) == "" {
			return out, fmt.Errorf("correction field required")
		}
	}
	eventRaw, err := json.Marshal(request)
	if err != nil {
		return out, err
	}
	err = s.Data.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&out, "id = ?", id).Error; err != nil {
			return err
		}
		// A replay returns the prior decision without touching a newer proposal.
		var prior HumanIntervention
		err := tx.Where("run_id = ? AND "+"`key`"+" = ?", id, "contract-rejection:"+request.ProposalHash).First(&prior).Error
		if err == nil {
			if prior.Kind != "scope_feedback" || prior.Reason != string(eventRaw) {
				return agentdata.ErrConflict
			}
			if !contractAwaitingConfirmation(out) || agentdata.Hash(json.RawMessage(out.ContractProposalJSON)) != request.ProposalHash {
				return nil
			}
		}
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if out.Cancelled || out.IsTerminal() || out.Expired(time.Now()) || out.SpecJSON != "" || !contractAwaitingConfirmation(out) {
			return agentdata.ErrConflict
		}
		if request.ProposalHash != agentdata.Hash(json.RawMessage(out.ContractProposalJSON)) {
			return fmt.Errorf("%w: proposal hash does not match", agentdata.ErrConflict)
		}
		if out.LeaseUntil != nil && out.LeaseUntil.After(time.Now()) {
			return fmt.Errorf("%w: proposal is still being recorded", agentdata.ErrConflict)
		}
		var history []Message
		if out.PlanJSON != "" {
			if err := json.Unmarshal([]byte(out.PlanJSON), &history); err != nil {
				return err
			}
		}
		feedback := map[string]any{"code": "CONTRACT_PROPOSAL_REJECTED", "proposal_hash": request.ProposalHash, "reason": request.Reason, "corrections": request.Corrections, "next_steps": []string{"Compare the caller corrections with the original request and rejected proposal.", "Call plan_task again with corrected scope parameters; identical tool parameters remain idempotent.", "Wait for caller confirmation of the new exact proposal before acquisition."}}
		raw, err := json.Marshal(feedback)
		if err != nil {
			return err
		}
		// Original user input is reconstructed by logicalHistory. Host feedback
		// must use the durable assistant/tool observation channel to survive it.
		feedbackID := "contract-feedback-" + request.ProposalHash[:24]
		present := false
		for _, message := range history {
			present = present || message.Role == "tool" && message.ToolCallID == feedbackID
		}
		if !present {
			receipt, err := json.Marshal(ToolResult{CallID: feedbackID, Name: "host_contract_feedback", Data: raw})
			if err != nil {
				return err
			}
			history = append(history, Message{Role: "assistant", ToolCalls: []ToolCall{{ID: feedbackID, Type: "function", Function: FunctionCall{Name: "host_contract_feedback", Arguments: "{}"}}}}, Message{Role: "tool", ToolCallID: feedbackID, Content: string(receipt)})
		}
		messages, err := json.Marshal(history)
		if err != nil {
			return err
		}
		scoped := *s
		scoped.Data = agentdata.NewStore(tx)
		if _, err = scoped.RecordIntervention(ctx, out, HumanIntervention{Key: "contract-rejection:" + request.ProposalHash, Kind: "scope_feedback", Reason: string(eventRaw)}); err != nil {
			return err
		}
		archive := map[string]any{"previous_proposal": json.RawMessage(out.ContractProposalJSON), "feedback": feedback}
		if prior.ID == 0 {
			if err = scoped.Data.AddTrace(ctx, id, "contract_rejected", archive); err != nil {
				return err
			}
		}
		out.ContractProposalJSON = ""
		out.PlanJSON = string(messages)
		out.Status = "planning"
		out.LastError = ""
		out.NextWakeAt = nil
		out.Token = ""
		out.LeaseUntil = nil
		return tx.Model(&agentdata.Run{}).Where("id = ?", id).Updates(map[string]any{"contract_proposal_json": "", "plan_json": out.PlanJSON, "status": out.Status, "last_error": "", "next_wake_at": nil, "token": "", "lease_until": nil}).Error
	})
	return out, err
}
