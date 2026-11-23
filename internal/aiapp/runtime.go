package aiapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/agentstatus"
	"go-lua-crawler/internal/knowledge"
	"go-lua-crawler/internal/programrunner"
	"go-lua-crawler/internal/repository"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"os"
	"strings"
	"time"
)

// ExecutionBrief is durable planning context, not a change to the frozen contract
// or an idempotency escape hatch. Embedded source records remain untrusted data.
type ExecutionBrief struct {
	KnownRecordsView string          `json:"known_records_view,omitempty"`
	ParentTaskID     string          `json:"parent_task_id"`
	Plan             json.RawMessage `json:"plan"`
	Focus            string          `json:"focus"`
	Gaps             []string        `json:"gaps,omitempty"`
	KnownRecords     []Record        `json:"known_records,omitempty"`
	CallerGuidance   string          `json:"caller_guidance,omitempty"`
}
type CreateRunRequest struct {
	SourceWorkspaceID string          `json:"source_workspace_id,omitempty"` // External source workflow metadata; never a model-loop policy.
	ExecutionBrief    *ExecutionBrief `json:"execution_brief,omitempty"`
	RequestKey        string          `json:"request_key"`
	Goal              string          `json:"goal"`
	SourceScope       []string        `json:"source_scope,omitempty"`
	EntryURLs         []string        `json:"entry_urls,omitempty"`
	Spec              *TaskSpec       `json:"spec,omitempty"`
	ActionMode        string          `json:"action_mode,omitempty"` // host-selected and frozen, not a user switch.
	RequireSummary    bool            `json:"require_summary,omitempty"`
	TimeoutSeconds    *int64          `json:"timeout_seconds,omitempty"`
	ParentDeadline    *time.Time      `json:"-"` // Trusted parent only; excluded from model/caller business input.
}
type Candidate struct {
	ID               string    `json:"id" gorm:"primaryKey;size:32"`
	RunID            string    `json:"run_id" gorm:"size:32;index"`
	ArtifactHash     string    `json:"artifact_hash" gorm:"size:64"`
	SourceID         string    `json:"source_id" gorm:"size:64"`
	ParentHash       string    `json:"parent_hash" gorm:"size:64"`
	ParentEpoch      uint64    `json:"parent_epoch"`
	ValidatorVersion string    `json:"validator_version" gorm:"size:64"`
	PolicyVersion    string    `json:"policy_version" gorm:"size:64"`
	ChecksJSON       string    `json:"checks_json" gorm:"type:longtext"`
	ResultJSON       string    `json:"result_json" gorm:"type:longtext"`
	ResultHash       string    `json:"result_hash" gorm:"size:64"`
	CreatedAt        time.Time `json:"created_at"`
}
type Service struct {
	Data                *agentdata.Store
	Knowledge           *knowledge.Store
	Model               ChatProvider
	AuxModel            ChatProvider
	Search              SearchProvider
	Runner              *RunnerClient
	Retriever           *knowledge.Retriever
	LoopEmbedding       knowledge.Embedder
	Fingerprint         string
	ConfigurationErrors map[string]string
}

func NewService(db *gorm.DB) *Service {
	return &Service{Data: agentdata.NewStore(db), Knowledge: knowledge.NewStore(db), Fingerprint: "unconfigured"}
}

func (s *Service) Migrate() error {
	if err := s.Data.Migrate(); err != nil {
		return err
	}
	if err := s.Knowledge.Migrate(); err != nil {
		return err
	}
	return agentdata.AutoMigrate(s.Data.DB, &Candidate{}, &HumanIntervention{}, &RecordReplacement{}, &repository.CrawlResult{})
}

