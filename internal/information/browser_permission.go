package information

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/agentstatus"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type BrowserGrant struct {
	TargetsJSON string           `json:"-" gorm:"type:text"` // Host-approved targets; old single-target grants remain readable.
	RequestID   string           `json:"request_id" gorm:"primaryKey;size:32"`
	Code        agentstatus.Code `json:"code"`
	TargetURL   string           `json:"target_url" gorm:"type:text"`
	Reason      string           `json:"reason" gorm:"type:text"`
	LuaRunID    string           `json:"lua_run_id,omitempty" gorm:"size:32"`
	AssetID     string           `json:"asset_id,omitempty" gorm:"size:64"`
	CreatedAt   time.Time        `json:"created_at"`
}

func (BrowserGrant) TableName() string { return "information_browser_grants" }

// No model tool accepts a grant, code, handle or token. Only the host can store
// permission after reviewing normal Lua failure or selecting a validated asset.
func (s *Service) storeBrowserGrant(ctx context.Context, r Request, target, reason, luaRun, assetID string) error {
	grant := BrowserGrant{RequestID: r.ID, Code: agentstatus.AntiBotBrowserApproved, TargetURL: target, Reason: reason, LuaRunID: luaRun, AssetID: assetID}
	q := s.DB.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&grant)
	if q.Error != nil {
		return q.Error
	}
	if q.RowsAffected == 0 {
		var prior BrowserGrant
		if err := s.DB.WithContext(ctx).First(&prior, "request_id = ?", r.ID).Error; err != nil {
			return err
		}
		if prior.Code != grant.Code {
			return agentdata.ErrConflict
		}
		if !browserGrantAllows(prior, target) {
			return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
				if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&prior, "request_id = ?", r.ID).Error; err != nil {
					return err
				}
				targets := []string{prior.TargetURL}
				if prior.TargetsJSON != "" {
					if err := json.Unmarshal([]byte(prior.TargetsJSON), &targets); err != nil {
						return err
					}
				}
				if !browserGrantAllows(prior, target) {
					targets = append(targets, target)
				}
				return tx.Model(&prior).Update("targets_json", string(mustMarshal(targets))).Error
			})
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
	if !browserGrantAllows(approval, a.EntryURL) {
		return actionFailure("BROWSER_SOURCE_REFERENCE_INVALID", fmt.Errorf("browser asset approval does not cover requested source %s", a.EntryURL), "Use the normal direct acquisition route for the new source, or use a host-approved browser target with browser_extract.", "Reuse this browser asset only within a host-approved source; changing url cannot transfer approval to another host.")
	}
	return s.storeBrowserGrant(ctx, r, a.EntryURL, "reuse of validated browser acquisition asset", "", a.ID)
}

func (s *Service) reserveBrowserStep(ctx context.Context, r Request, kind, target string, input any) (Step, bool, error) {
	var out Step
	fresh := false
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Restoring an admitted action never performs new work, even when its
		// failure already exhausted the permitted range.
		scoped := *s
		scoped.DB = tx
		var found bool
		var err error
		out, found, err = scoped.executionStep(ctx, r, kind, input)
		if err != nil || found {
			return err
		}
		var current Request
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&current, "id = ?", r.ID).Error; err != nil {
			return err
		}
		if terminal(current.Status) {
			return agentstatus.Wrap(agentstatus.TaskCancelled, fmt.Errorf("request is terminal: %s", current.Stage))
		}
		if !current.Deadline.IsZero() && !current.Deadline.After(time.Now()) {
			return agentstatus.Wrap(agentstatus.TaskDeadline, fmt.Errorf("request deadline reached"))
		}
		var grant BrowserGrant
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&grant, "request_id = ? AND code = ?", r.ID, agentstatus.AntiBotBrowserApproved).Error; err != nil {
			return agentstatus.Wrap(agentstatus.AgentCorrection, fmt.Errorf("browser permission absent: finish normal Lua correction and obtain service-owned anti-bot approval"))
		}
		if !browserGrantAllows(grant, target) {
			return agentstatus.Wrap(agentstatus.AgentCorrection, fmt.Errorf("browser URL does not match any host-approved source"))
		}
		out, fresh, err = scoped.reserveStep(ctx, r, kind, input)
		return err
	})
	return out, fresh, err
}

func browserGrantAllows(grant BrowserGrant, target string) bool {
	requested, err := url.Parse(target)
	if err != nil || requested.Hostname() == "" {
		return false
	}
	targets := []string{grant.TargetURL}
	if grant.TargetsJSON != "" {
		if json.Unmarshal([]byte(grant.TargetsJSON), &targets) != nil {
			return false
		}
	}
	for _, raw := range targets {
		allowed, err := url.Parse(raw)
		if err == nil && strings.EqualFold(allowed.Hostname(), requested.Hostname()) {
			return true
		}
	}
	return false
}

