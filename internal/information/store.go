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
	"go-lua-crawler/internal/engine"
	"go-lua-crawler/internal/knowledge"
	"go-lua-crawler/internal/programrunner"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Input struct {
	RequestKey     string          `json:"request_key"`
	RecordsOnly    bool            `json:"records_only,omitempty"`
	Goal           string          `json:"goal"`
	Guidance       string          `json:"guidance,omitempty"`
	Spec           *aiapp.TaskSpec `json:"spec,omitempty"`
	SourceScope    []string        `json:"source_scope,omitempty"`
	EntryURLs      []string        `json:"entry_urls,omitempty"`
	Refresh        bool            `json:"refresh,omitempty"`
	TimeoutSeconds *int64          `json:"timeout_seconds,omitempty"`
}

type Request struct {
	Status               string     `json:"status,omitempty" gorm:"size:32;index"`
	RequirementsJSON     string     `json:"-" gorm:"type:longtext"`
	PlanJSON             string     `json:"-" gorm:"type:longtext"`
	CurrentDeliveryID    string     `json:"current_delivery_id,omitempty" gorm:"size:32"`
	BaselineDeliveryID   string     `json:"baseline_delivery_id,omitempty" gorm:"size:32"`
	RequirementsRevision uint64     `json:"requirements_revision"`
	FactsRevision        uint64     `json:"facts_revision"`
	WakeAt               *time.Time `json:"wake_at,omitempty" gorm:"index"`
	WaitJSON             string     `json:"-" gorm:"type:longtext"`
	EinoMessagesJSON     string     `json:"-" gorm:"type:longtext"`
	ContextPolicyJSON    string     `json:"-" gorm:"type:text"`
	EinoEpoch            uint64     `json:"-"`
	ID                   string     `json:"id" gorm:"primaryKey;size:32"`
	Purpose              string     `json:"purpose,omitempty" gorm:"size:32"`
	RequestKey           string     `json:"request_key" gorm:"size:128;uniqueIndex"`
	InputHash            string     `json:"input_hash" gorm:"size:64"`
	InputJSON            string     `json:"-" gorm:"type:longtext"`
	DiscoveryJSON        string     `json:"-" gorm:"type:longtext"`
	NativeJobJSON        string     `json:"-" gorm:"type:longtext"`
	RefreshRequired      bool       `json:"refresh_required"`
	TimeboxSeconds       int64      `json:"timebox_seconds"`
	Stage                string     `json:"stage" gorm:"type:text"`
	ChildRunID           string     `json:"child_run_id,omitempty" gorm:"size:32"`
	ResultJSON           string     `json:"-" gorm:"type:longtext"`
	LastError            string     `json:"last_error,omitempty" gorm:"type:text"`
	FeedbackJSON         string     `json:"-" gorm:"type:longtext"`
	StatusJSON           string     `json:"-" gorm:"type:text"`
	RouteProgressJSON    string     `json:"-" gorm:"type:longtext"`
	LeaseToken           string     `json:"-" gorm:"size:32"`
	LeaseUntil           *time.Time `json:"lease_until,omitempty"`
	Deadline             time.Time  `json:"deadline" gorm:"default:null"`
	CreatedAt            time.Time  `json:"created_at"`
	UpdatedAt            time.Time  `json:"updated_at"`
}

func (Request) TableName() string { return "information_requests" }

