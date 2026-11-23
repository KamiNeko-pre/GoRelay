package information

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/mcpserver"
)

func registerSourceWorkspace(api *gin.RouterGroup, backend sourceBackend) {
	respond := func(c *gin.Context, out any, err error) {
		if err != nil {
			aiapp.WriteAPIError(c, err)
			return
		}
		c.JSON(http.StatusOK, out)
	}
	api.GET("/source-jobs", func(c *gin.Context) {
		size := 0
		if raw := c.Query("page_size"); raw != "" {
			var err error
			size, err = strconv.Atoi(raw)
			if err != nil {
				respond(c, nil, fmt.Errorf("page_size must be an integer"))
				return
			}
		}
		out, err := backend.listSourceJobs(c.Request.Context(), c.Query("source_id"), c.Query("cursor"), size)
		respond(c, out, err)
	})
	api.GET("/source-jobs/:job_id", func(c *gin.Context) {
		out, err := backend.GetJob(c.Request.Context(), mcpserver.JobInput{JobID: c.Param("job_id")})
		if err == nil {
			err = backend.decorateSourceJob(c.Request.Context(), &out)
		}
		respond(c, out, err)
	})
	api.POST("/source-jobs/:job_id/cancel", func(c *gin.Context) {
		out, err := backend.CancelJob(c.Request.Context(), mcpserver.JobInput{JobID: c.Param("job_id")})
		respond(c, out, err)
	})
	api.GET("/source-data", func(c *gin.Context) {
		var in mcpserver.DataInput
		if err := c.ShouldBindQuery(&in); err != nil {
			respond(c, nil, err)
			return
		}
		out, err := backend.ReadData(c.Request.Context(), in)
		if err == nil {
			evidenceAccess(c)
		}
		respond(c, out, err)
	})
	api.GET("/source-documents", func(c *gin.Context) {
		var in mcpserver.DataInput
		if err := c.ShouldBindQuery(&in); err != nil {
			respond(c, nil, err)
			return
		}
		out, err := backend.sourceDocuments(c.Request.Context(), in)
		if err == nil {
			evidenceAccess(c)
		}
		respond(c, out, err)
	})
	api.POST("/source-refreshes", func(c *gin.Context) {
		var in sourceRefreshInput
		if !aiapp.DecodeAPIRequest(c, &in) {
			return
		}
		out, err := backend.refreshSource(c.Request.Context(), in)
		respond(c, out, err)
	})
	api.GET("/source-differences", func(c *gin.Context) {
		if c.Query("before") == "" || c.Query("after") == "" {
			respond(c, nil, fmt.Errorf("before and after job IDs are required; choose two saved acquisitions"))
			return
		}
		out, err := backend.sourceDifference(c.Request.Context(), c.Query("before"), c.Query("after"))
		respond(c, out, err)
	})
}
