// Package information routes information requests above the crawler. It never
// changes the crawler's tools, prompts, execution loop or delivery contract.
package information

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/agentstatus"
	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/knowledge"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Input struct {
	RequestKey     string          `json:"request_key"`
	Goal           string          `json:"goal"`
	Spec           *aiapp.TaskSpec `json:"spec,omitempty"`
	SourceScope    []string        `json:"source_scope,omitempty"`
	EntryURLs      []string        `json:"entry_urls,omitempty"`
	Refresh        bool            `json:"refresh,omitempty"`
	TimeoutSeconds *int64          `json:"timeout_seconds,omitempty"`
}

type Request struct {
	ID                string     `json:"id" gorm:"primaryKey;size:32"`
	RequestKey        string     `json:"request_key" gorm:"size:128;uniqueIndex"`
	InputHash         string     `json:"input_hash" gorm:"size:64"`
	InputJSON         string     `json:"-" gorm:"type:longtext"`
	SpecJSON          string     `json:"-" gorm:"type:text"`
	ContextJSON       string     `json:"-" gorm:"type:longtext"`
	RefreshRequired   bool       `json:"refresh_required"`
	TimeboxSeconds    int64      `json:"timebox_seconds"`
	Stage             string     `json:"stage" gorm:"size:32;index"`
	ChildRunID        string     `json:"child_run_id,omitempty" gorm:"size:32"`
	ResultJSON        string     `json:"-" gorm:"type:longtext"`
	LastError         string     `json:"last_error,omitempty" gorm:"type:text"`
	FeedbackJSON      string     `json:"-" gorm:"type:longtext"`
	StatusJSON        string     `json:"-" gorm:"type:text"`
	RouteProgressJSON string     `json:"-" gorm:"type:longtext"`
	ExperimentJSON    string     `json:"-" gorm:"type:longtext"`
	LeaseToken        string     `json:"-" gorm:"size:32"`
	LeaseUntil        *time.Time `json:"lease_until,omitempty"`
	Deadline          time.Time  `json:"deadline" gorm:"default:null"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

func (Request) TableName() string { return "information_requests" }

type Step struct {
	ID         uint64    `json:"id" gorm:"primaryKey"`
	RequestID  string    `json:"request_id" gorm:"size:32;uniqueIndex:information_step"`
	Key        string    `json:"key" gorm:"size:128;uniqueIndex:information_step"`
	Kind       string    `json:"kind" gorm:"size:32"`
	InputHash  string    `json:"input_hash" gorm:"size:64"`
	InputJSON  string    `json:"input_json" gorm:"type:longtext"`
	Stage      string    `json:"stage" gorm:"size:32"`
	ResultJSON string    `json:"result_json" gorm:"type:longtext"`
	Error      string    `json:"error,omitempty" gorm:"type:text"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

func (Step) TableName() string { return "information_steps" }

// Lua assets refer to the existing immutable artifact. API and browser assets
// own a declarative manifest in this separate registry, never generated Go/JS.
type Asset struct {
	ID           string    `json:"id" gorm:"primaryKey;size:64"`
	Kind         string    `json:"kind" gorm:"size:16;index"`
	SourceID     string    `json:"source_id" gorm:"size:64;index"`
	Version      string    `json:"version" gorm:"size:64"`
	Description  string    `json:"description" gorm:"type:text"`
	EntryURL     string    `json:"entry_url" gorm:"type:text"`
	SpecJSON     string    `json:"output_schema" gorm:"type:text"`
	ScopeJSON    string    `json:"scope" gorm:"type:text"`
	ManifestJSON string    `json:"manifest" gorm:"type:longtext"`
	ProofJSON    string    `json:"validation_proof" gorm:"type:longtext"`
	Active       bool      `json:"active" gorm:"index"`
	VerifiedAt   time.Time `json:"verified_at"`
	CreatedAt    time.Time `json:"created_at"`
}

func (Asset) TableName() string { return "information_assets" }

type Service struct {
	DB             *gorm.DB
	Crawler        *aiapp.Service
	Retriever      *knowledge.Retriever
	Model          aiapp.ChatProvider
	Search         aiapp.SearchProvider
	Browser        *BrowserRunner
	BrowserPolicy  BrowserPolicy
	RequestTimeout time.Duration
	NewFetcher     func([]string) (*acquisition.PublicFetcher, error)
}

type requestExecutionOwner struct{ RequestID, Token string }
type requestExecutionOwnerKey struct{}

func NewService(crawler *aiapp.Service) *Service {
	s := &Service{Crawler: crawler, BrowserPolicy: BrowserPolicy{MaxFailedPlans: 3}}
	if crawler != nil {
		s.DB = crawler.Data.DB
		s.Retriever = crawler.Retriever
		s.Model = crawler.Model
		s.Search = crawler.Search
	}
	s.NewFetcher = func(scope []string) (*acquisition.PublicFetcher, error) {
		return acquisition.NewPublicFetcher(acquisition.Policy{AllowedDomains: scope, MaxRequests: 100, MaxBytes: 32 << 20, Timeout: 20 * time.Second})
	}
	return s
}
func (s *Service) Migrate() error {
	if s.DB == nil {
		return fmt.Errorf("information database unavailable")
	}
	return s.DB.AutoMigrate(&Request{}, &Step{}, &Asset{}, &AssetTrial{}, &BrowserGrant{})
}

func (s *Service) Create(ctx context.Context, in Input) (Request, error) {
	in.Goal = strings.TrimSpace(in.Goal)
	if in.RequestKey == "" || len(in.RequestKey) > 128 || in.Goal == "" || len(in.Goal) > 8000 {
		return Request{}, fmt.Errorf("request_key (1..128 bytes) and goal (1..8000 bytes) required")
	}
	scopeSet := map[string]bool{}
	for _, d := range in.SourceScope {
		d = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(d), "."))
		if d == "" || strings.ContainsAny(d, "/:@* \\?#") {
			return Request{}, fmt.Errorf("source_scope must contain plain domains")
		}
		scopeSet[d] = true
	}
	in.SourceScope = nil
	for d := range scopeSet {
		in.SourceScope = append(in.SourceScope, d)
	}
	sort.Strings(in.SourceScope)
	for _, u := range in.EntryURLs {
		if !urlInScope(u, in.SourceScope) {
			return Request{}, fmt.Errorf("entry URL must be HTTP(S), without credentials, and within source_scope")
		}
	}
	if in.Spec != nil {
		spec := *in.Spec
		if spec.ContractVersion == "" {
			spec.ContractVersion = "v2"
		}
		if spec.Goal != in.Goal {
			return Request{}, fmt.Errorf("goal and frozen spec differ")
		}
		if err := spec.Validate(); err != nil {
			return Request{}, err
		}
		in.Spec = &spec
	}
	duration, err := agentstatus.RequestTimeout(in.TimeoutSeconds, s.RequestTimeout)
	if err != nil {
		return Request{}, err
	}
	var deadline time.Time
	if duration > 0 {
		deadline = time.Now().Add(duration)
	}
	seconds := int64(duration / time.Second)
	if duration%time.Second > 0 {
		seconds++
	}
	// Restore a prior admission before reading today's experimental settings.
	// Configuration changes must not rewrite a durable request's policy.
	var prior Request
	lookup := s.DB.WithContext(ctx).Where("request_key = ?", in.RequestKey).First(&prior).Error
	if lookup == nil {
		if prior.InputHash != agentdata.Hash(in) {
			return prior, agentdata.ErrConflict
		}
		return prior, nil
	}
	if !errors.Is(lookup, gorm.ErrRecordNotFound) {
		return prior, lookup
	}
	controls, err := s.captureExperiment(ctx)
	if err != nil {
		return Request{}, agentstatus.Wrap(agentstatus.SystemConfiguration, err)
	}
	r := Request{ID: agentdata.Token(), RequestKey: in.RequestKey, InputHash: agentdata.Hash(in), InputJSON: string(mustMarshal(in)), ExperimentJSON: string(mustMarshal(controls)), Stage: "local_lookup", RefreshRequired: in.Refresh, Deadline: deadline, TimeboxSeconds: seconds}
	if in.Spec != nil {
		r.SpecJSON = string(mustMarshal(in.Spec))
	}
	created := s.DB.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&r)
	if created.Error != nil {
		return r, created.Error
	}
	if created.RowsAffected == 1 {
		// SQL owns the persisted deadline precision and defaults. Return
		// that same state on first admission and request-key recovery.
		return s.Request(ctx, r.ID)
	}
	if created.RowsAffected == 0 {
		r = Request{}
		if err := s.DB.WithContext(ctx).First(&r, "request_key = ?", in.RequestKey).Error; err != nil {
			return r, err
		}
		if r.InputHash != agentdata.Hash(in) {
			return r, agentdata.ErrConflict
		}
	}
	return r, nil
}
func (s *Service) Request(ctx context.Context, id string) (Request, error) {
	var r Request
	err := s.DB.WithContext(ctx).First(&r, "id = ?", id).Error
	return r, err
}

