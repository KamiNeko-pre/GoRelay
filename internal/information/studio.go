package information

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go-lua-crawler/internal/aiapp"
)

func studioRun(r Request, outcome string) map[string]any {
	var in Input
	_ = json.Unmarshal([]byte(r.InputJSON), &in)
	status := r.Status
	if status == "completed" && outcome == "partial" {
		status = "partial"
	}
	if status == "waiting_dependency" {
		status = "waiting"
	}
	if status == "running" && r.Stage == "blocked_provider" {
		status = r.Stage
	}
	if status == "running" && (r.LeaseUntil == nil || !r.LeaseUntil.After(time.Now())) {
		status = "queued"
	}

	return map[string]any{"id": r.ID, "goal": in.Goal, "status": status, "stage": r.Stage, "requirements": validRaw(r.RequirementsJSON),
		"budget_json": "{}", "operational_status": validRaw(r.StatusJSON), "last_error": r.LastError, "created_at": r.CreatedAt, "updated_at": r.UpdatedAt, "task_kind": "information"}
}

func (s *Service) studioSnapshot(ctx context.Context, r Request) (map[string]any, error) {
	out := map[string]any{"run": studioRun(r, ""), "children": []any{}, "operations": []any{}, "traces": []any{}, "metrics": map[string]any{}, "loop": nil}
	discoveryChildren, err := s.discoveryChildren(ctx, r.ID)
	if err != nil {
		return nil, err
	}
	var discovery map[string]any
	if len(discoveryChildren) > 0 {
		children := make([]map[string]any, 0, len(discoveryChildren))
		entries := []string{}
		for _, child := range discoveryChildren {
			children = append(children, studioRun(child, ""))
			if child.CurrentDeliveryID != "" {
				_, draft, err := s.SavedDelivery(ctx, child.CurrentDeliveryID)
				if err != nil {
					return nil, err
				}
				entries = append(entries, draft.DiscoveredSources...)
			}
		}
		discovery = map[string]any{"tasks": children, "entry_urls": entries}
	}
	if r.Status == "needs_input" {
		var wait informationWait
		if err := json.Unmarshal([]byte(r.WaitJSON), &wait); err != nil {
			return nil, err
		}
		out["question"] = wait.Reason
	}
	out["discovery"] = discovery
	out["work"] = map[string]any{"plan": validRaw(r.PlanJSON)}
	out["requirements"] = validRaw(r.RequirementsJSON)
	phase := 2
	if r.PlanJSON != "" && r.Stage == "update_plan" {
		phase = 1
	}
	if r.Stage == "understand_request" || r.Stage == "needs_input" {
		phase = 0
	}
	if r.Stage == "submit_delivery" {
		phase = 5
	}
	if r.CurrentDeliveryID != "" {
		phase = 6
	}
	out["progress"] = map[string]any{"stage": r.Stage, "phase": phase, "discovery": discovery}
	// Partial acquisition remains visible before the parent completes.
	diagnosticRunID := r.ChildRunID
	if diagnosticRunID != "" {
		child, err := s.Crawler.Data.Run(ctx, diagnosticRunID)
		if err != nil {
			return nil, err
		}
		children, err := s.studioChildStatuses(ctx, child.ID)
		if err != nil {
			return nil, err
		}

		out["crawler_run_id"] = child.ID
		if !terminal(r.Status) && r.ChildRunID != "" {
			p := 3
			if len(children) > 0 {
				p = 4
			}
			out["progress"].(map[string]any)["phase"] = p
		}
		out["crawler_state"] = map[string]any{"status": child.Status, "lease_until": child.LeaseUntil, "last_error": child.LastError}
		if !terminal(r.Status) {
			if child.Status == "needs_input" || strings.HasPrefix(child.Status, "blocked_") {
				out["run"].(map[string]any)["status"] = child.Status
			} else if child.LeaseUntil != nil && child.LeaseUntil.After(time.Now()) && !child.IsTerminal() {
				out["run"].(map[string]any)["status"] = "running"
			}
		}
		out["children"] = children

	}
	var view map[string]any
	var viewErr error
	if r.CurrentDeliveryID != "" {
		view, viewErr = s.deliveryDisplay(ctx, r.CurrentDeliveryID)
		out["result_provisional"] = false
	} else {
		view, viewErr = s.observationPreview(ctx, r.ID)
		out["result_provisional"] = true
	}
	if viewErr != nil {
		return nil, viewErr
	}
	out["result"] = view
	if r.CurrentDeliveryID != "" {
		outcome, _ := view["outcome"].(string)
		out["run"] = studioRun(r, outcome)
	}
	out["requirements"] = validRaw(r.RequirementsJSON)
	out["plan"] = validRaw(r.PlanJSON)

	steps, ops, err := s.studioRecentActivity(ctx, r, diagnosticRunID)
	if err != nil {
		return nil, err
	}
	out["routing_steps"], out["operations"] = steps, ops
	events, err := s.studioAgentEvents(ctx, diagnosticRunID)
	if err != nil {
		return nil, err
	}
	hostEvents, err := s.studioInformationEvents(ctx, r.ID)
	if err != nil {
		return nil, err
	}
	out["activity"] = append(hostEvents, events...)
	if r.Status == "failed" && r.CurrentDeliveryID == "" {
		out["resume_available"] = true
	}

	return out, nil
}

