package aiapp

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gin-gonic/gin"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/agentstatus"
	"go-lua-crawler/internal/knowledge"
	"gorm.io/gorm"
	"net/http"
	"os"
	"strings"
	"time"
)

type AnswerSnapshot struct {
	ID        string    `json:"id" gorm:"primaryKey;size:32"`
	Question  string    `json:"question" gorm:"type:text"`
	Payload   string    `json:"payload" gorm:"type:longtext"`
	CreatedAt time.Time `json:"created_at"`
}

var CoreFunctions = []string{"aiapp.ExecuteReadOnlyCall", "aiapp.RunTaskLoop", "aiapp.RegisterValidatedSource", "knowledge.BuildIndexPlan"}

func admin(c *gin.Context) {
	token := os.Getenv("GORELAY_ADMIN_TOKEN")
	if token == "" {
		c.AbortWithStatusJSON(503, gin.H{"error": "GORELAY_ADMIN_TOKEN not configured", "status": agentstatus.Of(agentstatus.SystemConfiguration)})
		return
	}
	value := strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")
	if subtle.ConstantTimeCompare([]byte(value), []byte(token)) != 1 {
		c.AbortWithStatusJSON(401, gin.H{"error": "admin token required", "status": agentstatus.Of(agentstatus.InvalidRequest)})
	}
}
func apiError(c *gin.Context, err error) {
	status := 400
	internalStatus := OperationalStatus(err)
	if internalStatus.Owner == "business" || internalStatus.Owner == "system" || internalStatus.Owner == "agent" {
		status = internalStatus.HTTPStatus
	}
	if errors.Is(err, agentdata.ErrDailyQuota) {
		status = 429
	}
	if errors.Is(err, ErrModelNotReady) {
		status = http.StatusServiceUnavailable
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		status = 404
	}
	if errors.Is(err, agentdata.ErrConflict) || errors.Is(err, knowledge.ErrIndexConflict) || errors.Is(err, ErrCorePending) || errors.Is(err, ErrContractConfirmation) {
		status = 409
	}
	c.JSON(status, gin.H{"error": err.Error(), "status": internalStatus})
}
func available(c *gin.Context, s *Service) bool {
	if s == nil || s.Data.DB == nil {
		c.JSON(503, gin.H{"error": "database is not initialized", "status": agentstatus.Of(agentstatus.SystemDependency)})
		return false
	}
	return true
}
func body(c *gin.Context, out any) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 3<<20)
	d := json.NewDecoder(c.Request.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		apiError(c, agentstatus.Wrap(agentstatus.InvalidRequest, err, "Correct the request JSON against this endpoint's schema."))
		return false
	}
	return true
}

