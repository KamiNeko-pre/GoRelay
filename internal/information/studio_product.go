package information

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/gin-gonic/gin"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/aiapp"
	task "go-lua-crawler/internal/task"
	"strconv"
	"time"
)

// User progress reads bounded metadata. Raw requests, source bodies, programs and
// reasoning belong to explicit diagnostic pages, never the polling response.
type studioActivityRow struct {
	ID        uint64    `json:"id"`
	Kind      string    `json:"kind"`
	Stage     string    `json:"stage"`
	Error     string    `json:"error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (s *Service) studioRecentActivity(ctx context.Context, r Request, childID string) ([]studioActivityRow, []studioActivityRow, error) {
	steps := []studioActivityRow{}
	ops := []studioActivityRow{}
	err := s.DB.WithContext(ctx).Model(&Step{}).Select("id, kind, stage, error, created_at, updated_at").Where("request_id = ?", r.ID).Order("id DESC").Limit(24).Scan(&steps).Error
	if err != nil {
		return nil, nil, err
	}
	if childID != "" {
		// Extract only observed errors, not the large result payload. The field is a
		// presentation projection; complete operation receipts remain unchanged.
		err = s.DB.WithContext(ctx).Model(&agentdata.Operation{}).Select("id, kind, stage, created_at, updated_at, CASE WHEN stage = 'failed' THEN COALESCE(NULLIF(JSON_UNQUOTE(JSON_EXTRACT(IF(JSON_VALID(result_json),result_json,'{}'), '$.error')), ''), JSON_UNQUOTE(JSON_EXTRACT(IF(JSON_VALID(result_json),result_json,'{}'), '$.failure.message')), '') ELSE '' END AS error").Where("run_id = ?", childID).Order("id DESC").Limit(24).Scan(&ops).Error
	}
	return steps, ops, err
}
func (s *Service) studioChildStatuses(ctx context.Context, childID string) ([]map[string]any, error) {
	var links []agentdata.RunTask
	if err := s.DB.WithContext(ctx).Where("run_id = ?", childID).Find(&links).Error; err != nil {
		return nil, err
	}
	out := []map[string]any{}
	ids := []uint64{}
	for _, link := range links {
		ids = append(ids, link.TaskID)
	}
	if len(ids) == 0 {
		return out, nil
	}
	var tasks []task.CrawlTask
	if err := s.DB.WithContext(ctx).Select("id, status, url, last_error, created_at, updated_at").Where("id IN ?", ids).Find(&tasks).Error; err != nil {
		return nil, err
	}
	for _, t := range tasks {
		out = append(out, map[string]any{"task": map[string]any{"id": t.ID, "status": t.Status, "url": t.URL, "last_error": t.LastError, "created_at": t.CreatedAt, "updated_at": t.UpdatedAt}})
	}
	return out, nil
}
func studioEvent(id string, at time.Time, raw string) (map[string]any, bool) {
	var event struct{ Type, Title, Detail, Tool, Status string }
	if json.Unmarshal([]byte(raw), &event) != nil {
		return nil, false
	}
	state := "done"
	switch event.Status {
	case "running", "pending", "started":
		state = "pending"
	case "failed", "error":
		state = "failed"
	case "waiting", "interrupted", "paused":
		state = "warning"
	}
	return map[string]any{"id": id, "time": at, "title": event.Title, "detail": event.Detail, "state": state, "kind": event.Type, "tool": event.Tool}, true
}
func (s *Service) studioInformationEvents(ctx context.Context, requestID string) ([]map[string]any, error) {
	events := []map[string]any{}
	var rows []Step
	if err := s.DB.WithContext(ctx).Select("id, result_json, created_at").Where("request_id = ? AND kind = ?", requestID, "agent_event").Order("id DESC").Limit(24).Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		if e, ok := studioEvent(fmt.Sprintf("information:%d", row.ID), row.CreatedAt, row.ResultJSON); ok {
			events = append(events, e)
		}
	}
	return events, nil
}
func (s *Service) studioAgentEvents(ctx context.Context, childID string) ([]map[string]any, error) {
	events := []map[string]any{}
	if childID == "" {
		return events, nil
	}
	var rows []agentdata.Trace
	if err := s.DB.WithContext(ctx).Select("id, data_json, created_at").Where("run_id = ? AND stage = ?", childID, "agent_event").Order("id DESC").Limit(24).Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		if e, ok := studioEvent(fmt.Sprintf("agent:%d", row.ID), row.CreatedAt, row.DataJSON); ok {
			events = append(events, e)
		}
	}
	return events, nil
}

type studioDebugPage struct {
	Limit                                    int
	StepBefore, OperationBefore, TraceBefore uint64
}

func debugPageSize(n int) int {
	if n < 1 {
		return 10
	}
	if n > 100 {
		return 100
	}
	return n
}
func (s *Service) studioDetails(ctx context.Context, r Request, page studioDebugPage) (map[string]any, error) {
	limit := debugPageSize(page.Limit)
	out := map[string]any{}
	next := map[string]uint64{"step_before": 1, "operation_before": 1, "trace_before": 1}
	more := false
	var steps []Step
	q := s.DB.WithContext(ctx).Where("request_id = ?", r.ID)
	if page.StepBefore > 0 {
		q = q.Where("id < ?", page.StepBefore)
	}
	if err := q.Order("id DESC").Limit(limit + 1).Find(&steps).Error; err != nil {
		return nil, err
	}
	if len(steps) > limit {
		steps = steps[:limit]
		more = true
	}
	if len(steps) > 0 {
		next["step_before"] = steps[len(steps)-1].ID
	}
	out["routing_steps"] = steps
	ops := []agentdata.Operation{}
	traces := []agentdata.Trace{}
	if r.ChildRunID != "" {
		q = s.DB.WithContext(ctx).Where("run_id = ?", r.ChildRunID)
		if page.OperationBefore > 0 {
			q = q.Where("id < ?", page.OperationBefore)
		}
		if err := q.Order("id DESC").Limit(limit + 1).Find(&ops).Error; err != nil {
			return nil, err
		}
		if len(ops) > limit {
			ops = ops[:limit]
			more = true
		}
		q = s.DB.WithContext(ctx).Where("run_id = ?", r.ChildRunID)
		if page.TraceBefore > 0 {
			q = q.Where("id < ?", page.TraceBefore)
		}
		if err := q.Order("id DESC").Limit(limit + 1).Find(&traces).Error; err != nil {
			return nil, err
		}
		if len(traces) > limit {
			traces = traces[:limit]
			more = true
		}
	}
	if len(ops) > 0 {
		next["operation_before"] = ops[len(ops)-1].ID
	}
	if len(traces) > 0 {
		next["trace_before"] = traces[len(traces)-1].ID
	}
	out["operations"], out["traces"], out["next"], out["has_more"] = ops, traces, next, more
	out["loop"] = aiapp.BuildLoopReport(traces)
	return out, nil
}
func (s *Service) handleStudioDetails(c *gin.Context) {
	if !informationAvailable(c, s) {
		return
	}
	r, err := s.Request(c.Request.Context(), c.Param("id"))
	if err != nil {
		aiapp.WriteAPIError(c, err)
		return
	}
	limit, err := strconv.Atoi(c.DefaultQuery("limit", "10"))
	if err != nil || limit < 1 || limit > 100 {
		aiapp.WriteAPIError(c, fmt.Errorf("limit must be an integer between 1 and 100"))
		return
	}
	page := studioDebugPage{Limit: limit}
	values := []struct {
		name   string
		target *uint64
	}{{"step_before", &page.StepBefore}, {"operation_before", &page.OperationBefore}, {"trace_before", &page.TraceBefore}}
	for _, v := range values {
		if raw := c.Query(v.name); raw != "" {
			n, e := strconv.ParseUint(raw, 10, 64)
			if e != nil || n == 0 {
				aiapp.WriteAPIError(c, fmt.Errorf("%s must be a positive record cursor", v.name))
				return
			}
			*v.target = n
		}
	}
	out, err := s.studioDetails(c.Request.Context(), r, page)
	if err != nil {
		aiapp.WriteAPIError(c, err)
		return
	}
	c.JSON(200, out)
}
