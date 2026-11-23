package information

import (
	"encoding/json"
	"fmt"

	"github.com/gin-gonic/gin"
	"go-lua-crawler/internal/agentstatus"
	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/mcpserver"
)

func informationAvailable(c *gin.Context, s *Service) bool {
	if s == nil || s.DB == nil {
		aiapp.WriteAPIError(c, agentstatus.Wrap(agentstatus.SystemDependency, fmt.Errorf("information database is not initialized")))
		return false
	}
	return true
}

func RegisterRoutes(r *gin.Engine, s *Service) {
	monitorAPI := r.Group("/api/v1")
	monitorAPI.Use(aiapp.RequireAdmin)
	registerMonitorRoutes(monitorAPI, s)
	monitorAPI.POST("/knowledge/documents", func(c *gin.Context) {
		if !informationAvailable(c, s) {
			return
		}
		var input DocumentImport
		if !aiapp.DecodeAPIRequest(c, &input) {
			return
		}
		head, err := s.ImportDocument(c.Request.Context(), input)
		if err != nil {
			aiapp.WriteAPIError(c, err)
			return
		}
		c.JSON(202, head)
	})
	api := r.Group("/api/v1/information")
	api.Use(aiapp.RequireAdmin)
	registerSourceAccess(r, api, s)
	registerStudioRoutes(api, s)
	api.POST("/requests", func(c *gin.Context) {
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
		c.JSON(202, gin.H{"request": request})
	})
	api.POST("/deliveries/:id/update", func(c *gin.Context) {
		if !informationAvailable(c, s) {
			return
		}
		var body UpdateInput
		if !aiapp.DecodeAPIRequest(c, &body) {
			return
		}
		request, err := s.CreateUpdate(c.Request.Context(), c.Param("id"), body)
		if err != nil {
			aiapp.WriteAPIError(c, err)
			return
		}
		c.JSON(202, gin.H{"request": request})
	})
	api.GET("/deliveries", func(c *gin.Context) {
		if !informationAvailable(c, s) {
			return
		}
		var query mcpserver.DeliveryQuery
		if err := c.ShouldBindQuery(&query); err != nil {
			aiapp.WriteAPIError(c, err)
			return
		}
		page, err := (sourceBackend{s}).FindDeliveries(c.Request.Context(), query)
		if err != nil {
			aiapp.WriteAPIError(c, err)
			return
		}
		c.JSON(200, page)
	})
	api.GET("/deliveries/:id", func(c *gin.Context) {
		if !informationAvailable(c, s) {
			return
		}
		result, err := (sourceBackend{s}).ReadDelivery(c.Request.Context(), mcpserver.DeliveryID{ID: c.Param("id")})
		if err != nil {
			aiapp.WriteAPIError(c, err)
			return
		}
		c.JSON(200, result)
	})
	api.GET("/requests/:id", func(c *gin.Context) {
		if !informationAvailable(c, s) {
			return
		}
		ctx := c.Request.Context()
		request, err := s.Request(ctx, c.Param("id"))
		if err != nil {
			aiapp.WriteAPIError(c, err)
			return
		}
		var steps []Step
		if err := s.DB.WithContext(ctx).Where("request_id = ?", request.ID).Order("id").Find(&steps).Error; err != nil {
			aiapp.WriteAPIError(c, err)
			return
		}
		var grants []BrowserGrant
		if err := s.DB.WithContext(ctx).Where("request_id = ?", request.ID).Find(&grants).Error; err != nil {
			aiapp.WriteAPIError(c, err)
			return
		}
		accounting, err := s.accountingFromSteps(ctx, request, steps)
		if err != nil {
			aiapp.WriteAPIError(c, err)
			return
		}
		out := gin.H{"request": request, "requirements": validRaw(request.RequirementsJSON), "plan": validRaw(request.PlanJSON), "result": validRaw(request.ResultJSON), "feedback": validRaw(request.FeedbackJSON), "steps": steps, "browser_permissions": grants, "accounting": accounting}
		if request.CurrentDeliveryID != "" {
			view, e := s.deliveryDisplay(ctx, request.CurrentDeliveryID)
			if e != nil {
				aiapp.WriteAPIError(c, e)
				return
			}
			out["result"] = view
		}
		if request.ChildRunID != "" {
			out["crawler_run_id"] = request.ChildRunID
		}
		c.JSON(200, out)
	})
	api.POST("/requests/:id/cancel", func(c *gin.Context) {
		if !informationAvailable(c, s) {
			return
		}
		if err := s.Cancel(c.Request.Context(), c.Param("id")); err != nil {
			aiapp.WriteAPIError(c, err)
			return
		}
		c.JSON(200, gin.H{"status": agentstatus.Of(agentstatus.TaskCancelled)})
	})
	api.GET("/assets", func(c *gin.Context) {
		if !informationAvailable(c, s) {
			return
		}
		if err := s.SyncLuaCatalog(c.Request.Context()); err != nil {
			aiapp.WriteAPIError(c, err)
			return
		}
		var assets []Asset
		if err := s.DB.WithContext(c.Request.Context()).Where("active = ?", true).Order("verified_at DESC").Find(&assets).Error; err != nil {
			aiapp.WriteAPIError(c, err)
			return
		}
		c.JSON(200, gin.H{"assets": assets})
	})
}

var _ json.Marshaler = Request{}