func browserFallbackReason(resp acquisition.Response) (string, bool) {
	reason, _ := browserSourceDiagnosis(resp)
	return reason, reason != ""
}

// Only a confirmed block invalidates a capture. Structure and page length
// heuristics can suggest Browser without discarding readable source evidence.
func browserSourceDiagnosis(resp acquisition.Response) (reason string, confirmedBlock bool) {
	status := resp.Receipt.Status
	trimmed := strings.TrimSpace(resp.Body)
	if trimmed == "" || json.Valid([]byte(trimmed)) {
		return "", false
	}
	lower := strings.ToLower(trimmed)
	if status == 403 || status == 503 || status >= 200 && status < 300 {
		for _, marker := range []string{"cf-browser-verification", "verify you are human", "enable javascript and cookies", "just a moment..."} {
			if strings.Contains(lower, marker) {
				return "source returned a challenge page: " + marker, true
			}
		}
	}
	if status >= 200 && status < 300 && strings.Contains(lower, "<script") {
		if doc, parseErr := goquery.NewDocumentFromReader(strings.NewReader(trimmed)); parseErr == nil {
			hasDynamicLoader := false
			doc.Find("script").Each(func(_ int, s *goquery.Selection) {
				bodyLower := strings.ToLower(s.Text())
				if strings.Contains(bodyLower, "document.write") && strings.Contains(bodyLower, ".js") {
					hasDynamicLoader = true
				} else if strings.Contains(bodyLower, "createelement") && strings.Contains(bodyLower, "script") {
					hasDynamicLoader = true
				}
			})
			hasEmptySPARoot := false
			doc.Find("#app, #root, #__next, #__nuxt, [data-reactroot], [ng-app]").Each(func(_ int, s *goquery.Selection) {
				if len([]rune(strings.Join(strings.Fields(s.Text()), " "))) < 20 {
					hasEmptySPARoot = true
				}
			})
			// Evaluate only <body> content (excluding <head><title> and site chrome)
			// so a SPA shell with a <title> tag is not mistaken for static content.
			body := doc.Find("body")
			if body.Length() == 0 {
				body = doc.Selection
			}
			body.Find("script,style,noscript,template,svg,nav,header,footer,[role=navigation],[role=banner],[role=contentinfo]").Remove()
			bodyText := strings.Join(strings.Fields(body.Text()), " ")
			if bodyText == "" && body.Find("a[href], img[alt], input[value]").Length() == 0 {
				return "source returned a script-rendered shell", true
			}
			if (hasDynamicLoader || hasEmptySPARoot) && len([]rune(bodyText)) < 160 {
				hasStaticData := false
				body.Find("table").Each(func(_ int, tbl *goquery.Selection) {
					if tbl.Find("tr").Length() >= 2 && len([]rune(strings.Join(strings.Fields(tbl.Text()), " "))) >= 40 {
						hasStaticData = true
					}
				})
				if !hasStaticData {
					return "source may require script rendering", false
				}
			}
		}
	}
	return "", false
}

func (s *Service) maybeApproveBrowserFromFetch(ctx context.Context, r *Request, step Step, scope []string, resp acquisition.Response) (string, bool) {
	if s == nil || s.DB == nil || r == nil {
		return "", false
	}
	reason, blocked := browserSourceDiagnosis(resp)
	if reason == "" {
		return "", false
	}
	target := resp.Receipt.URL
	if !urlInScope(target, scope) {
		return "", false
	}
	if blocked && reason != "source returned a script-rendered shell" {
		if resp.Receipt.RequestProfile == "" {
			return "", false
		}
		var priorAttempts int64
		if err := s.DB.WithContext(ctx).Table("information_task_documents d").
			Joins("JOIN information_steps p ON p.id = d.receipt_id AND p.request_id = d.request_id").
			Where("d.request_id = ? AND d.source_url = ? AND d.receipt_id <> ? AND d.status = ? AND p.eino_epoch = ? AND d.request_profile <> '' AND d.request_profile <> ?", r.ID, target, step.ID, "failed", r.EinoEpoch, resp.Receipt.RequestProfile).
			Count(&priorAttempts).Error; err != nil || priorAttempts == 0 {
			return "", false
		}
	}
	if err := s.storeBrowserGrant(ctx, *r, target, reason, "", ""); err != nil {
		return "", false
	}
	return target, true
}