func (s *Service) CancelWithDelivery(ctx context.Context, id string) error {
	return s.Data.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var r agentdata.Run
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", id).First(&r).Error; err != nil {
			return err
		}
		if r.IsTerminal() {
			return agentdata.ErrConflict
		}
		return tx.Model(&agentdata.Run{}).Where("id = ?", id).Updates(map[string]any{"status": "cancelled", "cancelled": true, "token": "", "lease_until": nil}).Error
	})
}
// ConfiguredStreamMode reports whether streaming chat completions are enabled via GORELAY_AI_STREAM.
func ConfiguredStreamMode() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("GORELAY_AI_STREAM"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func (s *Service) Configure() {
	s.ConfigurationErrors = map[string]string{}
	s.Model, s.AuxModel, s.Search, s.Runner, s.Retriever = nil, nil, nil, nil, nil
	s.LoopEmbedding = nil
	streamEnabled := ConfiguredStreamMode()
	if os.Getenv("GORELAY_AI_BACKEND") == "codex_cli" {
		if os.Getenv("GORELAY_ENV") == "production" {
			s.ConfigurationErrors["chat"] = "Codex CLI is available only for local experiments"
		} else if p, err := NewCodexProvider(os.Getenv("GORELAY_CODEX_BIN"), envDefault("GORELAY_AI_MODEL", "gpt-6.1-sol"), envDefault("GORELAY_CODEX_EVIDENCE_DIR", "output/codex-inference")); err == nil {
			s.Model = p
		} else {
			s.ConfigurationErrors["chat"] = err.Error()
		}
	} else if p, err := NewModelProvider(ProviderConfig{BaseURL: os.Getenv("GORELAY_AI_BASE_URL"), Model: os.Getenv("GORELAY_AI_MODEL"), APIKey: os.Getenv("GORELAY_AI_API_KEY"), Stream: streamEnabled}); err == nil {
		s.Model = p
	} else {
		s.ConfigurationErrors["chat"] = err.Error()
	}
	if auxModel := strings.TrimSpace(os.Getenv("GORELAY_AI_AUX_MODEL")); auxModel != "" {
		if p, err := NewModelProvider(ProviderConfig{
			BaseURL: envDefault("GORELAY_AI_AUX_BASE_URL", os.Getenv("GORELAY_AI_BASE_URL")),
			Model:   auxModel,
			APIKey:  envDefault("GORELAY_AI_AUX_API_KEY", os.Getenv("GORELAY_AI_API_KEY")),
			Stream:  streamEnabled,
		}); err == nil {
			s.AuxModel = p
		}
	}
	searchConfig := SearchConfig{Endpoint: os.Getenv("GORELAY_SEARCH_ENDPOINT"), APIKey: os.Getenv("GORELAY_SEARCH_API_KEY")}
	if os.Getenv("GORELAY_SEARCH_BACKEND") == "codex_cli" {
		if os.Getenv("GORELAY_ENV") == "production" {
			s.ConfigurationErrors["search"] = "Codex CLI is available only for local experiments"
		} else if p, err := NewCodexProvider(os.Getenv("GORELAY_CODEX_BIN"), envDefault("GORELAY_AI_MODEL", "gpt-6.1-sol"), envDefault("GORELAY_CODEX_SEARCH_EVIDENCE_DIR", "output/codex-search")); err == nil {
			s.Search = &CodexSearch{Transport: p}
		} else {
			s.ConfigurationErrors["search"] = err.Error()
		}
	} else if searchConfig.APIKey != "" {
		if p, err := NewBraveSearch(searchConfig); err == nil {
			s.Search = p
		} else {
			s.ConfigurationErrors["search"] = err.Error()
		}
	}
	if p, err := NewRunnerClient(os.Getenv("GORELAY_RUNNER_URL"), os.Getenv("GORELAY_RUNNER_TOKEN")); err == nil {
		s.Runner = p
	} else {
		s.ConfigurationErrors["runner"] = err.Error()
	}
	s.Fingerprint = EmbeddingFingerprint()
	embed, err := NewModelProvider(ProviderConfig{BaseURL: os.Getenv("GORELAY_EMBED_BASE_URL"), Model: os.Getenv("GORELAY_EMBED_MODEL"), APIKey: os.Getenv("GORELAY_EMBED_API_KEY")})
	if err == nil {
		s.LoopEmbedding = EmbeddingAdapter{Provider: embed}
	}
	vectors, e := knowledge.NewQdrant(os.Getenv("GORELAY_QDRANT_URL"), envDefault("GORELAY_QDRANT_COLLECTION", "gorelay"), os.Getenv("GORELAY_QDRANT_API_KEY"))
	if err == nil && e == nil {
		s.Retriever = &knowledge.Retriever{Store: s.Knowledge, Embed: EmbeddingAdapter{Provider: embed}, Vectors: vectors}
	}
	if err != nil {
		s.ConfigurationErrors["embedding"] = err.Error()
	}
	if e != nil {
		s.ConfigurationErrors["vectors"] = e.Error()
	}
}
func envDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
func EmbeddingFingerprint() string {
	size, overlap, err := ChunkingSettingsFromEnv()
	layout := fmt.Sprintf("rune-%d-overlap-%d-nonblank-v2", size, overlap)
	if err != nil {
		layout = "invalid-layout:" + os.Getenv("GORELAY_CHUNK_RUNES") + ":" + os.Getenv("GORELAY_CHUNK_OVERLAP")
	}
	return agentdata.Hash([]string{os.Getenv("GORELAY_EMBED_BASE_URL"), os.Getenv("GORELAY_EMBED_MODEL"), envDefault("GORELAY_EMBED_DIMENSIONS", "unspecified"), layout})
}

type EmbeddingAdapter struct{ Provider EmbeddingProvider }

func (a EmbeddingAdapter) EmbedTexts(ctx context.Context, texts []string) ([][]float32, error) {
	out, err := a.Provider.Embed(ctx, texts)
	return out.Vectors, err
}
func (a EmbeddingAdapter) EmbedMeasured(ctx context.Context, texts []string) (knowledge.EmbeddingMeasurement, error) {
	out, err := a.Provider.Embed(ctx, texts)
	result := knowledge.EmbeddingMeasurement{Vectors: out.Vectors, Model: out.Model, RequestID: out.RequestID, DurationMS: out.DurationMS}
	if out.Usage != nil {
		result.Usage = &knowledge.EmbeddingUsage{PromptTokens: out.Usage.PromptTokens, TotalTokens: out.Usage.TotalTokens}
	}
	return result, err
}
func (s *Service) Create(ctx context.Context, request CreateRunRequest) (agentdata.Run, error) {
	request.ActionMode = envDefault("GORELAY_ACTION_MODE", "code")
	if request.ActionMode != "code" && request.ActionMode != "per_tool" {
		return agentdata.Run{}, fmt.Errorf("GORELAY_ACTION_MODE must be code or per_tool")
	}
	request.Goal = strings.TrimSpace(request.Goal)
	if request.Goal == "" || request.RequestKey == "" || len(request.RequestKey) > 128 {
		return agentdata.Run{}, fmt.Errorf("goal/request_key missing or too long")
	}
	if request.RequireSummary || request.Spec != nil && (request.Spec.RequireSummary || len(request.Spec.Requirements) > 0) {
		return agentdata.Run{}, fmt.Errorf("crawler runs deliver records and acquisition capabilities; submit knowledge analysis or cited summaries through the separate knowledge workflow")
	}
	if request.Spec != nil {
		copy := *request.Spec
		if copy.ContractVersion == "" {
			copy.ContractVersion = "v2"
		}
		if request.RequireSummary {
			copy.RequireSummary = true
		}
		request.Spec = &copy
		if request.Spec.Goal != request.Goal {
			return agentdata.Run{}, fmt.Errorf("goal and spec goal differ")
		}
		if err := request.Spec.Validate(); err != nil {
			return agentdata.Run{}, err
		}
	}
	var prior agentdata.Run
	lookup := s.Data.DB.WithContext(ctx).Where("request_key = ?", request.RequestKey).First(&prior).Error
	if lookup == nil {
		var original CreateRunRequest
		if err := json.Unmarshal([]byte(prior.InputJSON), &original); err != nil {
			return prior, err
		}
		original.ActionMode = ""
		// Compare old requests in the current admission representation without
		// rewriting the stored historical contract or its original input.
		if original.Spec != nil && original.Spec.ContractVersion == "" {
			copy := *original.Spec
			copy.ContractVersion = "v2"
			original.Spec = &copy
		}
		compare := request
		original.ExecutionBrief = nil
		compare.ExecutionBrief = nil
		compare.ActionMode = ""
		if agentdata.Hash(original) != agentdata.Hash(compare) {
			return prior, agentdata.ErrConflict
		}
		return s.Data.RestoreSubmittedContract(ctx, prior.ID)
	}
	if !errors.Is(lookup, gorm.ErrRecordNotFound) {
		return prior, lookup
	}
	input, _ := json.Marshal(request)
	limits, err := configuredBudgetLimits()
	if err != nil {
		return agentdata.Run{}, err
	}
	workTimeout, err := programrunner.ConfiguredRunTimeout()
	if err != nil {
		return agentdata.Run{}, err
	}
	workTimeout, err = agentstatus.RequestTimeout(request.TimeoutSeconds, workTimeout)
	if err != nil {
		return agentdata.Run{}, err
	}
	promptPolicy, err := configuredPromptPolicy()
	if err != nil {
		return agentdata.Run{}, err
	}
	programLimits := programLimitsForRun(agentdata.Run{})
	var deadline time.Time
	if workTimeout > 0 {
		deadline = time.Now().Add(workTimeout)
	}
	if request.ParentDeadline != nil && !request.ParentDeadline.IsZero() && (deadline.IsZero() || request.ParentDeadline.Before(deadline)) {
		deadline = *request.ParentDeadline
	}
	snapshot := BudgetSnapshot{LoopVersion: 5, PromptPolicy: promptPolicy, ProgramLimits: &programLimits, Limits: limits, Candidates: map[string]int{}, Deadline: deadline}
	budget, _ := json.Marshal(snapshot)
	r, err := s.Data.CreateRun(ctx, request.RequestKey, request.Goal, string(input), string(budget), snapshot.Deadline)
	if err != nil {
		return r, err
	}
	return r, nil
}

