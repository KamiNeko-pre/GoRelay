package aiapp

import (
	"github.com/gin-gonic/gin"
	"strconv"
	"time"
)

func monitorID(c *gin.Context) (uint64, bool) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		c.JSON(400, gin.H{"error": "positive numeric id required"})
		return 0, false
	}
	return id, true
}

func registerMonitorRoutes(api *gin.RouterGroup, s *Service) {
	api.POST("/agent/monitors", func(c *gin.Context) {
		var in CreateMonitorRequest
		if !body(c, &in) || !available(c, s) {
			return
		}
		out, err := s.CreateMonitor(c.Request.Context(), in)
		if err != nil {
			apiError(c, err)
			return
		}
		c.JSON(201, out)
	})
	api.GET("/agent/monitors", func(c *gin.Context) {
		if !available(c, s) {
			return
		}
		var rows []Monitor
		if err := s.Data.DB.WithContext(c.Request.Context()).Order("id DESC").Limit(50).Find(&rows).Error; err != nil {
			apiError(c, err)
			return
		}
		out := make([]MonitorView, 0, len(rows))
		for _, m := range rows {
			config, err := m.Config()
			if err != nil {
				apiError(c, err)
				return
			}
			out = append(out, MonitorView{Monitor: m, ExecutionPlanReady: m.PlanJSON != "", Config: config, Checks: []MonitorCheck{}})
		}
		c.JSON(200, out)
	})
	api.GET("/agent/monitors/:id", func(c *gin.Context) {
		if !available(c, s) {
			return
		}
		id, ok := monitorID(c)
		if !ok {
			return
		}
		out, err := s.MonitorView(c.Request.Context(), id)
		if err != nil {
			apiError(c, err)
			return
		}
		c.JSON(200, out)
	})
	api.PATCH("/agent/monitors/:id", func(c *gin.Context) {
		if !available(c, s) {
			return
		}
		id, ok := monitorID(c)
		if !ok {
			return
		}
		var in UpdateMonitorRequest
		if !body(c, &in) {
			return
		}
		out, err := s.UpdateMonitor(c.Request.Context(), id, in)
		if err != nil {
			apiError(c, err)
			return
		}
		c.JSON(200, out)
	})
	api.POST("/agent/monitors/:id/refresh", func(c *gin.Context) {
		if !available(c, s) {
			return
		}
		id, ok := monitorID(c)
		if !ok {
			return
		}
		var in struct {
			RequestKey string `json:"request_key"`
		}
		if !body(c, &in) {
			return
		}
		out, err := s.RefreshMonitor(c.Request.Context(), id, in.RequestKey)
		if err != nil {
			apiError(c, err)
			return
		}
		c.JSON(202, out)
	})
	api.GET("/agent/inbox", func(c *gin.Context) {
		if !available(c, s) {
			return
		}
		out := []MonitorInbox{}
		if err := s.Data.DB.WithContext(c.Request.Context()).Order("id DESC").Limit(100).Find(&out).Error; err != nil {
			apiError(c, err)
			return
		}
		for n := range out {
			out[n].Verified = out[n].Current(time.Now().UTC())
		}
		c.JSON(200, out)
	})
	api.PATCH("/agent/inbox/:id", func(c *gin.Context) {
		if !available(c, s) {
			return
		}
		id, ok := monitorID(c)
		if !ok {
			return
		}
		var in struct {
			Status        string `json:"status"`
			ObservedRunID string `json:"observed_run_id"`
		}
		if !body(c, &in) {
			return
		}
		if in.Status != "read" && in.Status != "ignored" {
			c.JSON(400, gin.H{"error": "status must be read or ignored"})
			return
		}
		item, err := s.MarkMonitorInbox(c.Request.Context(), id, in.Status, in.ObservedRunID)
		if err != nil {
			apiError(c, err)
			return
		}
		c.JSON(200, item)
	})
}
