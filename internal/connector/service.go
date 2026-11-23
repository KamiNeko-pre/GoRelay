package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
		}
		if in.FreshnessTTLSec > 0 && c.Status == StatusActive && c.LastVerifiedAt != nil {
			if now.Sub(*c.LastVerifiedAt) <= time.Duration(in.FreshnessTTLSec)*time.Second {
				view.FreshCacheAvailable = true
			}
		}
		var ver ConnectorVersion
		if err := s.DB.WithContext(ctx).First(&ver, "source_id = ? AND version = ?", c.SourceID, c.ActiveVersion).Error; err == nil {
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
	Params       map[string]any  `json:"params,omitempty"`
	AuthProfile  string          `json:"auth_profile,omitempty"`
	WaitMS       int             `json:"wait_ms,omitempty"`
	RollbackTo   int             `json:"rollback_to,omitempty"`
}

type RunConnectorInput struct {
	SourceID        string         `json:"source_id"`
	TargetURL       string         `json:"target_url,omitempty"`
	Params          map[string]any `json:"params,omitempty"`
	FreshnessTTLSec int            `json:"freshness_ttl,omitempty"`
	WaitMS          int            `json:"wait_ms,omitempty"`
	ForceRefresh    bool           `json:"force_refresh,omitempty"`
}

type ExecutionResponse struct {
	JobID          uint64                  `json:"job_id,omitempty"`
	SourceID       string                  `json:"source_id"`
	Version        int                     `json:"version,omitempty"`
	Executor       string                  `json:"executor,omitempty"`
	Status         string                  `json:"status"` // succeeded | queued | running | failed | cancelled | degraded | cache_hit
	HitType        string                  `json:"hit_type,omitempty"`
	Terminal       bool                    `json:"terminal"`
	Deduplicated   bool                    `json:"deduplicated,omitempty"`
	LatencyMS      int64                   `json:"latency_ms,omitempty"`
	RecordsSaved   int                     `json:"records_saved,omitempty"`
	Data           *pipeline.DeliveredPage `json:"data,omitempty"`
	Drift          *pipeline.DriftDiagnosis `json:"drift,omitempty"`
	Error          string                  `json:"error,omitempty"`
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
		if err := s.validateAPITrial(ctx, spec, in.Params, in.AuthProfile, parsedOut); err != nil {
			return ExecutionResponse{}, fmt.Errorf("trial validation failed (connector not published): %w", err)
		}
		if b, mErr := json.Marshal(spec); mErr == nil {
			in.SpecOrCode = string(b)
		}
	} else {
		if targetURL == "" {
			return ExecutionResponse{}, fmt.Errorf("target_url is required for lua_sandbox")
		}
		if err := s.validateLuaTrial(ctx, targetURL, in.SpecOrCode, in.Params, in.AuthProfile, parsedOut); err != nil {
			return ExecutionResponse{}, fmt.Errorf("trial validation failed (connector not published): %w", err)
		}
	}

	if u, uErr := url.Parse(targetURL); uErr == nil {
		domain = strings.ToLower(u.Hostname())
	}

	hitType := HitColdBuild
	var publishedVersion int
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
		return tx.Create(&ver).Error
	})
	if err != nil {
		return ExecutionResponse{}, err
	}

	return s.enqueueAndWait(ctx, in.SourceID, publishedVersion, in.Executor, targetURL, in.Params, hitType, in.WaitMS, baseURL)
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
	if targetURL == "" {
		targetURL = conn.URLPattern
	}

	// Fast Cache Hit path when requested freshness TTL is satisfied and source is healthy.
	if !in.ForceRefresh && in.FreshnessTTLSec > 0 && conn.Status == StatusActive && conn.LastVerifiedAt != nil {
		if time.Since(*conn.LastVerifiedAt) <= time.Duration(in.FreshnessTTLSec)*time.Second {
			page, err := s.Pipeline.DeliverRecordsPage(ctx, conn.SourceID, 0, "", pipeline.DefaultMCPResponseMaxBytes, pipeline.DefaultMCPPageSize, baseURL)
			if err == nil && page.Returned > 0 {
				elapsed := time.Since(start).Milliseconds()
				_ = s.DB.WithContext(ctx).Model(&Connector{}).Where("source_id = ?", conn.SourceID).
					Updates(map[string]any{"reuse_count": gorm.Expr("reuse_count + 1"), "last_used_at": time.Now().UTC()}).Error
				_ = s.DB.WithContext(ctx).Create(&ExecutionMetric{
					SourceID: conn.SourceID, HitType: HitCacheHit, Executor: conn.Executor,
					LatencyMS: elapsed, SavedRows: page.Returned,
				}).Error
				return ExecutionResponse{
					SourceID:  conn.SourceID,
					Version:   conn.ActiveVersion,
					Executor:  conn.Executor,
					Status:    HitCacheHit,
					HitType:   HitCacheHit,
					Terminal:  true,
					LatencyMS: elapsed,
					Data:      &page,
				}, nil
			}
		}
	}

	if conn.Status == StatusDegraded {
		var drift DriftEvent
		_ = s.DB.WithContext(ctx).Order("id DESC").First(&drift, "source_id = ?", conn.SourceID).Error
		return ExecutionResponse{
			SourceID: conn.SourceID,
			Version:  conn.ActiveVersion,
			Executor: conn.Executor,
			Status:   StatusDegraded,
			Terminal: true,
			Error:    conn.LastDriftReason,
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
	return s.enqueueAndWait(ctx, conn.SourceID, conn.ActiveVersion, conn.Executor, targetURL, in.Params, hitType, in.WaitMS, baseURL)
}

func (s *Service) enqueueAndWait(ctx context.Context, sourceID string, version int, executor, targetURL string, params map[string]any, hitType string, waitMS int, baseURL string) (ExecutionResponse, error) {
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
		res, waitErr := s.waitForTask(ctx, existing.ID, waitMS, baseURL)
		res.Deduplicated = true
		return res, waitErr
	}

	taskRow := taskmodel.CrawlTask{
		RequestKey:       "conn:" + contract.RandomToken(),
		Target:           sourceID,
		URL:              targetURL,
		SourceID:         sourceID,
		ConnectorVersion: version,
		Executor:         executor,
		ActiveDedupKey:   dedupKey,
		ProgramParams:    string(paramsRaw),
		HitType:          hitType,
	}
	if err := repository.CreateQueuedCrawlTaskWithOutboxOn(ctx, s.DB, &taskRow); err != nil {
		return ExecutionResponse{}, err
	}
	if s.InlineExec != nil {
		if execErr := s.InlineExec(ctx, taskRow.ID); execErr != nil {
			return s.BuildJobResponse(ctx, taskRow.ID, "", baseURL)
		}
	}
	return s.waitForTask(ctx, taskRow.ID, waitMS, baseURL)
}