func registerStudioRoutes(api *gin.RouterGroup, s *Service) {
	api.GET("/studio/runs/:id/details", s.handleStudioDetails)
	api.GET("/studio/runs", func(c *gin.Context) {
		if !informationAvailable(c, s) {
			return
		}
		var rows []struct {
			Request         `gorm:"embedded"`
			DeliveryOutcome string
		}
		if err := s.DB.WithContext(c.Request.Context()).Model(&Request{}).Select("information_requests.*, JSON_UNQUOTE(JSON_EXTRACT(d.payload_json, '$.outcome')) AS delivery_outcome").Joins("LEFT JOIN information_deliveries d ON d.id = information_requests.current_delivery_id").Where("purpose = ? OR purpose IS NULL", "").Order("information_requests.created_at DESC").Limit(50).Find(&rows).Error; err != nil {
			aiapp.WriteAPIError(c, err)
			return
		}
		out := []map[string]any{}
		for _, r := range rows {
			out = append(out, studioRun(r.Request, r.DeliveryOutcome))
		}
		c.JSON(200, out)
	})
	api.POST("/studio/runs", func(c *gin.Context) {
		if !informationAvailable(c, s) {
			return
		}
		var in Input
		if !aiapp.DecodeAPIRequest(c, &in) {
			return
		}
		request, err := s.Create(c.Request.Context(), in)
		if err != nil {
			aiapp.WriteAPIError(c, err)
			return
		}
		c.JSON(202, studioRun(request, ""))
	})
	api.GET("/studio/runs/:id", func(c *gin.Context) {
		if !informationAvailable(c, s) {
			return
		}
		request, err := s.Request(c.Request.Context(), c.Param("id"))
		if err != nil {
			aiapp.WriteAPIError(c, err)
			return
		}
		out, err := s.studioSnapshot(c.Request.Context(), request)
		if err != nil {
			aiapp.WriteAPIError(c, err)
			return
		}
		c.JSON(200, out)
	})
	for _, action := range []string{"cancel", "advance", "resume", "clarify"} {
		action := action
		api.POST("/studio/runs/:id/"+action, func(c *gin.Context) {
			if !informationAvailable(c, s) {
				return
			}
			ctx := c.Request.Context()
			var err error
			switch action {
			case "clarify":
				var body struct {
					Guidance string `json:"guidance"`
				}
				if !aiapp.DecodeAPIRequest(c, &body) {
					return
				}
				err = s.Clarify(ctx, c.Param("id"), body.Guidance)
			case "cancel":
				err = s.Cancel(ctx, c.Param("id"))
			case "advance":
				err = s.Advance(ctx, c.Param("id"))
			case "resume":
				err = s.Resume(ctx, c.Param("id"))
			}
			if err != nil {
				aiapp.WriteAPIError(c, err)
				return
			}
			c.JSON(200, gin.H{"status": action})
		})
	}
	api.GET("/discovery/sources", func(c *gin.Context) {
		if !informationAvailable(c, s) {
			return
		}
		rows, err := s.knownSources(c.Request.Context(), nil)
		if err != nil {
			aiapp.WriteAPIError(c, err)
			return
		}
		c.JSON(200, gin.H{"sources": rows})
	})
}
