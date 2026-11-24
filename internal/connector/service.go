package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"gorelay/internal/acquisition"
	"gorelay/internal/contract"
	"gorelay/internal/engine"
	"gorelay/internal/fetchgov"
	"gorelay/internal/pipeline"
	"gorelay/internal/probe"
	"gorelay/internal/programrunner"
	"gorelay/internal/repository"
	taskmodel "gorelay/internal/task"
	"gorelay/internal/worker"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type ServiceConfig struct {
	RunnerURL          string
	RunnerToken        string
	CheckpointBaseURL  string // Internal worker callback base URL for remote lua-runner containers.
	RespectRobots      bool
	SnapshotDir        string
}

type Service struct {
	DB          *gorm.DB
	Cfg         ServiceConfig
	SafeClient  *fetchgov.SafeClient
	Prober      *probe.Prober
	Pipeline    *pipeline.Store
	Runner      *programrunner.RunnerClient
	Hub         *CheckpointHub
	// InlineExec optionally simulates publisher+worker delivery synchronously in tests/single-process mode.
	InlineExec func(context.Context, uint64) error
	luaLimits  engine.ProgramLimits
	canaryMu   sync.Mutex
	canaryStop context.CancelFunc
}

func Migrate(db *gorm.DB) error {
	return db.AutoMigrate(&Connector{}, &ConnectorVersion{}, &DriftEvent{}, &ExecutionMetric{})
}

func NewService(db *gorm.DB, gov *fetchgov.SafeClient, pipe *pipeline.Store, cfg ServiceConfig) (*Service, error) {
	if db == nil {
		return nil, fmt.Errorf("connector DB is required")
	}
	if gov == nil {
		var err error
		gcfg := fetchgov.DefaultConfig()
		gcfg.RespectRobots = cfg.RespectRobots
		gov, err = fetchgov.NewSafeClient(gcfg)
		if err != nil {
			return nil, err
		}
	}
	if pipe == nil {
		pipe = pipeline.NewStore(db, cfg.SnapshotDir)
	}
	var rc *programrunner.RunnerClient
	if cfg.RunnerURL != "" && cfg.RunnerToken != "" {
		var err error
		rc, err = programrunner.NewRunnerClient(cfg.RunnerURL, cfg.RunnerToken)
		if err != nil {
			return nil, err
		}
	}
	return &Service{
		DB:         db,
		Cfg:        cfg,
		SafeClient: gov,
		Prober:     probe.NewProber(gov),
		Pipeline:   pipe,
		Runner:     rc,
		Hub:        NewCheckpointHub(),
		luaLimits: engine.ProgramLimits{
			Timeout:        45 * time.Second,
			MaxRequests:    30,
			MaxBytes:       int64(fetchgov.DefaultMaxBodyBytes),
			MaxItems:       1000,
			MaxOutputBytes: 4 * 1024 * 1024,
		},
	}, nil
}

type ListSourcesInput struct {
	Domain          string
	URL             string
	Keyword         string
	Kind            string
	FreshnessTTLSec int
	Cursor          string
	PageSize        int
}