func (s *Service) waitForTask(ctx context.Context, taskID uint64, waitMS int, baseURL string) (ExecutionResponse, error) {
	if waitMS > 0 {
		deadline := time.Now().Add(time.Duration(waitMS) * time.Millisecond)
		for time.Now().Before(deadline) {
			var cur taskmodel.CrawlTask
			if err := s.DB.WithContext(ctx).First(&cur, taskID).Error; err == nil && cur.Status.IsTerminal() {
				return s.BuildJobResponse(ctx, taskID, "", baseURL)
			}
			select {
			case <-ctx.Done():
				return s.BuildJobResponse(context.Background(), taskID, "", baseURL)
			case <-time.After(40 * time.Millisecond):
			}
		}
	}
	return s.BuildJobResponse(ctx, taskID, "", baseURL)
}

func (s *Service) BuildJobResponse(ctx context.Context, taskID uint64, cursor, baseURL string) (ExecutionResponse, error) {
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
		page, err := s.Pipeline.DeliverRecordsPage(ctx, t.SourceID, t.ID, cursor, pipeline.DefaultMCPResponseMaxBytes, pipeline.DefaultMCPPageSize, baseURL)
		if err != nil {
			return out, err
		}
		if page.Returned == 0 && t.RecordsSaved > 0 {
			// All rows skipped as exact duplicates of prior versions: return current active view.
			page, err = s.Pipeline.DeliverRecordsPage(ctx, t.SourceID, 0, cursor, pipeline.DefaultMCPResponseMaxBytes, pipeline.DefaultMCPPageSize, baseURL)
			if err != nil {
				return out, err
			}
		}
		out.Data = &page
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
		return engine.ExecuteTask(ctx, t)
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
	if t.ProgramParams != "" {
		_ = json.Unmarshal([]byte(t.ProgramParams), &params)
	}

	var batchCount int
	var totalSaved int
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
			return s.commitBatchCAS(ctx, t, ver, outSchema, cpStr, records, evidence, batchCount == 1 && cursor == nil, &totalSaved)
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
		finalRows, finalEvidence, err = ExecuteHTTPAPI(ctx, s.SafeClient, spec, params, ver.AuthProfileRef, 0, cpHooks)
		if err != nil {
			return nil, err
		}
	case ExecutorLuaSandbox:
		finalRows, finalEvidence, err = s.runSandboxedLua(ctx, t, ver, params, cpHooks)
		if err != nil {
			return nil, err
		}
		if batchCount == 0 {
			if cerr := s.commitBatchCAS(ctx, t, ver, outSchema, "", finalRows, finalEvidence, true, &totalSaved); cerr != nil {
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
	totalSaved *int,
) error {
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
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
		})
		if err != nil {
			if errors.Is(err, pipeline.ErrSilentDrift) && ingestRes.Drift != nil {
				sampleJSON, _ := json.Marshal(ingestRes.Drift.SampleRow)
				_ = tx.Create(&DriftEvent{
					SourceID:         t.SourceID,
					ConnectorVersion: ver.Version,
					JobID:            t.ID,
					Code:             ingestRes.Drift.Code,
					Reason:           ingestRes.Drift.Reason,
					SampleJSON:       string(sampleJSON),
				}).Error
				_ = tx.Model(&Connector{}).Where("source_id = ?", t.SourceID).Updates(map[string]any{
					"status":               StatusDegraded,
					"consecutive_failures": gorm.Expr("consecutive_failures + 1"),
					"last_drift_reason":    ingestRes.Drift.Reason,
				}).Error
				_ = tx.Model(&taskmodel.CrawlTask{}).Where("id = ?", t.ID).Update("failure_code", "silent_drift").Error
				_ = tx.Create(&ExecutionMetric{
					SourceID: t.SourceID, JobID: t.ID, HitType: t.HitType, Executor: ver.Executor, DriftDetected: true,
				}).Error
			}
			return err
		}
		changed := ingestRes.SavedRows
		*totalSaved += changed
		return repository.UpdateCrawlTaskCheckpointCAS(ctx, tx, t.ID, t.RunToken, cpJSON, changed)
	})
}

