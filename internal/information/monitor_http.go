package information

import (
	"encoding/json"
	"strconv"

	"github.com/gin-gonic/gin"
	"go-lua-crawler/internal/aiapp"
)

func monitorID(c *gin.Context) (uint64, bool) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		aiapp.WriteAPIError(c, businessError("MONITOR_ID_REQUIRED", "monitor", "监测编号无效", "从持续监测列表选择实际条目"))
		return 0, false
	}
	return id, true
}

func (s *Service) monitorView(c *gin.Context, id uint64) (map[string]any, error) {
	var monitor Monitor
	if err := s.DB.WithContext(c.Request.Context()).First(&monitor, id).Error; err != nil {
		return nil, err
	}
	config, err := monitor.Config()
	if err != nil {
		return nil, err
	}
	var checks []MonitorCheck
	if err := s.DB.WithContext(c.Request.Context()).Where("monitor_id = ?", id).Order("id DESC").Find(&checks).Error; err != nil {
		return nil, err
	}
	raw, err := json.Marshal(monitor)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	out["config"], out["checks"] = config, checks
	return out, nil
}

func registerMonitorRoutes(api *gin.RouterGroup, s *Service) {
	api.POST("/agent/monitors", func(c *gin.Context) {
		if !informationAvailable(c, s) {
			return
		}
		var config MonitorConfig
		if !aiapp.DecodeAPIRequest(c, &config) {
			return
		}
		result, err := s.CreateMonitor(c.Request.Context(), config)
		if err != nil {
			aiapp.WriteAPIError(c, err)
			return
		}
		c.JSON(201, result)
	})
	api.GET("/agent/monitors", func(c *gin.Context) {
		if !informationAvailable(c, s) {
			return
		}
		var rows []Monitor
		if err := s.DB.WithContext(c.Request.Context()).Order("id DESC").Find(&rows).Error; err != nil {
			aiapp.WriteAPIError(c, err)
			return
		}
		c.JSON(200, rows)
	})
	api.GET("/agent/monitors/:id", func(c *gin.Context) {
		if !informationAvailable(c, s) {
			return
		}
		id, ok := monitorID(c)
		if !ok {
			return
		}
		view, err := s.monitorView(c, id)
		if err != nil {
			aiapp.WriteAPIError(c, err)
			return
		}
		c.JSON(200, view)
	})
	api.PATCH("/agent/monitors/:id", func(c *gin.Context) {
		if !informationAvailable(c, s) {
			return
		}
		id, ok := monitorID(c)
		if !ok {
			return
		}
		var input MonitorUpdate
		if !aiapp.DecodeAPIRequest(c, &input) {
			return
		}
		result, err := s.UpdateMonitor(c.Request.Context(), id, input)
		if err != nil {
			aiapp.WriteAPIError(c, err)
			return
		}
		c.JSON(200, result)
	})
	api.POST("/agent/monitors/:id/refresh", func(c *gin.Context) {
		if !informationAvailable(c, s) {
			return
		}
		id, ok := monitorID(c)
		if !ok {
			return
		}
		var input struct {
			RequestKey string `json:"request_key"`
		}
		if !aiapp.DecodeAPIRequest(c, &input) {
			return
		}
		request, err := s.RefreshMonitor(c.Request.Context(), id, input.RequestKey)
		if err != nil {
			aiapp.WriteAPIError(c, err)
			return
		}
		c.JSON(202, request)
	})
	api.GET("/agent/inbox", func(c *gin.Context) {
		if !informationAvailable(c, s) {
			return
		}
		var input struct {
			MonitorID uint64 `form:"monitor_id"`
			Offset    int    `form:"offset"`
			Limit     int    `form:"limit"`
		}
		if err := c.ShouldBindQuery(&input); err != nil {
			aiapp.WriteAPIError(c, err)
			return
		}
		if input.Offset < 0 || input.Limit < 0 {
			aiapp.WriteAPIError(c, businessError("INVALID_MONITOR_PAGE", "monitor", "offset 和 limit 不能为负数", "从 offset=0 开始并按 next_offset 继续"))
			return
		}
		if input.Limit == 0 {
			input.Limit = 20
		}
		query := s.DB.WithContext(c.Request.Context()).Model(&MonitorInbox{})
		if input.MonitorID != 0 {
			query = query.Where("monitor_id = ?", input.MonitorID)
		}
		var total int64
		if err := query.Count(&total).Error; err != nil {
			aiapp.WriteAPIError(c, err)
			return
		}
		var rows []MonitorInbox
		if err := query.Order("id DESC").Offset(input.Offset).Limit(input.Limit).Find(&rows).Error; err != nil {
			aiapp.WriteAPIError(c, err)
			return
		}
		items := []map[string]any{}
		for _, row := range rows {
			raw, err := json.Marshal(row)
			if err != nil {
				aiapp.WriteAPIError(c, err)
				return
			}
			item := map[string]any{}
			if err := json.Unmarshal(raw, &item); err != nil {
				aiapp.WriteAPIError(c, err)
				return
			}
			item["comparison"] = validRaw(row.ComparisonJSON)
			items = append(items, item)
		}
		out := gin.H{"items": items, "total": total}
		if next := input.Offset + len(rows); int64(next) < total {
			out["next_offset"] = next
		}
		c.JSON(200, out)
	})
	api.PATCH("/agent/inbox/:id", func(c *gin.Context) {
		if !informationAvailable(c, s) {
			return
		}
		id, ok := monitorID(c)
		if !ok {
			return
		}
		var input struct {
			Status string `json:"status"`
		}
		if !aiapp.DecodeAPIRequest(c, &input) {
			return
		}
		if input.Status != "read" && input.Status != "ignored" && input.Status != "unread" {
			aiapp.WriteAPIError(c, businessError("INVALID_INBOX_STATUS", "monitor", "状态只能为 read、ignored 或 unread", "选择已读、忽略或未读"))
			return
		}
		updated := s.DB.WithContext(c.Request.Context()).Model(&MonitorInbox{}).Where("id = ?", id).Update("status", input.Status)
		if updated.Error != nil {
			aiapp.WriteAPIError(c, updated.Error)
			return
		}
		if updated.RowsAffected == 0 {
			var item MonitorInbox
			if err := s.DB.WithContext(c.Request.Context()).First(&item, id).Error; err != nil {
				aiapp.WriteAPIError(c, err)
				return
			}
		}
		c.JSON(200, gin.H{"status": input.Status})
	})
}
