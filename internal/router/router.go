package router

import (
	"crypto/subtle"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
	"gorelay/internal/connector"
	"gorelay/internal/handler"
	"gorelay/internal/mcpserver"
)

func SetupRouter(services ...*connector.Service) *gin.Engine {
	var connSvc *connector.Service
	if len(services) > 0 {
		connSvc = services[0]
	}

	r := gin.Default()
	r.StaticFile("/", "./web/index.html")
	r.StaticFile("/evidence", "./web/evidence.html")
	r.GET("/healthz", func(c *gin.Context) { c.Status(200) })

	if connSvc != nil {
		publicBaseURL := strings.TrimSpace(os.Getenv("GORELAY_PUBLIC_BASE_URL"))
		mcpHandler := mcpserver.NewHandler(mcpserver.ConnectorBackend{Service: connSvc}, publicBaseURL)
		r.Any("/mcp", adminAuthMiddleware(), gin.WrapH(mcpHandler))
	}

	h := handler.V2Handler{Service: connSvc}
	v1 := r.Group("/api/v1")
	{
		v1.GET("/connectors", adminAuthMiddleware(), h.ListConnectors)
		v1.GET("/metrics/summary", adminAuthMiddleware(), h.MetricsSummary)
		v1.GET("/drift-events", adminAuthMiddleware(), h.ListDriftEvents)
		v1.GET("/sources/:id/export", adminAuthMiddleware(), h.ExportSource)
		v1.GET("/jobs/:id", adminAuthMiddleware(), h.GetJob)
		v1.GET("/jobs/:id/export", adminAuthMiddleware(), h.ExportJob)
		v1.GET("/evidence", adminAuthMiddleware(), h.ReadEvidence)
	}
	return r
}

func adminAuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		token := strings.TrimSpace(os.Getenv("GORELAY_ADMIN_TOKEN"))
		if token == "" {
			if strings.EqualFold(os.Getenv("GORELAY_ENV"), "production") {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "GORELAY_ADMIN_TOKEN required in production"})
				return
			}
			c.Next()
			return
		}
		got := c.GetHeader("Authorization")
		if subtle.ConstantTimeCompare([]byte(got), []byte("Bearer "+token)) != 1 {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		c.Next()
	}
}