type ConnectorView struct {
	SourceID            string     `json:"source_id"`
	Kind                string     `json:"kind"`
	Executor            string     `json:"executor"`
	Domain              string     `json:"domain"`
	URLPattern          string     `json:"url_pattern"`
	Description         string     `json:"description"`
	ActiveVersion       int        `json:"active_version"`
	Status              string     `json:"status"`
	ReuseCount          int        `json:"reuse_count"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	LastVerifiedAt      *time.Time `json:"last_verified_at,omitempty"`
	LastDriftReason     string     `json:"last_drift_reason,omitempty"`
	FreshCacheAvailable bool       `json:"fresh_cache_available,omitempty"`
	CachedRecords       int64      `json:"cached_records,omitempty"`
	InputSchema         any        `json:"input_schema,omitempty"`
	OutputSchema        any        `json:"output_schema,omitempty"`
}

type ListSourcesPage struct {
	Connectors []ConnectorView `json:"connectors"`
	Total      int64           `json:"total"`
	NextCursor string          `json:"next_cursor,omitempty"`
}

func (s *Service) ListSources(ctx context.Context, in ListSourcesInput) (ListSourcesPage, error) {
	if in.PageSize <= 0 || in.PageSize > 50 {
		in.PageSize = 20
	}
	q := s.DB.WithContext(ctx).Model(&Connector{})
	domain := strings.ToLower(strings.TrimSpace(in.Domain))
	if domain == "" && strings.TrimSpace(in.URL) != "" {
		if u, err := url.Parse(in.URL); err == nil {
			domain = strings.ToLower(u.Hostname())
		}
	}
	if domain != "" {
		q = q.Where("domain = ?", domain)
	}
	if strings.TrimSpace(in.Kind) != "" {
		q = q.Where("kind = ?", strings.TrimSpace(in.Kind))
	}
	if kw := strings.TrimSpace(in.Keyword); kw != "" {
		like := "%" + kw + "%"
		q = q.Where("(source_id LIKE ? OR description LIKE ? OR url_pattern LIKE ?)", like, like, like)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return ListSourcesPage{}, err
	}
	offset := 0
	if in.Cursor != "" {
		if v, err := strconv.Atoi(in.Cursor); err == nil && v >= 0 {
			offset = v
		}
	}
	var rows []Connector
	if err := q.Order("reuse_count DESC, updated_at DESC").Offset(offset).Limit(in.PageSize).Find(&rows).Error; err != nil {
		return ListSourcesPage{}, err
	}

	now := time.Now().UTC()
	cachedCounts := make(map[string]int64, len(rows))
	activeVersions := make(map[string]ConnectorVersion, len(rows))
	if len(rows) > 0 {
		ids := make([]string, 0, len(rows))
		wantedVer := make(map[string]int, len(rows))
		for _, c := range rows {
			ids = append(ids, c.SourceID)
			wantedVer[c.SourceID] = c.ActiveVersion
		}
		type countRow struct {
			SourceID string
			Cnt      int64
		}
		var counts []countRow
		if err := s.DB.WithContext(ctx).Model(&pipeline.SourceRecord{}).
			Select("source_id, COUNT(*) AS cnt").
			Where("source_id IN ? AND is_current = ?", ids, true).
			Group("source_id").Scan(&counts).Error; err == nil {
			for _, cr := range counts {
				cachedCounts[cr.SourceID] = cr.Cnt
			}
		}
		var vers []ConnectorVersion
		if err := s.DB.WithContext(ctx).
			Select("source_id, version, input_schema, output_schema").
			Where("source_id IN ?", ids).Find(&vers).Error; err == nil {
			for _, v := range vers {
				if wantedVer[v.SourceID] == v.Version {
					activeVersions[v.SourceID] = v
				}
			}
		}
	}
	out := ListSourcesPage{Connectors: make([]ConnectorView, 0, len(rows)), Total: total}
	for _, c := range rows {
		view := ConnectorView{
			SourceID:            c.SourceID,
			Kind:                c.Kind,
			Executor:            c.Executor,
			Domain:              c.Domain,
			URLPattern:          c.URLPattern,
			Description:         c.Description,
			ActiveVersion:       c.ActiveVersion,
			Status:              c.Status,
			ReuseCount:          c.ReuseCount,
			ConsecutiveFailures: c.ConsecutiveFailures,
			LastVerifiedAt:      c.LastVerifiedAt,
			LastDriftReason:     c.LastDriftReason,
			CachedRecords:       cachedCounts[c.SourceID],
		}
		if in.FreshnessTTLSec > 0 && c.Status == StatusActive && c.LastVerifiedAt != nil {
			if now.Sub(*c.LastVerifiedAt) <= time.Duration(in.FreshnessTTLSec)*time.Second {
				view.FreshCacheAvailable = true
			}
		}
		if ver, ok := activeVersions[c.SourceID]; ok {
			if ver.InputSchema != "" {
				var m any
				if json.Unmarshal([]byte(ver.InputSchema), &m) == nil {
					view.InputSchema = m
				}
			}
			if ver.OutputSchema != "" {
				var m any
				if json.Unmarshal([]byte(ver.OutputSchema), &m) == nil {
					view.OutputSchema = m
				}
			}
		}
		out.Connectors = append(out.Connectors, view)
	}
	if int64(offset+len(rows)) < total {
		out.NextCursor = strconv.Itoa(offset + len(rows))
	}
	return out, nil
}

type DeployAndRunInput struct {
	SourceID     string          `json:"source_id"`
	Kind         string          `json:"kind"`     // data | discovery
	Executor     string          `json:"executor"` // http_api | lua_sandbox
	Description  string          `json:"description"`
	TargetURL    string          `json:"target_url"`
	SpecOrCode   string          `json:"spec_or_code"`
	InputSchema  json.RawMessage `json:"input_schema,omitempty"`
	OutputSchema json.RawMessage `json:"output_schema"`
	Params       map[string]any        `json:"params,omitempty"`
	AuthProfile  string                `json:"auth_profile,omitempty"`
	WaitMS       int                              `json:"wait_ms,omitempty"`
	RollbackTo   int                              `json:"rollback_to,omitempty"`
	Query        *pipeline.RecordQuery            `json:"query,omitempty"`
	Queries      map[string]*pipeline.RecordQuery `json:"queries,omitempty"`
}

type RunConnectorInput struct {
	SourceID        string                           `json:"source_id"`
	TargetURL       string                           `json:"target_url,omitempty"`
	Params          map[string]any                   `json:"params,omitempty"`
	FreshnessTTLSec int                              `json:"freshness_ttl,omitempty"`
	Cursor          string                           `json:"cursor,omitempty"`
	WaitMS          int                              `json:"wait_ms,omitempty"`
	ForceRefresh    bool                             `json:"force_refresh,omitempty"`
	Query           *pipeline.RecordQuery            `json:"query,omitempty"`
	Queries         map[string]*pipeline.RecordQuery `json:"queries,omitempty"`
}

type ExecutionResponse struct {
	JobID            uint64                   `json:"job_id,omitempty"`
	SourceID         string                   `json:"source_id"`
	Version          int                      `json:"version,omitempty"`
	ActiveVersion    int                      `json:"active_version,omitempty"`
	Executor         string                   `json:"executor,omitempty"`
	Status           string                   `json:"status"` // succeeded | queued | running | failed | cancelled | degraded | cache_hit
	HitType          string                   `json:"hit_type,omitempty"`
	Terminal         bool                     `json:"terminal"`
	Deduplicated     bool                     `json:"deduplicated,omitempty"`
	LatencyMS        int64                    `json:"latency_ms,omitempty"`
	RecordsSaved     int                      `json:"records_saved,omitempty"`
	IntactCachedRows int64                    `json:"intact_cached_rows,omitempty"`
	Data             *pipeline.DeliveredPage  `json:"data,omitempty"`
	Drift            *pipeline.DriftDiagnosis `json:"drift,omitempty"`
	Error            string                   `json:"error,omitempty"`
}

func (s *Service) DeployAndRun(ctx context.Context, in DeployAndRunInput, baseURL string) (ExecutionResponse, error) {
	in.SourceID = strings.TrimSpace(in.SourceID)
	if in.SourceID == "" {
		return ExecutionResponse{}, fmt.Errorf("source_id is required")
	}
	if in.RollbackTo > 0 {
		if err := s.RollbackVersion(ctx, in.SourceID, in.RollbackTo); err != nil {
			return ExecutionResponse{}, err
		}
		return s.RunConnector(ctx, RunConnectorInput{
			SourceID:     in.SourceID,
			TargetURL:    in.TargetURL,
			Params:       in.Params,
			WaitMS:       in.WaitMS,
			ForceRefresh: true,
			Query:        in.Query,
			Queries:      in.Queries,
		}, baseURL)
	}

	if in.Kind == "" {
		in.Kind = KindData
	}
	if in.Kind != KindData && in.Kind != KindDiscovery {
		return ExecutionResponse{}, fmt.Errorf("kind must be data or discovery")
	}
	if in.Executor != ExecutorHTTPAPI && in.Executor != ExecutorLuaSandbox {
		return ExecutionResponse{}, fmt.Errorf("executor must be http_api or lua_sandbox")
	}
	if strings.TrimSpace(in.SpecOrCode) == "" {
		return ExecutionResponse{}, fmt.Errorf("spec_or_code is required")
	}

	normOutJSON, parsedOut, err := contract.NormalizeOutputSchema(in.OutputSchema)
	if err != nil {
		return ExecutionResponse{}, err
	}
	normInJSON, err := contract.NormalizeParameterContract(in.InputSchema, in.Params)
	if err != nil {
		return ExecutionResponse{}, err
	}

	targetURL := strings.TrimSpace(in.TargetURL)
	var domain string
	var trial trialOutcome
	if in.Executor == ExecutorHTTPAPI {
		var spec APISpec
		if err := json.Unmarshal([]byte(in.SpecOrCode), &spec); err != nil {
			return ExecutionResponse{}, fmt.Errorf("invalid APISpec JSON: %w", err)
		}
		if spec.URLTemplate == "" {
			spec.URLTemplate = targetURL
		}
		if targetURL == "" {
			targetURL = spec.URLTemplate
		}
		trial, err = s.validateAPITrial(ctx, spec, in.Params, in.AuthProfile, parsedOut)
		if err != nil {
			res := s.intactStateResponse(ctx, in.SourceID, "failed", err.Error())
			res.Drift = trial.Drift
			return res, fmt.Errorf("trial validation failed (connector not published): %w", err)
		}
		if b, mErr := json.Marshal(spec); mErr == nil {
			in.SpecOrCode = string(b)
		}
	} else {
		if targetURL == "" {
			return ExecutionResponse{}, fmt.Errorf("target_url is required for lua_sandbox")
		}
		trial, err = s.validateLuaTrial(ctx, targetURL, in.SpecOrCode, in.Params, in.AuthProfile, parsedOut)
		if err != nil {
			res := s.intactStateResponse(ctx, in.SourceID, "failed", err.Error())
			res.Drift = trial.Drift
			return res, fmt.Errorf("trial validation failed (connector not published): %w", err)
		}
	}

	if u, uErr := url.Parse(targetURL); uErr == nil {
		domain = strings.ToLower(u.Hostname())
	}

	params := in.Params
	if params == nil {
		params = map[string]any{}
	}
	paramsRaw, err := json.Marshal(params)
	if err != nil {
		return ExecutionResponse{}, err
	}

	hitType := HitColdBuild
	var publishedVersion int
	var jobID uint64
	var hasRemainingPages bool
	now := time.Now().UTC()

	err = s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing Connector
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&existing, "source_id = ?", in.SourceID).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			publishedVersion = 1
			c := Connector{
				SourceID:       in.SourceID,
				Kind:           in.Kind,
				Executor:       in.Executor,
				Domain:         domain,
				URLPattern:     targetURL,
				Description:    in.Description,
				ActiveVersion:  publishedVersion,
				Status:         StatusActive,
				LastVerifiedAt: &now,
				LastUsedAt:     &now,
			}
			if err := tx.Create(&c).Error; err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else {
			if existing.Status == StatusDegraded {
				hitType = HitRepair
				_ = tx.Model(&DriftEvent{}).
					Where("source_id = ? AND resolved_by_version = 0", in.SourceID).
					Update("resolved_by_version", existing.ActiveVersion+1).Error
			}
			publishedVersion = existing.ActiveVersion + 1
			if err := tx.Model(&Connector{}).Where("source_id = ?", in.SourceID).Updates(map[string]any{
				"kind":                 in.Kind,
				"executor":             in.Executor,
				"domain":               domain,
				"url_pattern":          targetURL,
				"description":          chooseNonEmpty(in.Description, existing.Description),
				"active_version":       publishedVersion,
				"status":               StatusActive,
				"consecutive_failures": 0,
				"last_drift_reason":    "",
				"last_verified_at":     now,
				"last_used_at":         now,
			}).Error; err != nil {
				return err
			}
		}

		ver := ConnectorVersion{
			SourceID:       in.SourceID,
			Version:        publishedVersion,
			Executor:       in.Executor,
			ArtifactHash:   contract.HashJSON([]any{in.Executor, in.SpecOrCode, normInJSON, normOutJSON}),
			SpecOrCode:     in.SpecOrCode,
			InputSchema:    normInJSON,
			OutputSchema:   normOutJSON,
			AuthProfileRef: in.AuthProfile,
		}
		if err := tx.Create(&ver).Error; err != nil {
			return err
		}

		// Promote validated trial page-1 data atomically so DeployAndRun NEVER
		// double-fetches page 1 from upstream.
		var cpJSON string
		if trial.NextCursor != nil {
			hasRemainingPages = true
			if rawCP, mErr := json.Marshal(trial.NextCursor); mErr == nil {
				cpJSON = string(rawCP)
			}
		}
		dedupKey := contract.HashJSON([]any{in.SourceID, publishedVersion, targetURL, params})
		taskRow := taskmodel.CrawlTask{
			RequestKey:       "conn:" + contract.RandomToken(),
			URL:              targetURL,
			SourceID:         in.SourceID,
			ConnectorVersion: publishedVersion,
			Executor:         in.Executor,
			ActiveDedupKey:   dedupKey,
			ParamsJSON:       string(paramsRaw),
			CheckpointJSON:   cpJSON,
			HitType:          hitType,
			LatencyMS:        trial.LatencyMS,
			Status:           taskmodel.StatusQueued,
			QueuedAt:         &now,
		}
		if !hasRemainingPages {
			taskRow.Status = taskmodel.StatusSucceeded
			taskRow.StartedAt = &now
			taskRow.FinishedAt = &now
		}
		if err := tx.Create(&taskRow).Error; err != nil {
			return err
		}
		jobID = taskRow.ID

		ingestRes, err := s.Pipeline.IngestBatchTx(ctx, tx, pipeline.IngestInput{
			SourceID:         in.SourceID,
			ConnectorVersion: publishedVersion,
			JobID:            taskRow.ID,
			OutputSchema:     parsedOut,
			Records:          trial.Rows,
			Evidence:         trial.Evidence,
			RequireNonEmpty:  true,
			SkipMinRows:      hasRemainingPages,
		})
		if err != nil {
			return err
		}
		if err := tx.Model(&taskmodel.CrawlTask{}).Where("id = ?", taskRow.ID).
			Update("records_saved", ingestRes.SavedRows).Error; err != nil {
			return err
		}

		if !hasRemainingPages {
			return tx.Create(&ExecutionMetric{
				SourceID:  in.SourceID,
				JobID:     taskRow.ID,
				HitType:   hitType,
				Executor:  in.Executor,
				LatencyMS: trial.LatencyMS,
				SavedRows: ingestRes.SavedRows,
			}).Error
		}

		// Multi-page connector: enqueue outbox so Worker resumes cleanly from page 2.
		outbox := taskmodel.TaskOutbox{TaskID: taskRow.ID}
		return tx.Create(&outbox).Error
	})
	if err != nil {
		return ExecutionResponse{}, err
	}

	if !hasRemainingPages {
		return s.BuildJobResponseWithDelivery(ctx, jobID, "", baseURL, in.Query, in.Queries, false)
	}
	if s.InlineExec != nil {
		if execErr := s.InlineExec(ctx, jobID); execErr != nil {
			return s.BuildJobResponseWithDelivery(ctx, jobID, "", baseURL, in.Query, in.Queries, false)
		}
	}
	return s.waitForTask(ctx, jobID, in.WaitMS, baseURL, in.Query, in.Queries)
}

func (s *Service) RollbackVersion(ctx context.Context, sourceID string, targetVersion int) error {
	var ver ConnectorVersion
	if err := s.DB.WithContext(ctx).First(&ver, "source_id = ? AND version = ?", sourceID, targetVersion).Error; err != nil {
		return fmt.Errorf("target version %d for %q not found: %w", targetVersion, sourceID, err)
	}
	res := s.DB.WithContext(ctx).Model(&Connector{}).Where("source_id = ?", sourceID).Updates(map[string]any{
		"executor":             ver.Executor,
		"active_version":       targetVersion,
		"status":               StatusActive,
		"consecutive_failures": 0,
		"last_drift_reason":    "",
	})
	return res.Error
}

func (s *Service) RunConnector(ctx context.Context, in RunConnectorInput, baseURL string) (ExecutionResponse, error) {
	start := time.Now()
	var conn Connector
	if err := s.DB.WithContext(ctx).First(&conn, "source_id = ?", strings.TrimSpace(in.SourceID)).Error; err != nil {
		return ExecutionResponse{}, fmt.Errorf("connector %q not found: %w", in.SourceID, err)
	}
	if conn.Status == StatusDisabled {
		return ExecutionResponse{}, fmt.Errorf("connector %q is disabled", conn.SourceID)
	}
	var ver ConnectorVersion
	if err := s.DB.WithContext(ctx).First(&ver, "source_id = ? AND version = ?", conn.SourceID, conn.ActiveVersion).Error; err != nil {
		return ExecutionResponse{}, err
	}
	if err := contract.ValidateParameters(ver.InputSchema, in.Params); err != nil {
		return ExecutionResponse{}, err
	}
	targetURL := strings.TrimSpace(in.TargetURL)
	urlOverridden := targetURL != "" && targetURL != conn.URLPattern
	if targetURL == "" {
		targetURL = conn.URLPattern
	}

	// Fast Cache Hit path when requested freshness TTL is satisfied, target_url is not overridden, and source is healthy.
	if !in.ForceRefresh && !urlOverridden && in.FreshnessTTLSec > 0 && conn.Status == StatusActive && conn.LastVerifiedAt != nil {
		if time.Since(*conn.LastVerifiedAt) <= time.Duration(in.FreshnessTTLSec)*time.Second {
			page, err := s.deliverStoredView(ctx, conn.SourceID, 0, in.Cursor, baseURL, in.Query, in.Queries, false)
			if err == nil && (page.Returned > 0 || page.Total > 0 || in.Cursor != "") {
				var latestTask taskmodel.CrawlTask
				_ = s.DB.WithContext(ctx).Select("id").
					Where("source_id = ? AND status = ?", conn.SourceID, taskmodel.StatusSucceeded).
					Order("id DESC").First(&latestTask).Error
				elapsed := time.Since(start).Milliseconds()
				_ = s.DB.WithContext(ctx).Model(&Connector{}).Where("source_id = ?", conn.SourceID).
					Updates(map[string]any{"reuse_count": gorm.Expr("reuse_count + 1"), "last_used_at": time.Now().UTC()}).Error
				_ = s.DB.WithContext(ctx).Create(&ExecutionMetric{
					SourceID: conn.SourceID, JobID: latestTask.ID, HitType: HitCacheHit, Executor: conn.Executor,
					LatencyMS: elapsed, SavedRows: page.Returned,
				}).Error
				return ExecutionResponse{
					JobID:         latestTask.ID,
					SourceID:      conn.SourceID,
					Version:       conn.ActiveVersion,
					ActiveVersion: conn.ActiveVersion,
					Executor:      conn.Executor,
					Status:        HitCacheHit,
					HitType:       HitCacheHit,
					Terminal:      true,
					LatencyMS:     elapsed,
					Data:          &page,
				}, nil
			}
		}
	}

	if conn.Status == StatusDegraded {
		var drift DriftEvent
		_ = s.DB.WithContext(ctx).Order("id DESC").First(&drift, "source_id = ?", conn.SourceID).Error
		intact := s.countIntactCachedRows(ctx, conn.SourceID)
		return ExecutionResponse{
			SourceID:         conn.SourceID,
			Version:          conn.ActiveVersion,
			ActiveVersion:    conn.ActiveVersion,
			Executor:         conn.Executor,
			Status:           StatusDegraded,
			Terminal:         true,
			IntactCachedRows: intact,
			Error:            conn.LastDriftReason,
			Drift: &pipeline.DriftDiagnosis{
				Code:   drift.Code,
				Reason: drift.Reason,
			},
		}, fmt.Errorf("connector %q is degraded (%s); update spec_or_code with deploy_and_run or rollback", conn.SourceID, conn.LastDriftReason)
	}

	hitType := HitLuaReuse
	if conn.Executor == ExecutorHTTPAPI {
		hitType = HitAPIReuse
	}
	return s.enqueueAndWait(ctx, conn.SourceID, conn.ActiveVersion, conn.Executor, targetURL, in.Params, hitType, in.WaitMS, baseURL, in.Query, in.Queries)
}

func (s *Service) enqueueAndWait(ctx context.Context, sourceID string, version int, executor, targetURL string, params map[string]any, hitType string, waitMS int, baseURL string, query *pipeline.RecordQuery, queries map[string]*pipeline.RecordQuery) (ExecutionResponse, error) {
	if params == nil {
		params = map[string]any{}
	}
	paramsRaw, err := json.Marshal(params)
	if err != nil {
		return ExecutionResponse{}, err
	}
	dedupKey := contract.HashJSON([]any{sourceID, version, targetURL, params})

	// Singleflight dedup across concurrent host sessions for active identical jobs.
	var existing taskmodel.CrawlTask
	if err := s.DB.WithContext(ctx).
		Where("active_dedup_key = ? AND status IN ?", dedupKey, []taskmodel.Status{taskmodel.StatusPending, taskmodel.StatusQueued, taskmodel.StatusRunning}).
		Order("id DESC").First(&existing).Error; err == nil {
		res, waitErr := s.waitForTask(ctx, existing.ID, waitMS, baseURL, query, queries)
		res.Deduplicated = true
		return res, waitErr
	}

	taskRow := taskmodel.CrawlTask{
		RequestKey:       "conn:" + contract.RandomToken(),
		URL:              targetURL,
		SourceID:         sourceID,
		ConnectorVersion: version,
		Executor:         executor,
		ActiveDedupKey:   dedupKey,
		ParamsJSON:       string(paramsRaw),
		HitType:          hitType,
	}
	if err := repository.CreateQueuedCrawlTaskWithOutboxOn(ctx, s.DB, &taskRow); err != nil {
		return ExecutionResponse{}, err
	}
	if s.InlineExec != nil {
		if execErr := s.InlineExec(ctx, taskRow.ID); execErr != nil {
			return s.BuildJobResponseWithDelivery(ctx, taskRow.ID, "", baseURL, query, queries, false)
		}
	}
	return s.waitForTask(ctx, taskRow.ID, waitMS, baseURL, query, queries)
}

func (s *Service) waitForTask(ctx context.Context, taskID uint64, waitMS int, baseURL string, query *pipeline.RecordQuery, queries map[string]*pipeline.RecordQuery) (ExecutionResponse, error) {
	if waitMS > 0 {
		deadline := time.Now().Add(time.Duration(waitMS) * time.Millisecond)
		for time.Now().Before(deadline) {
			var cur taskmodel.CrawlTask
			if err := s.DB.WithContext(ctx).First(&cur, taskID).Error; err == nil && cur.Status.IsTerminal() {
				return s.BuildJobResponseWithDelivery(ctx, taskID, "", baseURL, query, queries, false)
			}
			select {
			case <-ctx.Done():
				return s.BuildJobResponseWithDelivery(context.Background(), taskID, "", baseURL, query, queries, false)
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
	return s.BuildJobResponseWithDelivery(ctx, taskID, "", baseURL, query, queries, false)
}

func (s *Service) deliverStoredView(ctx context.Context, sourceID string, jobID uint64, cursor, baseURL string, query *pipeline.RecordQuery, queries map[string]*pipeline.RecordQuery, rawPage bool) (pipeline.DeliveredPage, error) {
	var (
		page pipeline.DeliveredPage
		err  error
	)
	switch {
	case len(queries) > 0:
		page, err = s.Pipeline.DeliverRecordsMultiQuery(ctx, sourceID, jobID, s.Pipeline.QuerySafetyMaxBytes(), pipeline.DefaultMCPPageSize, baseURL, queries)
	case query != nil && !query.IsEmpty():
		page, err = s.Pipeline.DeliverRecordsPageWithQuery(ctx, sourceID, jobID, cursor, s.Pipeline.QuerySafetyMaxBytes(), pipeline.DefaultMCPPageSize, baseURL, query)
	case rawPage:
		page, err = s.Pipeline.DeliverRecordsPage(ctx, sourceID, jobID, cursor, pipeline.DefaultMCPResponseMaxBytes, pipeline.DefaultMCPPageSize, baseURL)
	default:
		page, err = s.Pipeline.DeliverReceipt(ctx, sourceID, jobID, baseURL)
	}
	if err == nil {
		// Parent ExecutionResponse already carries top-level source_id and job_id.
		page.SourceID = ""
		page.JobID = 0
	}
	return page, err
}

func (s *Service) countIntactCachedRows(ctx context.Context, sourceID string) int64 {
	if strings.TrimSpace(sourceID) == "" {
		return 0
	}
	var cnt int64
	_ = s.DB.WithContext(ctx).Model(&pipeline.SourceRecord{}).
		Where("source_id = ? AND is_current = ?", sourceID, true).Count(&cnt).Error
	return cnt
}

func (s *Service) intactStateResponse(ctx context.Context, sourceID, status, errMsg string) ExecutionResponse {
	res := ExecutionResponse{
		SourceID: sourceID,
		Status:   status,
		Terminal: true,
		Error:    errMsg,
	}
	if strings.TrimSpace(sourceID) == "" {
		return res
	}
	var conn Connector
	if err := s.DB.WithContext(ctx).Select("active_version, executor").First(&conn, "source_id = ?", sourceID).Error; err == nil {
		res.ActiveVersion = conn.ActiveVersion
		res.Executor = conn.Executor
		res.IntactCachedRows = s.countIntactCachedRows(ctx, sourceID)
	}
	return res
}

func (s *Service) BuildJobResponse(ctx context.Context, taskID uint64, cursor, baseURL string) (ExecutionResponse, error) {
	return s.BuildJobResponseWithDelivery(ctx, taskID, cursor, baseURL, nil, nil, true)
}

func (s *Service) BuildJobResponseWithDelivery(ctx context.Context, taskID uint64, cursor, baseURL string, query *pipeline.RecordQuery, queries map[string]*pipeline.RecordQuery, rawPage bool) (ExecutionResponse, error) {
	var t taskmodel.CrawlTask
	if err := s.DB.WithContext(ctx).First(&t, taskID).Error; err != nil {
		return ExecutionResponse{}, err
	}
	out := ExecutionResponse{
		JobID:        t.ID,
		SourceID:     t.SourceID,
		Version:      t.ConnectorVersion,
		Executor:     t.Executor,
		Status:       string(t.Status),
		HitType:      t.HitType,
		Terminal:     t.Status.IsTerminal(),
		LatencyMS:    t.LatencyMS,
		RecordsSaved: t.RecordsSaved,
		Error:        t.LastError,
	}
	if t.Status == taskmodel.StatusSucceeded {
		jobFilter := t.ID
		if t.RecordsSaved == 0 {
			// All rows matched prior content_hash (unchanged warm refresh/rollback):
			// deliver the source's current active view seamlessly.
			jobFilter = 0
		}
		page, err := s.deliverStoredView(ctx, t.SourceID, jobFilter, cursor, baseURL, query, queries, rawPage)
		if err != nil {
			return out, err
		}
		if page.Returned == 0 && page.Total == 0 && jobFilter != 0 && cursor == "" {
			page, err = s.deliverStoredView(ctx, t.SourceID, 0, cursor, baseURL, query, queries, rawPage)
			if err != nil {
				return out, err
			}
		}
		out.Data = &page
	} else if t.Status == taskmodel.StatusFailed || t.Status == taskmodel.StatusCancelled {
		var conn Connector
		if err := s.DB.WithContext(ctx).Select("active_version").First(&conn, "source_id = ?", t.SourceID).Error; err == nil {
			out.ActiveVersion = conn.ActiveVersion
		}
		out.IntactCachedRows = s.countIntactCachedRows(ctx, t.SourceID)
	}
	if t.FailureCode == "silent_drift" {
		var drift DriftEvent
		if err := s.DB.WithContext(ctx).Order("id DESC").First(&drift, "job_id = ?", t.ID).Error; err == nil {
			out.Drift = &pipeline.DriftDiagnosis{Code: drift.Code, Reason: drift.Reason}
		}
	}
	return out, nil
}

func (s *Service) CancelJob(ctx context.Context, jobID uint64) (ExecutionResponse, error) {
	row, err := repository.CancelCrawlTask(ctx, s.DB, jobID)
	if err != nil {
		return ExecutionResponse{}, err
	}
	worker.CancelActiveTask(jobID)
	return ExecutionResponse{
		JobID:    row.ID,
		SourceID: row.SourceID,
		Version:  row.ConnectorVersion,
		Status:   string(row.Status),
		Terminal: row.Status.IsTerminal(),
	}, nil
}

// ExecuteConnectorTask is the unified Worker entry point for both http_api and lua_sandbox connectors.
func (s *Service) ExecuteConnectorTask(ctx context.Context, t taskmodel.CrawlTask) (repository.PersistTaskResult, error) {
	if t.SourceID == "" || t.ConnectorVersion <= 0 {
		return nil, fmt.Errorf("crawl task %d missing source_id or connector_version", t.ID)
	}
	started := time.Now()
	var ver ConnectorVersion
	if err := s.DB.WithContext(ctx).First(&ver, "source_id = ? AND version = ?", t.SourceID, t.ConnectorVersion).Error; err != nil {
		return nil, fmt.Errorf("load connector %s v%d: %w", t.SourceID, t.ConnectorVersion, err)
	}
	_, outSchema, err := contract.NormalizeOutputSchema([]byte(ver.OutputSchema))
	if err != nil {
		return nil, err
	}
	var params map[string]any
	if t.ParamsJSON != "" {
		_ = json.Unmarshal([]byte(t.ParamsJSON), &params)
	}

	var connMeta Connector
	var isTargetOverride bool
	if err := s.DB.WithContext(ctx).Select("url_pattern").First(&connMeta, "source_id = ?", t.SourceID).Error; err == nil {
		if t.URL != "" && connMeta.URLPattern != "" && t.URL != connMeta.URLPattern {
			isTargetOverride = true
		}
	}

	var batchCount int
	var totalSaved int
	var lastBatchErr error
	var isPaginatedAPI bool
	cpHooks := &engine.CheckpointHooks{
		Get: func(ctx context.Context) (any, error) {
			var cur taskmodel.CrawlTask
			if err := s.DB.WithContext(ctx).Select("checkpoint_json").First(&cur, t.ID).Error; err != nil {
				return nil, err
			}
			if cur.CheckpointJSON == "" {
				return nil, nil
			}
			var val any
			dec := json.NewDecoder(strings.NewReader(cur.CheckpointJSON))
			dec.UseNumber()
			if err := dec.Decode(&val); err != nil {
				return nil, err
			}
			return val, nil
		},
		Save: func(ctx context.Context, cursor any, records []map[string]any, evidence []acquisition.Response) error {
			batchCount++
			var cpStr string
			if cursor != nil {
				if raw, mErr := json.Marshal(cursor); mErr == nil {
					cpStr = string(raw)
				}
			}
			skipMinRows := isPaginatedAPI || cursor != nil
			requireNonEmpty := t.RecordsSaved == 0 && batchCount == 1 && cursor == nil
			saveErr := s.commitBatchCAS(ctx, t, ver, outSchema, cpStr, records, evidence, requireNonEmpty, !isTargetOverride, skipMinRows, &totalSaved)
			if saveErr != nil {
				lastBatchErr = saveErr
			}
			return saveErr
		},
	}

	var finalEvidence []acquisition.Response
	var finalRows []map[string]any
	switch ver.Executor {
	case ExecutorHTTPAPI:
		var spec APISpec
		if err := json.Unmarshal([]byte(ver.SpecOrCode), &spec); err != nil {
			return nil, err
		}
		if isTargetOverride {
			spec.URLTemplate = t.URL
			if !strings.Contains(t.URL, "{") {
				spec.Pagination = "none"
				spec.MaxPages = 1
			}
		} else if spec.Pagination != "" && !strings.EqualFold(spec.Pagination, "none") && spec.MaxPages != 1 {
			isPaginatedAPI = true
		}
		finalRows, finalEvidence, err = ExecuteHTTPAPI(ctx, s.SafeClient, spec, params, ver.AuthProfileRef, 0, cpHooks)
		if err != nil {
			if lastBatchErr != nil && (errors.Is(lastBatchErr, pipeline.ErrSilentDrift) || errors.Is(lastBatchErr, repository.ErrTaskTransitionConflict)) {
				return nil, fmt.Errorf("%w: %v", lastBatchErr, err)
			}
			return nil, err
		}
		if isPaginatedAPI && outSchema.MinRows > 0 {
			totalJobRows := t.RecordsSaved + len(finalRows)
			if totalJobRows < outSchema.MinRows {
				drift := &pipeline.DriftDiagnosis{
					Code:      "ROW_COUNT_BELOW_BASELINE",
					Reason:    fmt.Sprintf("yielded %d rows across pages, below expected min_rows %d", totalJobRows, outSchema.MinRows),
					TotalRows: totalJobRows,
				}
				if len(finalRows) > 0 {
					drift.SampleRow = finalRows[0]
				}
				if !isTargetOverride {
					_ = s.recordDriftFailure(ctx, t, ver, compactDriftSample(drift))
				}
				return nil, fmt.Errorf("%w: %s", pipeline.ErrSilentDrift, drift.Reason)
			}
		}
	case ExecutorLuaSandbox:
		finalRows, finalEvidence, err = s.runSandboxedLua(ctx, t, ver, params, cpHooks)
		if err != nil {
			if lastBatchErr != nil && (errors.Is(lastBatchErr, pipeline.ErrSilentDrift) || errors.Is(lastBatchErr, repository.ErrTaskTransitionConflict)) {
				return nil, fmt.Errorf("%w: %v", lastBatchErr, err)
			}
			return nil, err
		}
		if batchCount == 0 {
			if cerr := s.commitBatchCAS(ctx, t, ver, outSchema, "", finalRows, finalEvidence, true, !isTargetOverride, false, &totalSaved); cerr != nil {
				return nil, cerr
			}
		}
	default:
		return nil, fmt.Errorf("unknown executor %q", ver.Executor)
	}

	latencyMS := time.Since(started).Milliseconds()
	_ = finalRows
	return func(tx *gorm.DB) error {
		now := time.Now().UTC()
		if err := tx.Model(&taskmodel.CrawlTask{}).Where("id = ?", t.ID).Updates(map[string]any{
			"latency_ms": latencyMS,
		}).Error; err != nil {
			return err
		}
		if err := tx.Model(&Connector{}).Where("source_id = ?", t.SourceID).Updates(map[string]any{
			"reuse_count":          gorm.Expr("reuse_count + 1"),
			"consecutive_failures": 0,
			"last_verified_at":     now,
			"last_used_at":         now,
		}).Error; err != nil {
			return err
		}
		return tx.Create(&ExecutionMetric{
			SourceID:  t.SourceID,
			JobID:     t.ID,
			HitType:   chooseNonEmpty(t.HitType, t.Executor),
			Executor:  ver.Executor,
			LatencyMS: latencyMS,
			SavedRows: totalSaved,
		}).Error
	}, nil
}

func (s *Service) commitBatchCAS(
	ctx context.Context,
	t taskmodel.CrawlTask,
	ver ConnectorVersion,
	schema contract.OutputSchema,
	cpJSON string,
	records []map[string]any,
	evidence []acquisition.Response,
	requireNonEmpty bool,
	allowDegrade bool,
	skipMinRows bool,
	totalSaved *int,
) error {
	var detectedDrift *pipeline.DriftDiagnosis
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var cur taskmodel.CrawlTask
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&cur, t.ID).Error; err != nil {
			return err
		}
		if cur.Status != taskmodel.StatusRunning || cur.RunToken != t.RunToken || cur.Cancelled {
			return repository.ErrTaskTransitionConflict
		}
		ingestRes, err := s.Pipeline.IngestBatchTx(ctx, tx, pipeline.IngestInput{
			SourceID:         t.SourceID,
			ConnectorVersion: ver.Version,
			JobID:            t.ID,
			OutputSchema:     schema,
			Records:          records,
			Evidence:         evidence,
			AllowEmptyBatch:  !requireNonEmpty,
			RequireNonEmpty:  requireNonEmpty,
			SkipMinRows:      skipMinRows,
		})
		if err != nil {
			if errors.Is(err, pipeline.ErrSilentDrift) && ingestRes.Drift != nil {
				detectedDrift = ingestRes.Drift
			}
			return err
		}
		changed := ingestRes.SavedRows
		*totalSaved += changed
		return repository.UpdateCrawlTaskCheckpointCAS(ctx, tx, t.ID, t.RunToken, cpJSON, changed)
	})
	if err != nil && detectedDrift != nil && allowDegrade {
		// Record drift diagnosis and degraded connector status in a SEPARATE
		// committed transaction after the ingestion transaction rolls back,
		// ensuring prior good records remain untouched while drift metadata persists.
		if derr := s.recordDriftFailure(ctx, t, ver, detectedDrift); derr != nil && !errors.Is(derr, repository.ErrTaskTransitionConflict) {
			return fmt.Errorf("%w (record drift failed: %v)", err, derr)
		}
	}
	return err
}

func (s *Service) recordDriftFailure(ctx context.Context, t taskmodel.CrawlTask, ver ConnectorVersion, drift *pipeline.DriftDiagnosis) error {
	if drift == nil {
		return nil
	}
	sampleJSON, _ := json.Marshal(drift.SampleRow)
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var cur taskmodel.CrawlTask
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&cur, t.ID).Error; err != nil {
			return err
		}
		if cur.Status != taskmodel.StatusRunning || cur.RunToken != t.RunToken || cur.Cancelled {
			return repository.ErrTaskTransitionConflict
		}
		if err := tx.Create(&DriftEvent{
			SourceID:         t.SourceID,
			ConnectorVersion: ver.Version,
			JobID:            t.ID,
			Code:             drift.Code,
			Reason:           drift.Reason,
			SampleJSON:       string(sampleJSON),
		}).Error; err != nil {
			return err
		}
		if err := tx.Model(&Connector{}).
			Where("source_id = ? AND active_version = ?", t.SourceID, ver.Version).
			Updates(map[string]any{
				"status":               StatusDegraded,
				"consecutive_failures": gorm.Expr("consecutive_failures + 1"),
				"last_drift_reason":    drift.Reason,
			}).Error; err != nil {
			return err
		}
		return tx.Create(&ExecutionMetric{
			SourceID:      t.SourceID,
			JobID:         t.ID,
			HitType:       chooseNonEmpty(t.HitType, ver.Executor),
			Executor:      ver.Executor,
			DriftDetected: true,
		}).Error
	})
}

type trialOutcome struct {
	Rows       []map[string]any
	Evidence   []acquisition.Response
	NextCursor any
	LatencyMS  int64
	Drift      *pipeline.DriftDiagnosis
}

func compactDriftSample(drift *pipeline.DriftDiagnosis) *pipeline.DriftDiagnosis {
	if drift == nil || len(drift.SampleRow) == 0 {
		return drift
	}
	cp := *drift
	sample := make(map[string]any, len(drift.SampleRow))
	for k, v := range drift.SampleRow {
		if s, ok := v.(string); ok {
			runes := []rune(s)
			if len(runes) > pipeline.DefaultReceiptFieldChars {
				sample[k] = string(runes[:pipeline.DefaultReceiptFieldChars]) + "..."
				continue
			}
		}
		sample[k] = v
	}
	cp.SampleRow = sample
	return &cp
}

func (s *Service) validateAPITrial(ctx context.Context, spec APISpec, params map[string]any, authProfile string, schema contract.OutputSchema) (trialOutcome, error) {
	started := time.Now()
	var nextCursor any
	cp := &engine.CheckpointHooks{
		Save: func(_ context.Context, cursor any, _ []map[string]any, _ []acquisition.Response) error {
			nextCursor = cursor
			return nil
		},
	}
	fetchWithProbeCache := func(fctx context.Context, req fetchgov.FetchRequest) (acquisition.Response, error) {
		if (req.Method == "" || strings.EqualFold(req.Method, http.MethodGet)) && len(req.Headers) == 0 && s.Prober != nil {
			if cached, ok := s.Prober.LookupRecent(req.URL, req.AuthProfile); ok {
				cached.Receipt.ReusedSnapshot = true
				return cached, nil
			}
		}
		return s.SafeClient.Fetch(fctx, req)
	}
	rows, evidence, err := executeHTTPAPIWithFetch(ctx, s.SafeClient, spec, params, authProfile, 1, cp, fetchWithProbeCache)
	if err != nil {
		return trialOutcome{}, err
	}
	if spec.MaxPages == 1 {
		nextCursor = nil
	}
	trialSchema := schema
	if nextCursor != nil && trialSchema.MinRows > 1 {
		// Multi-page trial validates page 1 schema & non-empty yield without double-fetching;
		// full min_rows is enforced once all remaining pages complete in ExecuteConnectorTask.
		trialSchema.MinRows = 1
	}
	if drift := pipeline.CheckSilentDrift(rows, trialSchema, evidence, false); drift != nil {
		return trialOutcome{Drift: compactDriftSample(drift)}, fmt.Errorf("%s: %s", drift.Code, drift.Reason)
	}
	return trialOutcome{
		Rows:       rows,
		Evidence:   evidence,
		NextCursor: nextCursor,
		LatencyMS:  time.Since(started).Milliseconds(),
	}, nil
}

func (s *Service) validateLuaTrial(ctx context.Context, targetURL, code string, params map[string]any, authProfile string, schema contract.OutputSchema) (trialOutcome, error) {
	started := time.Now()
	var seedSnaps map[string]acquisition.Response
	if s.Prober != nil {
		if cached, ok := s.Prober.LookupRecent(targetURL, authProfile); ok && cached.Receipt.SnapshotKey != "" {
			cached.Receipt.ReusedSnapshot = true
			seedSnaps = map[string]acquisition.Response{cached.Receipt.SnapshotKey: cached}
		}
	}
	req := programrunner.Request{
		Program: engine.ProgramRequest{
			Code:              code,
			TargetURL:         targetURL,
			Params:            params,
			AllowPartialItems: false,
		},
		Limits: engine.ProgramLimits{
			Timeout:        20 * time.Second,
			MaxRequests:    10,
			MaxBytes:       int64(fetchgov.DefaultMaxBodyBytes),
			MaxItems:       200,
			MaxOutputBytes: 2 * 1024 * 1024,
		},
		AuthProfile:   authProfile,
		RespectRobots: s.Cfg.RespectRobots,
		Snapshots:     seedSnaps,
	}
	var result engine.ProgramResult
	var err error
	if s.Runner != nil {
		runCtx := ctx
		if len(seedSnaps) > 0 {
			if sharedCtx, cleanup, sErr := s.Runner.ShareSnapshots(ctx, seedSnaps); sErr == nil && cleanup != nil {
				defer cleanup()
				runCtx = sharedCtx
			}
		}
		result, err = s.Runner.Execute(runCtx, req)
	} else {
		var resp programrunner.Response
		resp, err = programrunner.RunWithClient(ctx, req, nil, nil, s.SafeClient)
		if resp.Result != nil {
			result = *resp.Result
		}
	}
	if err != nil {
		return trialOutcome{}, err
	}
	rows := extractItemsSlice(result.Collection)
	if len(rows) == 0 && len(result.CheckpointedItems) > 0 {
		rows = result.CheckpointedItems
	}
	if drift := pipeline.CheckSilentDrift(rows, schema, result.SourceEvidence, false); drift != nil {
		return trialOutcome{Drift: compactDriftSample(drift)}, fmt.Errorf("%s: %s", drift.Code, drift.Reason)
	}
	return trialOutcome{
		Rows:       rows,
		Evidence:   result.SourceEvidence,
		NextCursor: result.FinalCheckpoint,
		LatencyMS:  time.Since(started).Milliseconds(),
	}, nil
}

func (s *Service) runSandboxedLua(ctx context.Context, t taskmodel.CrawlTask, ver ConnectorVersion, params map[string]any, cp *engine.CheckpointHooks) ([]map[string]any, []acquisition.Response, error) {
	req := programrunner.Request{
		Program: engine.ProgramRequest{
			Code:      ver.SpecOrCode,
			TargetURL: t.URL,
			Params:    params,
		},
		Limits:        s.luaLimits,
		AuthProfile:   ver.AuthProfileRef,
		RespectRobots: s.Cfg.RespectRobots,
		HasCheckpoint: cp != nil,
	}
	var result engine.ProgramResult
	var err error
	if s.Runner != nil {
		if s.Cfg.CheckpointBaseURL != "" && cp != nil {
			cbToken := contract.RandomToken()
			unregister := s.Hub.Register(strconv.FormatUint(t.ID, 10), cbToken, cp)
			defer unregister()
			req.CheckpointCallbackURL = strings.TrimRight(s.Cfg.CheckpointBaseURL, "/") + "/internal/checkpoints/" + strconv.FormatUint(t.ID, 10)
			req.CheckpointCallbackToken = cbToken
		}
		result, err = s.Runner.Execute(ctx, req)
	} else {
		var resp programrunner.Response
		resp, err = programrunner.RunWithClient(ctx, req, nil, cp, s.SafeClient)
		if resp.Result != nil {
			result = *resp.Result
		}
	}
	if err != nil {
		return nil, result.SourceEvidence, err
	}
	return extractItemsSlice(result.Collection), result.SourceEvidence, nil
}

// StartHotCanaryMonitor periodically checks recently used active connectors without probing stale ones.
func (s *Service) StartHotCanaryMonitor(ctx context.Context, interval, maxInactive time.Duration, staleAfter time.Duration) {
	if interval <= 0 {
		interval = 15 * time.Minute
	}
	if maxInactive <= 0 {
		maxInactive = 14 * 24 * time.Hour
	}
	if staleAfter <= 0 {
		staleAfter = 6 * time.Hour
	}
	s.canaryMu.Lock()
	if s.canaryStop != nil {
		s.canaryMu.Unlock()
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	s.canaryStop = cancel
	s.canaryMu.Unlock()

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				_ = s.ProbeHotConnectorsOnce(runCtx, maxInactive, staleAfter)
			}
		}
	}()
}

func (s *Service) ProbeHotConnectorsOnce(ctx context.Context, maxInactive, staleAfter time.Duration) error {
	now := time.Now().UTC()
	activeCutoff := now.Add(-maxInactive)
	verifyCutoff := now.Add(-staleAfter)
	var hot []Connector
	if err := s.DB.WithContext(ctx).
		Where("status = ? AND last_used_at >= ? AND (last_verified_at IS NULL OR last_verified_at < ?)", StatusActive, activeCutoff, verifyCutoff).
		Limit(10).Find(&hot).Error; err != nil {
		return err
	}
	for _, c := range hot {
		_, _ = s.RunConnector(ctx, RunConnectorInput{
			SourceID:     c.SourceID,
			ForceRefresh: true,
			WaitMS:       0,
		}, "")
	}
	return nil
}

func extractItemsSlice(collection map[string]any) []map[string]any {
	if collection == nil {
		return nil
	}
	raw, ok := collection["items"]
	if !ok {
		return nil
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil
	}
	rows := make([]map[string]any, 0, len(arr))
	for _, item := range arr {
		if m, ok := item.(map[string]any); ok {
			rows = append(rows, m)
		}
	}
	return rows
}

func chooseNonEmpty(preferred, fallback string) string {
	if strings.TrimSpace(preferred) != "" {
		return preferred
	}
	return fallback
}
