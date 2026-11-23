package handler

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"gorelay/internal/connector"
	"gorelay/internal/repository"
	taskmodel "gorelay/internal/task"
)

type V2Handler struct {
	Service *connector.Service
}

func (h V2Handler) ListConnectors(c *gin.Context) {
	if h.Service == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "connector service unavailable"})
		return
	}
	page, err := h.Service.ListSources(c.Request.Context(), connector.ListSourcesInput{
		Domain:   c.Query("domain"),
		Keyword:  c.Query("keyword"),
		Kind:     c.Query("kind"),
		Cursor:   c.Query("cursor"),
		PageSize: 50,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, page)
}

func (h V2Handler) MetricsSummary(c *gin.Context) {
	if h.Service == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "connector service unavailable"})
		return
	}
	type row struct {
		HitType       string  `json:"hit_type"`
		Runs          int64   `json:"runs"`
		AvgLatencyMS  float64 `json:"avg_latency_ms"`
		TotalSaved    int64   `json:"total_saved"`
		DriftCount    int64   `json:"drift_count"`
	}
	var grouped []row
	err := h.Service.DB.WithContext(c.Request.Context()).
		Model(&connector.ExecutionMetric{}).
		Select("hit_type, COUNT(*) AS runs, AVG(latency_ms) AS avg_latency_ms, SUM(saved_rows) AS total_saved, SUM(CASE WHEN drift_detected THEN 1 ELSE 0 END) AS drift_count").
		Group("hit_type").
		Scan(&grouped).Error
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	var connectorTotal int64
	_ = h.Service.DB.WithContext(c.Request.Context()).Model(&connector.Connector{}).Count(&connectorTotal).Error
	var degradedTotal int64
	_ = h.Service.DB.WithContext(c.Request.Context()).Model(&connector.Connector{}).Where("status = ?", connector.StatusDegraded).Count(&degradedTotal).Error

	c.JSON(http.StatusOK, gin.H{
		"connectors_total":    connectorTotal,
		"connectors_degraded": degradedTotal,
		"by_hit_type":         grouped,
	})
}

func (h V2Handler) ListDriftEvents(c *gin.Context) {
	if h.Service == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "connector service unavailable"})
		return
	}
	var events []connector.DriftEvent
	if err := h.Service.DB.WithContext(c.Request.Context()).Order("id DESC").Limit(50).Find(&events).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"events": events})
}

func (h V2Handler) GetJob(c *gin.Context) {
	if h.Service == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "connector service unavailable"})
		return
	}
	jobID, err := strconv.ParseUint(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || jobID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid job id"})
		return
	}
	res, err := h.Service.BuildJobResponse(c.Request.Context(), jobID, c.Query("cursor"), "")
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	var events []taskmodel.TaskEvent
	_ = repository.DB.WithContext(c.Request.Context()).Where("task_id = ?", jobID).Order("id ASC").Limit(50).Find(&events).Error
	c.JSON(http.StatusOK, gin.H{"job": res, "events": events})
}

func (h V2Handler) ReadEvidence(c *gin.Context) {
	if h.Service == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "connector service unavailable"})
		return
	}
	ref := strings.TrimSpace(c.Query("ref"))
	query := strings.TrimSpace(c.Query("query"))
	rawSources := strings.TrimSpace(c.Query("source_ids"))
	var sourceIDs []string
	if rawSources != "" {
		for _, part := range strings.Split(rawSources, ",") {
			if p := strings.TrimSpace(part); p != "" {
				sourceIDs = append(sourceIDs, p)
			}
		}
	}
	page, err := h.Service.Pipeline.SearchEvidence(c.Request.Context(), sourceIDs, query, ref, c.Query("cursor"), 10, "")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, page)
}