type Step struct {
	ChildRunID          string    `json:"child_run_id,omitempty" gorm:"size:32;index"`
	ExecutionKind       string    `json:"-" gorm:"size:32;index"`
	ExecutionInputJSON  string    `json:"-" gorm:"type:longtext"`
	ExecutionResultJSON string    `json:"-" gorm:"type:longtext"`
	ExecutionError      string    `json:"-" gorm:"type:text"`
	EinoEpoch           uint64    `json:"-" gorm:"index;index:idx_information_steps_req_epoch_id,priority:2"`
	CallID              string    `json:"call_id,omitempty" gorm:"size:128;index"`
	ID                  uint64    `json:"id" gorm:"primaryKey;index:idx_information_steps_req_epoch_id,priority:3"`
	RequestID           string    `json:"request_id" gorm:"size:32;uniqueIndex:information_step;index:idx_information_steps_req_epoch_id,priority:1"`
	Key                 string    `json:"key" gorm:"size:128;uniqueIndex:information_step"`
	Kind                string    `json:"kind" gorm:"type:text"`
	InputHash           string    `json:"input_hash" gorm:"size:64"`
	RawInputJSON        string    `json:"-" gorm:"type:longtext"`
	InputJSON           string    `json:"input_json" gorm:"type:longtext"`
	Stage               string    `json:"stage" gorm:"size:32"`
	ResultJSON          string    `json:"result_json" gorm:"type:longtext"`
	Error               string    `json:"error,omitempty" gorm:"type:text"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
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
	DB                 *gorm.DB
	Crawler            *aiapp.Service
	Retriever          *knowledge.Retriever
	Model              aiapp.ChatProvider
	AuxModel           aiapp.ChatProvider
	DeliveryAuditModel aiapp.ChatProvider
	Search             aiapp.SearchProvider
	Browser            *BrowserRunner
	RequestTimeout     time.Duration
	NewFetcher         func([]string) (*acquisition.PublicFetcher, error)
	LuaRunner          func(context.Context, programrunner.Request) (engine.ProgramResult, error)
}

type requestExecutionOwner struct{ RequestID, Token string }
type requestExecutionOwnerKey struct{}

func NewService(crawler *aiapp.Service) *Service {
	s := &Service{Crawler: crawler}
	if crawler != nil {
		s.DB = crawler.Data.DB
		s.Retriever = crawler.Retriever
		s.Model = crawler.Model
		s.AuxModel = crawler.AuxModel
		s.Search = crawler.Search
	}
	s.NewFetcher = func(scope []string) (*acquisition.PublicFetcher, error) {
		return acquisition.NewPublicFetcher(aiapp.InspectionPolicy(scope))
	}
	return s
}
func (s *Service) Migrate() error {
	if s.DB == nil {
		return fmt.Errorf("information database unavailable")
	}
	return agentdata.AutoMigrate(s.DB, &Request{}, &Step{}, &Asset{}, &AssetTrial{}, &BrowserGrant{}, &DiscoveryCapability{}, &DiscoveredSource{}, &InformationCheckpoint{}, &agentdata.SourceRefresh{}, &observationRow{}, &taskObservation{}, &taskDocument{}, &deliveryEvidence{}, &Delivery{}, &Monitor{}, &MonitorCheck{}, &MonitorInbox{})
}

func normalizeSourceScope(domains []string) ([]string, error) {
	set := map[string]bool{}
	for _, domain := range domains {
		domain = strings.TrimSpace(domain)
		if strings.HasPrefix(domain, "http://") || strings.HasPrefix(domain, "https://") {
			if u, err := url.Parse(domain); err == nil && u.Hostname() != "" {
				domain = u.Hostname()
			}
		}
		domain = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(domain), "."))
		if domain == "" || strings.ContainsAny(domain, "/:@* \\?#") {
			return nil, fmt.Errorf("source_scope must contain plain domains")
		}
		set[domain] = true
	}
	out := []string{}
	for domain := range set {
		out = append(out, domain)
	}
	sort.Strings(out)
	return out, nil
}

func (s *Service) Create(ctx context.Context, in Input) (Request, error) {
	return s.create(ctx, in, "")
}

func (s *Service) create(ctx context.Context, in Input, purpose string) (Request, error) {
	in.Goal = strings.TrimSpace(in.Goal)
	if in.RequestKey == "" {
		in.RequestKey = agentdata.Token()
	}
	if len(in.RequestKey) > 128 || in.Goal == "" {
		return Request{}, fmt.Errorf("goal required (and request_key <= 128 bytes)")
	}
	var err error
	in.SourceScope, err = normalizeSourceScope(in.SourceScope)
	if err != nil {
		return Request{}, err
	}
	if in.Spec != nil && len(in.Spec.SourceScope) > 0 {
		scope, err := normalizeSourceScope(in.Spec.SourceScope)
		if err != nil {
			return Request{}, err
		}
		if len(in.SourceScope) == 0 {
			in.SourceScope = scope
		} else if strings.Join(in.SourceScope, "\n") != strings.Join(scope, "\n") {
			err := businessError("UNSUPPORTED_REQUIREMENT", "requirements", "两处显式来源范围不一致", "在 source_scope 中提供一个明确的来源范围")
			err.Details = map[string]any{"path": "spec.source_scope"}
			return Request{}, err
		}
	}
	for _, u := range in.EntryURLs {
		if !urlInScope(u, in.SourceScope) {
			return Request{}, fmt.Errorf("entry URL must be HTTP(S), without credentials, and within source_scope")
		}
	}
	requirements, err := requirementsFromInput(in)
	if err != nil {
		return Request{}, err
	}
	in.Spec = nil
	inputHash := agentdata.Hash([]any{in, requirements})
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
	var prior Request
	lookup := s.DB.WithContext(ctx).Where("request_key = ?", in.RequestKey).First(&prior).Error
	if lookup == nil {
		if prior.InputHash != inputHash {
			return prior, agentdata.ErrConflict
		}
		return prior, nil
	}
	if !errors.Is(lookup, gorm.ErrRecordNotFound) {
		return prior, lookup
	}
	r := Request{ID: agentdata.Token(), RequestKey: in.RequestKey, Purpose: purpose, InputHash: inputHash, InputJSON: string(mustMarshal(in)), Stage: "understand_request", RefreshRequired: in.Refresh, Deadline: deadline, TimeboxSeconds: seconds}
	r.RequirementsJSON = string(mustMarshal(requirements))
	r.RequirementsRevision = 1
	r.Status = "queued"
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
		if r.InputHash != inputHash {
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

func (r Request) deadlineReached(now time.Time) bool {
	return !r.Deadline.IsZero() && !r.Deadline.After(now)
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
		Deadline          *time.Time      `json:"deadline"`
		OperationalStatus json.RawMessage `json:"operational_status,omitempty"`
	}{plain: plain(r), Deadline: deadline, OperationalStatus: validRaw(r.StatusJSON)})
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
// executionStep reads the physical result of the same admitted operation. The
// Eino receipt ID is supplied by the host, never accepted from model arguments.
func (s *Service) executionStep(ctx context.Context, r Request, kind string, input any) (Step, bool, error) {
	var step Step
	var err error
	if receiptID, ok := ctx.Value(acquisitionReceiptKey{}).(uint64); ok {
		err = s.DB.WithContext(ctx).First(&step, "id = ? AND request_id = ?", receiptID, r.ID).Error
		if err != nil {
			return step, false, err
		}
		if step.ExecutionKind == "" {
			return step, false, nil
		}
		if step.ExecutionKind != kind || step.ExecutionInputJSON != string(mustMarshal(input)) {
			return step, false, agentdata.ErrConflict
		}
	} else {
		err = s.DB.WithContext(ctx).First(&step, "request_id = ? AND `key` = ?", r.ID, kind+":"+agentdata.Hash(input)).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return step, false, nil
		}
		if err != nil {
			return step, false, err
		}
	}
	return executionResult(step), true, nil
}

func executionResult(step Step) Step {
	step.Kind, step.InputJSON = step.ExecutionKind, step.ExecutionInputJSON
	step.ResultJSON, step.Error = step.ExecutionResultJSON, step.ExecutionError
	step.Stage = "reserved"
	if step.ExecutionResultJSON != "" {
		step.Stage = "succeeded"
		if step.ExecutionError != "" {
			step.Stage = "failed"
		}
	}
	return step
}

func (s *Service) reserveStep(ctx context.Context, r Request, kind string, input any) (Step, bool, error) {
	raw := string(mustMarshal(input))
	if _, ok := ctx.Value(acquisitionReceiptKey{}).(uint64); ok {
		step, found, err := s.executionStep(ctx, r, kind, input)
		if err != nil || found {
			return step, false, err
		}
		q := s.DB.WithContext(ctx).Model(&Step{}).Where("id = ? AND request_id = ? AND execution_kind = ''", step.ID, r.ID).Updates(map[string]any{"execution_kind": kind, "execution_input_json": raw})
		if q.Error != nil {
			return step, false, q.Error
		}
		if q.RowsAffected != 1 {
			return step, false, agentdata.ErrConflict
		}
		step.ExecutionKind, step.ExecutionInputJSON = kind, raw
		return executionResult(step), true, nil
	}
	hash := agentdata.Hash(input)
	step := Step{RequestID: r.ID, Key: kind + ":" + hash, Kind: kind, InputHash: hash, InputJSON: raw, Stage: "reserved", EinoEpoch: r.EinoEpoch, ExecutionKind: kind, ExecutionInputJSON: raw}
	q := s.DB.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&step)
	if q.Error != nil {
		return step, false, q.Error
	}
	if q.RowsAffected == 1 {
		return step, true, nil
	}
	step, _, err := s.executionStep(ctx, r, kind, input)
	return step, false, err
}
func stepResult(out any, cause error) ([]byte, error) {
	raw, err := json.Marshal(out)
	if err != nil || cause == nil {
		return raw, err
	}
	var payload map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if decoder.Decode(&payload) != nil || payload == nil {
		payload = map[string]any{"data": out}
	}
	var failure *ActionError
	if !errors.As(cause, &failure) {
		failure = actionFailure("SAVED_ACTION_FAILED", cause, "Read the saved diagnostic and current task facts; correct a real parameter only when source evidence identifies the correction, or use another applicable source.")
	}
	opStatus := aiapp.OperationalStatus(cause)
	payload["failure"] = failure
	payload["failure_status"] = opStatus.Code
	payload["operational_status"] = opStatus
	return json.Marshal(payload)
}

func (s *Service) finishStep(ctx context.Context, step Step, out any, cause error) error {
	stage := "succeeded"
	message := ""
	if cause != nil {
		stage = "failed"
		message = cause.Error()
	}
	raw, err := stepResult(out, cause)
	if err != nil {
		return err
	}
	updates := map[string]any{"execution_result_json": string(raw), "execution_error": message}
	if step.CallID == "" {
		updates["stage"], updates["result_json"], updates["error"] = stage, string(raw), message
	}
	return s.DB.WithContext(ctx).Model(&Step{}).Where("id = ? AND execution_kind = ?", step.ID, step.ExecutionKind).Updates(updates).Error
}