func RegisterRoutes(r *gin.Engine, s *Service) {
	r.StaticFile("/agent", "./web/agent.html")
	api := r.Group("/api/v1")
	api.GET("/agent/status", func(c *gin.Context) {
		status := gin.H{"core_functions": CoreFunctions, "core_decisions": "implemented", "action_mode": envDefault("GORELAY_ACTION_MODE", "code"), "database": s != nil, "chat": false, "search": false, "runner": false, "retrieval": false}
		if s != nil {
			status["configuration_errors"] = s.ConfigurationErrors
			status["model_readiness"] = s.ModelReadiness()
			status["chat"] = s.Model != nil && s.ModelReadiness().Ready
			status["spend_policy_required"] = s.RequireSpendPolicy
			spend, costErr := spendPolicyFromEnv()
			costReady := !s.RequireSpendPolicy || (costErr == nil && spend != nil)
			status["spend_policy_ready"] = costReady
			if !costReady {
				status["chat"] = false
			}
			status["search"] = s.Search != nil
			status["runner"] = s.Runner != nil
			status["retrieval"] = s.Retriever != nil
		}
		c.JSON(200, status)
	})
	protected := api.Group("")
	protected.Use(admin)
	registerMonitorRoutes(protected, s)
	protected.POST("/agent/runs", func(c *gin.Context) {
		var request CreateRunRequest
		if !body(c, &request) {
			return
		}
		if strings.TrimSpace(request.Goal) == "" || request.RequestKey == "" {
			apiError(c, fmt.Errorf("goal and request_key required"))
			return
		}
		if !available(c, s) {
			return
		}
		out, err := s.Create(c.Request.Context(), request)
		if err != nil {
			apiError(c, err)
			return
		}
		c.JSON(202, out)
	})
	protected.GET("/agent/runs", func(c *gin.Context) {
		if !available(c, s) {
			return
		}
		var rows []agentdata.Run
		err := s.Data.DB.WithContext(c.Request.Context()).Order("created_at DESC").Limit(50).Find(&rows).Error
		if err != nil {
			apiError(c, err)
			return
		}
		c.JSON(200, rows)
	})
	protected.GET("/agent/runs/:id", func(c *gin.Context) {
		if !available(c, s) {
			return
		}
		ctx := c.Request.Context()
		run, err := s.Data.Run(ctx, c.Param("id"))
		if err != nil {
			apiError(c, err)
			return
		}
		children, err := s.ChildResults(ctx, run.ID)
		if err != nil {
			apiError(c, err)
			return
		}
		ops, err := s.Data.Operations(ctx, run.ID)
		if err != nil {
			apiError(c, err)
			return
		}
		trace, err := s.Data.Traces(ctx, run.ID)
		if err != nil {
			apiError(c, err)
			return
		}
		result, resultErr := s.Evaluate(ctx, run)
		loop := BuildLoopReport(trace)
		var traceCount int64
		if err := s.Data.DB.WithContext(ctx).Model(&agentdata.Trace{}).Where("run_id = ?", run.ID).Count(&traceCount).Error; err != nil {
			apiError(c, err)
			return
		}
		loop.HistoryTruncated = traceCount > int64(len(trace))
		accounting, err := s.Accounting(ctx, run)
		if err != nil {
			apiError(c, err)
			return
		}
		progress, err := s.currentProgress(ctx, run)
		if err != nil {
			apiError(c, err)
			return
		}
		c.JSON(200, gin.H{"run": run, "contract": contractView(run), "progress": progress, "children": children, "operations": ops, "traces": trace, "loop": loop, "result": result, "coverage_available": resultErr == nil, "metrics": OperationMetrics(ops, accounting.IndexAttempts...), "accounting": accounting})
	})
	protected.POST("/agent/runs/:id/interventions", func(c *gin.Context) {
		if !available(c, s) {
			return
		}
		var event HumanIntervention
		if !body(c, &event) {
			return
		}
		r, err := s.Data.Run(c.Request.Context(), c.Param("id"))
		if err != nil {
			apiError(c, err)
			return
		}
		out, err := s.RecordIntervention(c.Request.Context(), r, event)
		if err != nil {
			apiError(c, err)
			return
		}
		c.JSON(200, out)
	})
	protected.POST("/agent/runs/:id/contract/reject", func(c *gin.Context) {
		if !available(c, s) {
			return
		}
		var request ContractRejection
		if !body(c, &request) {
			return
		}
		out, err := s.RejectContract(c.Request.Context(), c.Param("id"), request)
		if err != nil {
			apiError(c, err)
			return
		}
		c.JSON(http.StatusOK, out)
	})
	protected.POST("/agent/runs/:id/contract/approve", func(c *gin.Context) {
		if !available(c, s) {
			return
		}
		var request struct {
			ProposalHash string `json:"proposal_hash"`
		}
		if !body(c, &request) {
			return
		}
		out, err := s.ApproveContract(c.Request.Context(), c.Param("id"), request.ProposalHash)
		if err != nil {
			apiError(c, err)
			return
		}
		c.JSON(http.StatusOK, out)
	})
	protected.POST("/agent/runs/:id/cancel", func(c *gin.Context) {
		if !available(c, s) {
			return
		}
		if err := s.CancelWithDelivery(c.Request.Context(), c.Param("id")); err != nil {
			apiError(c, err)
			return
		}
		c.JSON(200, gin.H{"status": "cancelled"})
	})
	protected.POST("/agent/runs/:id/advance", func(c *gin.Context) {
		if !available(c, s) {
			return
		}
		if err := s.Advance(c.Request.Context(), c.Param("id")); err != nil {
			apiError(c, err)
			return
		}
		c.JSON(200, gin.H{"status": "advanced"})
	})
	protected.POST("/agent/runs/:id/resume", func(c *gin.Context) {
		if !available(c, s) {
			return
		}
		q := s.Data.DB.WithContext(c.Request.Context()).Model(&agentdata.Run{}).Where("id = ? AND status IN ? AND cancelled = ? AND (deadline IS NULL OR deadline > ?) AND (lease_until IS NULL OR lease_until <= ?)", c.Param("id"), []string{"blocked_core", "blocked_configuration", "needs_input"}, false, time.Now(), time.Now()).Updates(map[string]any{"status": "planning", "next_wake_at": nil, "last_error": ""})
		if q.Error != nil {
			apiError(c, q.Error)
			return
		}
		if q.RowsAffected != 1 {
			apiError(c, agentdata.ErrConflict)
			return
		}
		c.JSON(200, gin.H{"status": "planning", "budget_reset": false})
	})
	protected.GET("/agent/sources", func(c *gin.Context) {
		if !available(c, s) {
			return
		}
		var heads []agentdata.SourceHead
		var versions []agentdata.SourceVersion
		var trials []Candidate
		ctx := c.Request.Context()
		for _, q := range []*gorm.DB{s.Data.DB.WithContext(ctx).Limit(100).Find(&heads), s.Data.DB.WithContext(ctx).Order("id DESC").Limit(100).Find(&versions), s.Data.DB.WithContext(ctx).Order("created_at DESC").Limit(50).Find(&trials)} {
			if q.Error != nil {
				apiError(c, q.Error)
				return
			}
		}
		legacy, legacyErr := legacyCatalog()
		note := ""
		if legacyErr != nil {
			note = legacyErr.Error()
		}
		c.JSON(200, gin.H{"heads": heads, "versions": versions, "trials": trials, "legacy": legacy, "legacy_catalog_error": note})
	})
	protected.POST("/agent/sources/:id/rollback", func(c *gin.Context) {
		var a struct {
			Version string `json:"version"`
		}
		if !body(c, &a) {
			return
		}
		if a.Version == "" {
			apiError(c, fmt.Errorf("target version required"))
			return
		}
		if !available(c, s) {
			return
		}
		out, err := s.RollbackSource(c.Request.Context(), c.Param("id"), a.Version)
		if err != nil {
			apiError(c, err)
			return
		}
		c.JSON(200, out)
	})
	protected.POST("/agent/sources/:id/quarantine", func(c *gin.Context) {
		var a struct {
			Version string `json:"version"`
			Reason  string `json:"reason"`
		}
		if !body(c, &a) {
			return
		}
		if a.Version == "" || a.Reason == "" {
			apiError(c, fmt.Errorf("version and reason required"))
			return
		}
		if !available(c, s) {
			return
		}
		err := s.Data.DB.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
			if err := agentdata.NewStore(tx).Quarantine(c.Request.Context(), c.Param("id"), a.Version, a.Reason); err != nil {
				return err
			}
			var heads []knowledge.Head
			if err := tx.Where("source = ? AND active = ?", c.Param("id"), true).Find(&heads).Error; err != nil {
				return err
			}
			for _, h := range heads {
				if err := knowledge.NewStore(tx).Invalidate(c.Request.Context(), h.ID); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			apiError(c, err)
			return
		}
		c.JSON(200, gin.H{"status": "quarantined", "affected_knowledge": "inactive"})
	})
	protected.GET("/knowledge/documents", func(c *gin.Context) {
		if !available(c, s) {
			return
		}
		rows, err := s.Knowledge.Heads(c.Request.Context())
		if err != nil {
			apiError(c, err)
			return
		}
		c.JSON(200, rows)
	})
	protected.POST("/knowledge/documents", func(c *gin.Context) {
		var input knowledge.Input
		if !body(c, &input) {
			return
		}
		doc, err := knowledge.NewDocument(input)
		if err != nil {
			apiError(c, err)
			return
		}
		if !available(c, s) {
			return
		}
		head, err := s.Knowledge.Put(c.Request.Context(), doc, s.Fingerprint)
		if err != nil {
			apiError(c, err)
			return
		}
		c.JSON(202, head)
	})
	protected.POST("/knowledge/documents/:id/invalidate", func(c *gin.Context) {
		if !available(c, s) {
			return
		}
		if err := s.Knowledge.Invalidate(c.Request.Context(), c.Param("id")); err != nil {
			apiError(c, err)
			return
		}
		c.JSON(200, gin.H{"status": "inactive"})
	})
	protected.GET("/knowledge/index-work", func(c *gin.Context) {
		if !available(c, s) {
			return
		}
		rows, err := s.Knowledge.Works(c.Request.Context())
		if err != nil {
			apiError(c, err)
			return
		}
		c.JSON(200, rows)
	})
	protected.GET("/knowledge/index-attempts", func(c *gin.Context) {
		if !available(c, s) {
			return
		}
		rows, err := s.Knowledge.IndexAttempts(c.Request.Context(), c.Query("doc_id"))
		if err != nil {
			apiError(c, err)
			return
		}
		c.JSON(200, rows)
	})
	protected.POST("/knowledge/documents/:id/retry", func(c *gin.Context) {
		if !available(c, s) {
			return
		}
		if err := s.Knowledge.Retry(c.Request.Context(), c.Param("id")); err != nil {
			apiError(c, err)
			return
		}
		c.JSON(202, gin.H{"status": "pending"})
	})
	protected.POST("/knowledge/query", func(c *gin.Context) {
		var a struct {
			Question string `json:"question"`
		}
		if !body(c, &a) {
			return
		}
		if strings.TrimSpace(a.Question) == "" || len(a.Question) > 4000 {
			apiError(c, fmt.Errorf("question required, max 4000 bytes"))
			return
		}
		if !available(c, s) {
			return
		}
		if s.Retriever == nil || s.Model == nil {
			c.JSON(503, gin.H{"error": "embedding/vector/chat providers not configured"})
			return
		}
		set, err := s.Retriever.Retrieve(c.Request.Context(), a.Question, 8)
		if err != nil {
			apiError(c, err)
			return
		}
		answer, err := (RAGResponder{Model: s.Model, Store: s.Knowledge}).Answer(c.Request.Context(), a.Question, set)
		if err != nil {
			apiError(c, err)
			return
		}
		raw, _ := json.Marshal(answer)
		row := AnswerSnapshot{ID: set.RequestID, Question: a.Question, Payload: string(raw)}
		if err = s.Data.DB.WithContext(c.Request.Context()).Create(&row).Error; err != nil {
			apiError(c, err)
			return
		}
		c.JSON(200, gin.H{"snapshot_id": row.ID, "result": answer})
	})
	protected.GET("/knowledge/answers/:id", func(c *gin.Context) {
		if !available(c, s) {
			return
		}
		var row AnswerSnapshot
		if err := s.Data.DB.WithContext(c.Request.Context()).First(&row, "id = ?", c.Param("id")).Error; err != nil {
			apiError(c, err)
			return
		}
		c.JSON(200, row)
	})
}

