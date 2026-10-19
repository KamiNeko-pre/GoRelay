package agentdata

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// The original input owns a caller-supplied contract. Natural-language requests
// without spec remain eligible for planning; storage does not invent one.
func submittedContract(input string) (string, error) {
	var request struct {
		Spec json.RawMessage `json:"spec"`
	}
	if json.Unmarshal([]byte(input), &request) != nil || len(request.Spec) == 0 || string(request.Spec) == "null" {
		return "", nil
	}
	var spec map[string]any
	d := json.NewDecoder(bytes.NewReader(request.Spec))
	d.UseNumber()
	if err := d.Decode(&spec); err != nil || spec == nil {
		return "", fmt.Errorf("%w: submitted contract must be an object", ErrConflict)
	}
	raw, err := json.Marshal(spec)
	return string(raw), err
}

type ContractAcceptance struct {
	Origin            string    `json:"origin"`
	ContractHash      string    `json:"contract_hash"`
	OriginalInputHash string    `json:"original_input_hash"`
	AcceptedAt        time.Time `json:"accepted_at"`
}

func ContractAcceptanceJSON(spec, origin, input string, now time.Time) string {
	raw, _ := json.Marshal(ContractAcceptance{Origin: origin, ContractHash: Hash(json.RawMessage(spec)), OriginalInputHash: Hash(input), AcceptedAt: now})
	return string(raw)
}

// RestoreSubmittedContract closes the historical two-write admission window.
// Once execution has begun, inconsistent admission is paused, never rewritten.
func (s *Store) RestoreSubmittedContract(ctx context.Context, id string) (Run, error) {
	var r Run
	var conflict error
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&r, "id = ?", id).Error; err != nil {
			return err
		}
		submitted, err := submittedContract(r.InputJSON)
		if err != nil {
			return err
		}
		if submitted == "" {
			return nil
		}
		if r.SpecJSON != "" {
			current, e := submittedContract(`{"spec":` + r.SpecJSON + `}`)
			if e == nil && current == submitted {
				if r.ContractAcceptanceJSON == "" {
					r.ContractAcceptanceJSON = ContractAcceptanceJSON(r.SpecJSON, "caller", r.InputJSON, time.Now())
					return tx.Model(&Run{}).Where("id = ?", id).Update("contract_acceptance_json", r.ContractAcceptanceJSON).Error
				}
				return nil
			}
			conflict = fmt.Errorf("%w: accepted contract differs from original submitted contract", ErrConflict)
		} else {
			var operations, children int64
			if err := tx.Model(&Operation{}).Where("run_id = ?", id).Count(&operations).Error; err != nil {
				return err
			}
			if err := tx.Model(&RunTask{}).Where("run_id = ?", id).Count(&children).Error; err != nil {
				return err
			}
			started := operations > 0 || children > 0 || r.PlanJSON != "" || r.ProgressJSON != "" || r.HostProgressJSON != "" || r.ResultJSON != "" || (r.LeaseUntil != nil && r.LeaseUntil.After(time.Now())) || r.IsTerminal()
			if !started {
				r.SpecJSON = submitted
				r.ContractAcceptanceJSON = ContractAcceptanceJSON(submitted, "caller", r.InputJSON, time.Now())
				return tx.Model(&Run{}).Where("id = ?", id).Updates(map[string]any{"spec_json": submitted, "contract_acceptance_json": r.ContractAcceptanceJSON}).Error
			}
			conflict = fmt.Errorf("%w: submitted contract missing after execution started", ErrConflict)
		}
		if !r.IsTerminal() {
			r.Status, r.LastError = "blocked_configuration", conflict.Error()
			return tx.Model(&Run{}).Where("id = ?", id).Updates(map[string]any{"status": r.Status, "last_error": r.LastError, "token": "", "lease_until": nil}).Error
		}
		return nil
	})
	if err != nil {
		return r, err
	}
	return r, conflict
}
