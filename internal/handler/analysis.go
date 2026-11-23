package handler

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gorelay/internal/repository"

	"github.com/gin-gonic/gin"
)

const defaultAnalysisSnapshotLimit = 50

// ListAnalysisSnapshots returns ended task records for browser-side analysis.
func ListAnalysisSnapshots(c *gin.Context) {
	from, to, limit, err := parseAnalysisQuery(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": http.StatusBadRequest, "message": err.Error()})
		return
	}
	records, err := repository.ListEndedTaskSnapshots(c.Request.Context(), c.Query("rule_id"), from, to, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": http.StatusInternalServerError, "message": "list analysis snapshots failed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": http.StatusOK, "data": records})
}

// ListAnalysisRules returns the metadata used by the frontend to interpret
// different Lua result shapes without hard-coding a website in JavaScript.
func ListAnalysisRules(c *gin.Context) {
	rules, err := repository.ListRuleDefinitions()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": http.StatusInternalServerError, "message": "list analysis rules failed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": http.StatusOK, "data": rules})
}

func parseAnalysisQuery(c *gin.Context) (from *time.Time, to *time.Time, limit int, err error) {
	from, err = parseOptionalRFC3339(c.Query("from"), "from")
	if err != nil {
		return nil, nil, 0, err
	}
	to, err = parseOptionalRFC3339(c.Query("to"), "to")
	if err != nil {
		return nil, nil, 0, err
	}
	if from != nil && to != nil && from.After(*to) {
		return nil, nil, 0, fmt.Errorf("from must not be after to")
	}

	limit = defaultAnalysisSnapshotLimit
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit <= 0 {
			return nil, nil, 0, fmt.Errorf("limit must be a positive integer")
		}
	}
	return from, to, limit, nil
}

func parseOptionalRFC3339(raw, name string) (*time.Time, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, fmt.Errorf("%s must use RFC3339 format", name)
	}
	return &parsed, nil
}
