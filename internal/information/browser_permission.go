package information

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"

	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/agentstatus"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type BrowserPolicy struct {
	MaxFailedPlans int `json:"max_failed_plans"`
}
type BrowserGrant struct {
	RequestID  string           `json:"request_id" gorm:"primaryKey;size:32"`
	Code       agentstatus.Code `json:"code"`
	TargetURL  string           `json:"target_url" gorm:"type:text"`
	Reason     string           `json:"reason" gorm:"type:text"`
	LuaRunID   string           `json:"lua_run_id,omitempty" gorm:"size:32"`
	AssetID    string           `json:"asset_id,omitempty" gorm:"size:64"`
	PolicyJSON string           `json:"-" gorm:"type:text"`
	CreatedAt  time.Time        `json:"created_at"`
}

func (BrowserGrant) TableName() string { return "information_browser_grants" }

// No model tool accepts a grant, code, handle or token. Only the host can store
// permission after reviewing normal Lua failure or selecting a validated asset.
func (s *Service) storeBrowserGrant(ctx context.Context, r Request, target, reason, luaRun, assetID string) error {
	policy := s.BrowserPolicy
	if policy.MaxFailedPlans <= 0 {
		return agentstatus.Wrap(agentstatus.SystemConfiguration, fmt.Errorf("positive browser max_failed_plans required"))
	}
	grant := BrowserGrant{RequestID: r.ID, Code: agentstatus.AntiBotBrowserApproved, TargetURL: target, Reason: reason, LuaRunID: luaRun, AssetID: assetID, PolicyJSON: string(mustMarshal(policy))}
	q := s.DB.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&grant)
	if q.Error != nil {
		return q.Error
	}
	if q.RowsAffected == 0 {
		var prior BrowserGrant
		if err := s.DB.WithContext(ctx).First(&prior, "request_id = ?", r.ID).Error; err != nil {
			return err
		}
		if prior.TargetURL != target || prior.Code != grant.Code {
			return agentdata.ErrConflict
		}
	}
	return nil
}

func (s *Service) grantBrowserAsset(ctx context.Context, r Request, a Asset) error {
	if a.Kind != "browser" || !a.Active {
		return agentstatus.Wrap(agentstatus.AgentCorrection, fmt.Errorf("select an active browser asset"))
	}
	var proof struct {
		ApprovalRequest string `json:"browser_approval_request"`
	}
	if err := json.Unmarshal([]byte(a.ProofJSON), &proof); err != nil || proof.ApprovalRequest == "" {
		return agentstatus.Wrap(agentstatus.SystemState, fmt.Errorf("browser asset has no original host approval proof"))
	}
	var approval BrowserGrant
	if err := s.DB.WithContext(ctx).First(&approval, "request_id = ? AND code = ?", proof.ApprovalRequest, agentstatus.AntiBotBrowserApproved).Error; err != nil {
		return err
	}
	return s.storeBrowserGrant(ctx, r, a.EntryURL, "reuse of validated browser acquisition asset", "", a.ID)
}

func (s *Service) reserveBrowserStep(ctx context.Context, r Request, kind, target string, input any) (Step, bool, error) {
	var out Step
	fresh := false
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Restoring an admitted action never performs new work, even when its
		// failure already exhausted the permitted range.
		err := tx.First(&out, "request_id = ? AND `key` = ?", r.ID, kind+":"+agentdata.Hash(input)).Error
		if err == nil {
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var current Request
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&current, "id = ?", r.ID).Error; err != nil {
			return err
		}
		if terminal(current.Stage) {
			return agentstatus.Wrap(agentstatus.TaskCancelled, fmt.Errorf("request is terminal: %s", current.Stage))
		}
		if !current.Deadline.IsZero() && !current.Deadline.After(time.Now()) {
			return agentstatus.Wrap(agentstatus.TaskDeadline, fmt.Errorf("request deadline reached"))
		}
		var grant BrowserGrant
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&grant, "request_id = ? AND code = ?", r.ID, agentstatus.AntiBotBrowserApproved).Error; err != nil {
			return agentstatus.Wrap(agentstatus.AgentCorrection, fmt.Errorf("browser permission absent: finish normal Lua correction and obtain service-owned anti-bot approval"))
		}
		allowed, _ := url.Parse(grant.TargetURL)
		requested, err := url.Parse(target)
		if err != nil || requested.Hostname() == "" || allowed == nil || allowed.Hostname() != requested.Hostname() {
			return agentstatus.Wrap(agentstatus.AgentCorrection, fmt.Errorf("browser URL does not match the approved source host"))
		}
		var policy BrowserPolicy
		if err := json.Unmarshal([]byte(grant.PolicyJSON), &policy); err != nil || policy.MaxFailedPlans <= 0 {
			return agentstatus.Wrap(agentstatus.SystemConfiguration, fmt.Errorf("invalid frozen browser attempt policy"))
		}
		var failed int64
		if err := tx.Model(&Step{}).Where("request_id = ? AND kind IN ? AND stage IN ?", r.ID, []string{"trial_browser", "execute_browser", "inspect_browser"}, []string{"failed", "reserved"}).Count(&failed).Error; err != nil {
			return err
		}
		if failed >= int64(policy.MaxFailedPlans) {
			return agentstatus.Wrap(agentstatus.BrowserAttemptsExhausted, fmt.Errorf("%d failed or unresolved browser plans reached the frozen range %d; successful pagination never consumes this range", failed, policy.MaxFailedPlans), "Preserve actual browser receipts and the agent's correction history; do not reset the grant or rename an identical plan.")
		}
		scoped := *s
		scoped.DB = tx
		out, fresh, err = scoped.reserveStep(ctx, r, kind, input)
		return err
	})
	return out, fresh, err
}
