package router

import (
	"github.com/gin-gonic/gin"
	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/handler"
	"go-lua-crawler/internal/information"
)

func SetupRouter(services ...*aiapp.Service) *gin.Engine {
	var ai *aiapp.Service
	if len(services) > 0 {
		ai = services[0]
	}
	var info *information.Service
	if ai != nil {
		info = information.NewService(ai)
		if err := info.Configure(); err != nil {
			info = nil
		}
	}
	return SetupRouterWithInformation(ai, info)
}

func SetupRouterWithInformation(ai *aiapp.Service, info *information.Service) *gin.Engine {
	// Gin 默认路由器已经包含日志和 panic recovery 中间件。
	r := gin.Default()
	// 生产服务直接托管分析工作台；API 仍然使用 /api/v1 前缀，
	// 因此前端不会需要单独启动一个开发服务器。
	r.Static("/assets", "./web/assets")
	r.StaticFile("/", "./web/index.html")
	r.GET("/healthz", func(c *gin.Context) { c.Status(200) })
	aiapp.RegisterRoutes(r, ai)
	information.RegisterRoutes(r, info)

	// 统一使用版本化 API 前缀，为以后增加 v2 预留兼容空间。
	v1 := r.Group("/api/v1")
	{
		v1.POST("/tasks", handler.CreateCrawlTask)
		v1.GET("/tasks/:id", handler.GetCrawlTask)
		v1.GET("/tasks/:id/result", handler.GetCrawlTaskResult)
		v1.GET("/tasks/:id/events", handler.ListTaskEvents)
		v1.GET("/analysis/snapshots", handler.ListAnalysisSnapshots)
		v1.GET("/analysis/rules", handler.ListAnalysisRules)

	}
	return r
}
