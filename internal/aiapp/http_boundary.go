package aiapp

import "github.com/gin-gonic/gin"

// Other application frontends share the existing API authentication and error
// contract without adding tools or RAG instructions to the crawler.
func RequireAdmin(c *gin.Context)                   { admin(c) }
func WriteAPIError(c *gin.Context, err error)       { apiError(c, err) }
func DecodeAPIRequest(c *gin.Context, out any) bool { return body(c, out) }