func (r Request) RequestContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if r.Deadline.IsZero() {
		return context.WithCancel(ctx)
	}
	return context.WithDeadline(ctx, r.Deadline)
}
func (r Request) MarshalJSON() ([]byte, error) {
	type plain Request
	var deadline *time.Time
	if !r.Deadline.IsZero() {
		deadline = &r.Deadline
	}
	return json.Marshal(struct {
		plain
		Deadline   *time.Time      `json:"deadline"`
		Status     json.RawMessage `json:"status,omitempty"`
		Experiment json.RawMessage `json:"experiment"`
	}{plain: plain(r), Deadline: deadline, Status: validRaw(r.StatusJSON), Experiment: validRaw(r.ExperimentJSON)})
}
func validRaw(raw string) json.RawMessage {
	if json.Valid([]byte(raw)) {
		return json.RawMessage(raw)
	}
	return nil
}
func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
func urlInScope(raw string, domains []string) bool {
	u, e := url.Parse(raw)
	if e != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return false
	}
	return hostInScope(u.Hostname(), domains)
}
func hostInScope(host string, domains []string) bool {
	if len(domains) == 0 {
		return true
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, d := range domains {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

var ErrUnknownStep = errors.New("prior external step outcome is unknown; inspect its saved state before retrying")

// reserveStep belongs to the host. Neither a model call id nor elapsed time is
// part of this key. A crash after admission cannot silently repeat remote work.
func (s *Service) reserveStep(ctx context.Context, r Request, kind string, input any) (Step, bool, error) {
	hash := agentdata.Hash(input)
	step := Step{RequestID: r.ID, Key: kind + ":" + hash, Kind: kind, InputHash: hash, InputJSON: string(mustMarshal(input)), Stage: "reserved"}
	q := s.DB.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&step)
	if q.Error != nil {
		return step, false, q.Error
	}
	if q.RowsAffected == 1 {
		return step, true, nil
	}
	key := step.Key
	step = Step{}
	if err := s.DB.WithContext(ctx).First(&step, "request_id = ? AND `key` = ?", r.ID, key).Error; err != nil {
		return step, false, err
	}
	return step, false, nil
}
func (s *Service) finishStep(ctx context.Context, step Step, out any, cause error) error {
	stage := "succeeded"
	message := ""
	if cause != nil {
		stage = "failed"
		message = cause.Error()
	}
	return s.DB.WithContext(ctx).Model(&Step{}).Where("id = ? AND stage = ?", step.ID, "reserved").Updates(map[string]any{"stage": stage, "result_json": string(mustMarshal(out)), "error": message}).Error
}