// Unknown provider usage stays unknown. Reservations are displayed separately
// from measured tokens/latency, never represented as a paid-call cost of zero.
func OperationMetrics(ops []agentdata.Operation, indexAttempts ...knowledge.IndexAttempt) map[string]any {
	var tokens, prompt, completion int64
	known, unknown := 0, 0
	latencies := []int64{}
	byStage := map[string]*StageUsage{}
	addUsage := func(stage string, usage *Usage) {
		row := byStage[stage]
		if row == nil {
			row = &StageUsage{}
			byStage[stage] = row
		}
		if usage == nil {
			unknown++
			row.UnknownAttempts++
			return
		}
		known++
		row.KnownAttempts++
		row.ReportedTokens += usage.TotalTokens
		row.CachedPromptTokens += usage.Cached()
		tokens += usage.TotalTokens
		prompt += usage.PromptTokens
		completion += usage.CompletionTokens
	}
	for _, op := range ops {
		var usage *Usage
		switch op.Kind {
		case "model":
			_ = json.Unmarshal([]byte(op.UsageJSON), &usage)
			addUsage("decision", usage)
		case "discover_sources":
			var receipt ToolResult
			var response SearchResponse
			_ = json.Unmarshal([]byte(op.ResultJSON), &receipt)
			_ = json.Unmarshal(receipt.Data, &response)
			addUsage("search", response.Usage)
		case "search_knowledge":
			var receipt ToolResult
			var response knowledge.EvidenceSet
			_ = json.Unmarshal([]byte(op.ResultJSON), &receipt)
			_ = json.Unmarshal(receipt.Data, &response)
			if response.Metrics.Usage != nil {
				usage = &Usage{PromptTokens: response.Metrics.Usage.PromptTokens, TotalTokens: response.Metrics.Usage.TotalTokens}
			}
			addUsage("retrieval_embedding", usage)
		default:
			continue
		}
		var response ChatResponse
		if op.Kind == "model" && json.Unmarshal([]byte(op.ResultJSON), &response) == nil && response.DurationMS > 0 {
			latencies = append(latencies, response.DurationMS)
		}
	}
	for _, attempt := range indexAttempts {
		var metrics knowledge.IndexMetrics
		if json.Unmarshal([]byte(attempt.MetricsJSON), &metrics) != nil {
			continue
		}
		if metrics.EmbeddingCalls == 0 {
			continue
		}
		var usage *Usage
		if metrics.Usage != nil {
			usage = &Usage{PromptTokens: metrics.Usage.PromptTokens, TotalTokens: metrics.Usage.TotalTokens}
		}
		addUsage("index_embedding", usage)
	}
	return map[string]any{"reported_total_tokens": tokens, "token_scope": "known reported usage across decision/search/retrieval/index; not monetary cost", "usage_by_stage": byStage, "reported_prompt_tokens": prompt, "reported_completion_tokens": completion, "usage_known_attempts": known, "usage_unknown_attempts": unknown, "model_latency_ms_samples": latencies, "p95_ready": len(latencies) >= 20}
}

type StageUsage struct {
	KnownAttempts      int   `json:"known_attempts"`
	UnknownAttempts    int   `json:"unknown_attempts"`
	ReportedTokens     int64 `json:"reported_tokens"`
	CachedPromptTokens int64 `json:"cached_prompt_tokens"`
}