func (s *Service) validateAPITrial(ctx context.Context, spec APISpec, params map[string]any, authProfile string, schema contract.OutputSchema) error {
	rows, evidence, err := ExecuteHTTPAPI(ctx, s.SafeClient, spec, params, authProfile, 1, nil)
	if err != nil {
		return err
	}
	if drift := pipeline.CheckSilentDrift(rows, schema, evidence, false); drift != nil {
		return fmt.Errorf("%s: %s", drift.Code, drift.Reason)
	}
	return nil
}

func (s *Service) validateLuaTrial(ctx context.Context, targetURL, code string, params map[string]any, authProfile string, schema contract.OutputSchema) error {
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
	}
	var result engine.ProgramResult
	var err error
	if s.Runner != nil {
		result, err = s.Runner.Execute(ctx, req)
	} else {
		var resp programrunner.Response
		resp, err = programrunner.RunWithClient(ctx, req, nil, nil, s.SafeClient)
		if resp.Result != nil {
			result = *resp.Result
		}
	}
	if err != nil {
		return err
	}
	rows := extractItemsSlice(result.Collection)
	if drift := pipeline.CheckSilentDrift(rows, schema, result.SourceEvidence, false); drift != nil {
		return fmt.Errorf("%s: %s", drift.Code, drift.Reason)
	}
	return nil
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
