package information

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"

	"github.com/gin-gonic/gin"
	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/knowledge"
	"go-lua-crawler/internal/mcpserver"
)

func registerSourceAccess(r *gin.Engine, api *gin.RouterGroup, service *Service) {
	backend := sourceBackend{service: service}
	registerSourceWorkspace(api, backend)
	r.Any("/mcp", aiapp.RequireAdmin, gin.WrapH(mcpserver.NewHandler(backend, os.Getenv("GORELAY_PUBLIC_BASE_URL"))))
	r.StaticFile("/evidence", "./web/evidence.html")
	api.GET("/sources", func(c *gin.Context) {
		var in mcpserver.SourceQuery
		if err := c.ShouldBindQuery(&in); err != nil {
			aiapp.WriteAPIError(c, err)
			return
		}
		out, err := backend.FindSources(c.Request.Context(), in)
		if err != nil {
			aiapp.WriteAPIError(c, err)
			return
		}
		c.JSON(http.StatusOK, out)
	})
	api.POST("/evidence/search", func(c *gin.Context) {
		var in mcpserver.EvidenceInput
		if !aiapp.DecodeAPIRequest(c, &in) {
			return
		}
		out, err := backend.SearchEvidence(c.Request.Context(), in)
		if err != nil {
			aiapp.WriteAPIError(c, err)
			return
		}
		c.JSON(http.StatusOK, out)
	})
	// Citation tabs share a read-only browser session. The ordinary admin token
	// remains required for MCP calls and all write endpoints.
	r.GET("/api/v1/information/evidence/:id", evidenceAccess, func(c *gin.Context) {
		if !informationAvailable(c, service) {
			return
		}
		store := knowledge.NewStore(service.DB)
		id := c.Param("id")
		if revision := c.Query("revision"); revision != "" {
			page, err := store.ReadEvidenceDocument(c.Request.Context(), id, revision, "", 1)
			if err != nil {
				aiapp.WriteAPIError(c, err)
				return
			}
			if len(page.Entries) == 0 {
				c.JSON(http.StatusNotFound, gin.H{"error": "saved document has no readable text"})
				return
			}
			id = page.Entries[0].ID
		}
		passage, err := store.ReadEvidenceSnapshot(c.Request.Context(), id)
		if err != nil {
			aiapp.WriteAPIError(c, err)
			return
		}
		doc, err := store.Document(c.Request.Context(), passage.DocID, passage.Revision)
		if err != nil {
			aiapp.WriteAPIError(c, err)
			return
		}
		passage.Title = savedDocumentTitle(knowledge.Head{Title: doc.Title, RecordKey: doc.RecordKey, Source: doc.Source, URL: doc.URL})
		c.Header("Cache-Control", "private, no-store")
		c.JSON(http.StatusOK, gin.H{"evidence": passage, "body": doc.Body})
	})
}

func evidenceAccess(c *gin.Context) {
	token := os.Getenv("GORELAY_ADMIN_TOKEN")
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write([]byte("gorelay-evidence-read-v1"))
	value := hex.EncodeToString(mac.Sum(nil))
	cookie, err := c.Cookie("gorelay-evidence-read")
	if token != "" && err == nil && hmac.Equal([]byte(cookie), []byte(value)) {
		return
	}
	aiapp.RequireAdmin(c)
	if !c.IsAborted() {
		c.SetSameSite(http.SameSiteStrictMode)
		c.SetCookie("gorelay-evidence-read", value, 0, "/api/v1/information/evidence/", "", c.Request.TLS != nil, true)
	}
}
