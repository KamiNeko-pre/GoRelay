package aiapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/agentstatus"
	"go-lua-crawler/internal/knowledge"
	"gorm.io/gorm"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type CreateRunRequest struct {
	RequestKey     string              `json:"request_key"`
	Goal           string              `json:"goal"`
	SourceScope    []string            `json:"source_scope,omitempty"`
	EntryURLs      []string            `json:"entry_urls,omitempty"`
	Spec           *TaskSpec           `json:"spec,omitempty"`
	ActionMode     string              `json:"action_mode,omitempty"` // host-selected and frozen, not a user switch.
	Experiment     *ExperimentControls `json:"experiment,omitempty"`
	RequireSummary bool                `json:"require_summary,omitempty"`
	TimeoutSeconds *int64              `json:"timeout_seconds,omitempty"`
	ParentDeadline *time.Time          `json:"-"` // Trusted parent only; excluded from model/caller business input.
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
	ChecksJSON       string    `json:"checks_json" gorm:"type:text"`
	ResultJSON       string    `json:"result_json" gorm:"type:longtext"`
	ResultHash       string    `json:"result_hash" gorm:"size:64"`
	CreatedAt        time.Time `json:"created_at"`
}
type Service struct {
	RequireSpendPolicy  bool
	Data                *agentdata.Store
	Knowledge           *knowledge.Store
	Model               ChatProvider
	Search              SearchProvider
	Runner              *RunnerClient
	Retriever           *knowledge.Retriever
	LoopEmbedding       knowledge.Embedder
	Decide              func(context.Context, LoopState) (NextAction, error)
	Admission           func(context.Context, AdmissionContext) (AdmissionDecision, error)
	ReadOnly            func(context.Context, ToolCall, EvidenceReader) (ToolResult, error)
	Fingerprint         string
	ConfigurationErrors map[string]string
	readiness           *modelReadinessState
}

func NewService(db *gorm.DB) *Service {
	return &Service{Data: agentdata.NewStore(db), Knowledge: knowledge.NewStore(db), Decide: RunTaskLoop, Admission: RegisterValidatedSource, ReadOnly: ExecuteReadOnlyCall, Fingerprint: "unconfigured"}
}
func (s *Service) Configure() {
	s.ConfigurationErrors = map[string]string{}
	s.readiness = &modelReadinessState{}
	s.Model, s.Search, s.Runner, s.Retriever = nil, nil, nil, nil
	s.LoopEmbedding = nil
	s.RequireSpendPolicy = os.Getenv("GORELAY_AI_BACKEND") != "codex_cli"
	if os.Getenv("GORELAY_AI_BACKEND") == "codex_cli" {
		if os.Getenv("GORELAY_ENV") == "production" {
			s.ConfigurationErrors["chat"] = "Codex CLI is available only for local experiments"
		} else if p, err := NewCodexProvider(os.Getenv("GORELAY_CODEX_BIN"), envDefault("GORELAY_AI_MODEL", "gpt-6.1-sol"), envDefault("GORELAY_CODEX_EVIDENCE_DIR", "output/codex-inference")); err == nil {
			s.Model = p
		} else {
			s.ConfigurationErrors["chat"] = err.Error()
		}
	} else if p, err := NewModelProvider(ProviderConfig{BaseURL: os.Getenv("GORELAY_AI_BASE_URL"), Model: os.Getenv("GORELAY_AI_MODEL"), APIKey: os.Getenv("GORELAY_AI_API_KEY")}); err == nil {
		s.Model = p
	} else {
		s.ConfigurationErrors["chat"] = err.Error()
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
	} else if p, err := NewBraveSearch(searchConfig); err == nil {
		s.Search = p
	} else {
		s.ConfigurationErrors["search"] = err.Error()
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
	if request.Goal == "" || len(request.Goal) > 8000 || request.RequestKey == "" || len(request.RequestKey) > 128 {
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
		original.Experiment = nil
		// Compare old requests in the current admission representation without
		// rewriting the stored historical contract or its original input.
		if original.Spec != nil && original.Spec.ContractVersion == "" {
			copy := *original.Spec
			copy.ContractVersion = "v2"
			original.Spec = &copy
		}
		compare := request
		compare.ActionMode = ""
		compare.Experiment = nil
		if agentdata.Hash(original) != agentdata.Hash(compare) {
			return prior, agentdata.ErrConflict
		}
		return s.Data.RestoreSubmittedContract(ctx, prior.ID)
	}
	if !errors.Is(lookup, gorm.ErrRecordNotFound) {
		return prior, lookup
	}
	controls, err := s.captureExperiment(ctx)
	if err != nil {
		return agentdata.Run{}, err
	}
	request.Experiment = &controls
	input, _ := json.Marshal(request)
	limits, err := configuredBudgetLimits()
	if err != nil {
		return agentdata.Run{}, err
	}
	spend, err := spendPolicyFromEnv()
	if err != nil {
		return agentdata.Run{}, err
	}
	if s.RequireSpendPolicy && spend == nil {
		return agentdata.Run{}, fmt.Errorf("%w: native API runs require GORELAY_SPEND_POLICY_PATH", agentdata.ErrSpendAdmission)
	}
	if err := s.PreflightModel(ctx); err != nil {
		return agentdata.Run{}, err
	}
	workTimeout, err := configuredRunTimeout()
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
	programLimits, err := ConfiguredProgramLimits()
	if err != nil {
		return agentdata.Run{}, err
	}
	var deadline time.Time
	if workTimeout > 0 {
		deadline = time.Now().Add(workTimeout)
	}
	if request.ParentDeadline != nil && !request.ParentDeadline.IsZero() && (deadline.IsZero() || request.ParentDeadline.Before(deadline)) {
		deadline = *request.ParentDeadline
	}
	snapshot := BudgetSnapshot{LoopVersion: 5, PromptPolicy: promptPolicy, ProgramLimits: &programLimits, Spend: spend, Limits: limits, Candidates: map[string]int{}, Deadline: deadline, Unlimited: os.Getenv("GORELAY_EXPERIMENT_UNLIMITED_BUDGET") == "1"}
	budget, _ := json.Marshal(snapshot)
	limit, err := strconv.Atoi(envDefault("GORELAY_DEMO_DAILY_RUNS", "0"))
	if err != nil || limit < 0 {
		return agentdata.Run{}, fmt.Errorf("GORELAY_DEMO_DAILY_RUNS must be a nonnegative integer")
	}
	r, err := s.Data.CreateRunLimited(ctx, request.RequestKey, request.Goal, string(input), string(budget), snapshot.Deadline, limit, time.Now())
	if err != nil {
		return r, err
	}
	return r, nil
}
func (s *Service) state(ctx context.Context, r agentdata.Run) (LoopState, error) {
	state := LoopState{Run: r}
	if r.HostProgressJSON != "" {
		if err := json.Unmarshal([]byte(r.HostProgressJSON), &state.HostProgress); err != nil {
			return state, err
		}
	}
	if r.SpecJSON != "" {
		var spec TaskSpec
		if err := json.Unmarshal([]byte(r.SpecJSON), &spec); err != nil {
			return state, err
		}
		state.Spec = &spec
	}
	if r.PlanJSON != "" {
		if err := json.Unmarshal([]byte(r.PlanJSON), &state.Messages); err != nil {
			return state, err
		}
	}
	if err := json.Unmarshal([]byte(r.BudgetJSON), &state.Budget); err != nil {
		return state, err
	}
	var err error
	state.Children, err = s.Data.Children(ctx, r.ID)
	if err != nil {
		return state, err
	}
	state.Operations, err = s.Data.Operations(ctx, r.ID)
	if err != nil {
		return state, err
	}
	state.Progress, err = s.currentProgress(ctx, r)
	if err != nil {
		return state, err
	}
	set := evidenceFromOperations(r.ID, state.Operations)
	set, err = (knowledge.SetReader{Store: s.Knowledge, Set: set}).CurrentSet(ctx)
	if err != nil {
		return state, err
	}
	state.EvidenceCatalog = evidenceCatalog(set)
	state.TaskResults, err = s.childResultsForLinks(ctx, state.Children)
	if err != nil {
		return state, err
	}
	if state.Spec != nil {
		result, e := s.evaluateChildResults(ctx, r, *state.Spec, state.TaskResults, state.Operations)
		if e != nil {
			return state, e
		}
		state.Result = &result
	}
	return state, err
}
func (s *Service) Advance(ctx context.Context, id string) error {
	const leaseTTL = 2 * time.Minute
	current, err := s.Data.Run(ctx, id)
	if err != nil {
		return err
	}
	if current.Expired(time.Now()) && !current.IsTerminal() {
		return s.expireWithDelivery(ctx, id)
	}
	if contractAwaitingConfirmation(current) {
		return ErrContractConfirmation
	}
	r, err := s.Data.Claim(ctx, id, leaseTTL)
	if err != nil {
		return err
	}
	ctx, stopLease := keepLease(ctx, leaseTTL/3, func(ctx context.Context) error {
		return s.Data.RenewRunLease(ctx, id, r.Token, leaseTTL)
	})
	defer stopLease()
	round := LoopRound{RoundID: agentdata.Token(), Stage: "started", StartedAt: time.Now(), StatusBefore: r.Status, Tools: []LoopToolObservation{}}
	if err := s.Data.AddTrace(ctx, id, "loop_round", round); err != nil {
		return err
	}
	state, err := s.executionState(ctx, r)
	if err != nil {
		return s.saveBoundary(ctx, r, "failed", err, &round)
	}
	r = state.Run
	round.BudgetBefore = &state.Budget.Spent
	round.CoverageBefore = loopCoverage(state.Result)
	if r.Expired(time.Now()) {
		return s.saveBoundary(ctx, r, "expired", fmt.Errorf("run deadline"), &round)
	}
	if handled, err := s.advanceMonitorPlan(ctx, r, state, &round); handled {
		return err
	}
	action, err := s.Decide(ctx, state)
	round.Action, round.Reason, round.RequestedStatus, round.ModelAttempt = action.Kind, action.Reason, action.Status, action.ModelAttempt
	if err != nil {
		status := "failed"
		if errors.Is(err, ErrCorePending) {
			status = "blocked_core"
		}
		return s.saveBoundary(ctx, r, status, err, &round)
	}
	switch action.Kind {
	case "model":
		return s.think(ctx, state, action.ModelAttempt, &round)
	case "tools":
		pending := map[string]ToolCall{}
		if len(state.Messages) > 0 {
			for _, c := range state.Messages[len(state.Messages)-1].ToolCalls {
				pending[c.ID] = c
			}
		}
		if len(pending) != len(action.Calls) {
			return s.saveBoundary(ctx, r, "failed", fmt.Errorf("all proposed tool calls need exactly one reply"), &round)
		}
		seen := map[string]bool{}
		var providerStop error
		for _, call := range action.Calls {
			original, ok := pending[call.ID]
			if !ok || seen[call.ID] || original.Function != call.Function {
				return s.saveBoundary(ctx, r, "failed", fmt.Errorf("tool call is not from the current model proposal"), &round)
			}
			seen[call.ID] = true
			var result ToolResult
			var toolErr error
			if providerStop != nil {
				result, toolErr = s.replyAfterProviderStop(ctx, r, call)
			} else {
				// Visibility only controls saved-receipt replay after compaction.
				// The same normalized business request is never executed again.
				result, toolErr = s.executeDurableToolWithVisibility(ctx, r, call, visibleReceiptsOf(state.Messages))
			}
			observation := LoopToolObservation{CallID: call.ID, Name: call.Function.Name}
			if toolErr != nil {
				observation.Error = toolErr.Error()
			}
			round.Tools = append(round.Tools, observation)
			if errors.Is(toolErr, ErrCorePending) {
				return s.saveBoundary(ctx, r, "blocked_core", toolErr, &round)
			}
			result = toolFailureReceipt(call, result, toolErr)
			var refusal *ProviderError
			if errors.Is(toolErr, agentdata.ErrSpendAdmission) || errors.Is(toolErr, ErrProviderQuota) || (errors.As(toolErr, &refusal) && !refusal.Retryable) {
				providerStop = toolErr
			}
			data, _ := json.Marshal(modelObservationForRun(r, result, state.Spec))
			state.Messages = append(state.Messages, Message{Role: "tool", ToolCallID: call.ID, Content: string(data)})
		}
		data, _ := json.Marshal(state.Messages)
		if providerStop != nil {
			return s.stopUnavailableProvider(ctx, r, providerStop, &round, string(data))
		}
		current, err := s.Data.Run(ctx, r.ID)
		if err != nil {
			return err
		}
		if contractAwaitingConfirmation(current) {
			return s.saveLoopRun(ctx, r, map[string]any{"status": "needs_input", "plan_json": string(data), "last_error": ErrContractConfirmation.Error()}, &round, nil)
		}
		return s.saveLoopRun(ctx, r, map[string]any{"status": "reviewing", "plan_json": string(data)}, &round, nil)
	case "wait":
		wake := action.WakeAt
		if wake == nil {
			until := time.Now().Add(5 * time.Second)
			wake = &until
		}
		return s.saveLoopRun(ctx, r, map[string]any{"status": "waiting", "next_wake_at": wake}, &round, nil)
	case "finish":
		result, err := s.Evaluate(ctx, r)
		if err != nil {
			return s.saveBoundary(ctx, r, "failed", err, &round)
		}
		if action.Status == "completed" && !taskDeliverySatisfied(&result) {
			round.CompletionRejected = true
			return s.saveBoundary(ctx, r, "reviewing", fmt.Errorf("completion rejected: required facts or cited summary missing/conflicted"), &round)
		}
		if action.Status != "completed" && action.Status != "partial" && action.Status != "failed" {
			return s.saveBoundary(ctx, r, "failed", fmt.Errorf("%w: invalid completion status", errDeliveryRejected), &round)
		}
		if action.Status == "partial" && result.Coverage.PartialValidRecords == 0 {
			return s.saveBoundary(ctx, r, "failed", fmt.Errorf("%w: partial needs valid delivered records", errDeliveryRejected), &round)
		}
		payload, _ := json.Marshal(result)
		if action.HostStop && action.Status != "completed" && action.Reason != "" {
			attachStoppedDelivery(&result, action.Reason, state.Operations)
			payload, _ = json.Marshal(result)
		}
		return s.saveLoopRun(ctx, r, map[string]any{"status": action.Status, "result_json": string(payload)}, &round, nil)
	default:
		return s.saveBoundary(ctx, r, "failed", fmt.Errorf("%w: unknown next action kind", errDeliveryRejected), &round)
	}
}

// errDeliveryRejected marks a failure that came from the caller's own proposal
// being refused by a host check, not from an external stop. The distinction is
// the whole point of attachStoppedDelivery: an external stop must still hand
// back whatever material was already saved, while a rejected proposal must not
// fabricate a delivery out of it. A rejected proposal has no accepted work to
// report, so its ResultJSON stays empty and the refusal is visible as an API
// error plus the run's last_error.
var errDeliveryRejected = errors.New("delivery proposal rejected")

func (s *Service) saveBoundary(ctx context.Context, r agentdata.Run, status string, cause error, round *LoopRound) error {
	updates := map[string]any{"status": status, "last_error": cause.Error()}
	// Only stops that actually accumulated material produce a delivered result.
	// "reviewing" is a retry, and a rejected proposal carries nothing to project.
	carriesMaterial := status == "failed" || status == "expired"
	if carriesMaterial && !errors.Is(cause, errDeliveryRejected) {
		result, e := s.Evaluate(ctx, r)
		if e != nil {
			_ = json.Unmarshal([]byte(r.ResultJSON), &result)
			result.Gaps = append(result.Gaps, "结果核对未完成："+e.Error())
		}
		ops, e := s.Data.Operations(ctx, r.ID)
		if e != nil {
			result.Gaps = append(result.Gaps, "停止时无法读取调查材料："+e.Error())
		}
		attachStoppedDelivery(&result, cause.Error(), ops)
		payload, _ := json.Marshal(result)
		updates["result_json"] = string(payload)
	}
	if err := s.saveLoopRun(ctx, r, updates, round, cause); err != nil {
		return err
	}
	s.Data.AddTrace(ctx, r.ID, status, map[string]string{"error": cause.Error()})
	return cause
}
func (s *Service) charge(ctx context.Context, r agentdata.Run, key, inputHash, kind string, cost CallCost, quotes ...*agentdata.SpendQuote) (agentdata.Operation, bool, error) {
	var quote *agentdata.SpendQuote
	if len(quotes) > 0 {
		quote = quotes[0]
	}
	return s.Data.ReserveOperationWithSpend(ctx, r.ID, r.Token, key, inputHash, kind, func(raw string) (string, error) {
		var snapshot BudgetSnapshot
		if err := json.Unmarshal([]byte(raw), &snapshot); err != nil {
			return "", err
		}
		b, err := NewRunBudget(snapshot)
		if err != nil {
			return "", err
		}
		if err = b.Reserve(cost, time.Now()); err != nil {
			return "", err
		}
		encoded, err := json.Marshal(b.Snapshot())
		return string(encoded), err
	}, quote)
}
func (s *Service) think(ctx context.Context, state LoopState, attempt int, round *LoopRound) error {
	r := state.Run
	if s.Model == nil {
		return s.saveBoundary(ctx, r, "blocked_configuration", fmt.Errorf("chat provider is not configured"), round)
	}
	messages := state.Messages
	// Replay identity follows semantic inputs, not accounting rows or the host
	// snapshot added while assembling this request.
	inputHash := agentdata.Hash(map[string]any{"input": r.InputJSON, "spec": r.SpecJSON, "progress": r.ProgressJSON, "history": logicalHistory(state, messages), "tasks": loopTaskFacts(state)})
	if state.HostProgress.Recovery.Phase != "" {
		inputHash = agentdata.Hash([]string{inputHash, state.HostProgress.Recovery.Phase})
	}
	messages, compaction, err := compactContextMeasured(state, messages)
	if err != nil {
		return s.saveBoundary(ctx, r, "failed", err, round)
	}
	key := agentdata.Hash([]any{"model", inputHash})
	if attempt < 0 || attempt > 2 {
		return s.saveBoundary(ctx, r, "failed", fmt.Errorf("at most three explicit attempts for one model request"), round)
	}
	if attempt > 0 {
		key = agentdata.Hash([]any{"model", inputHash, attempt})
	}
	request := ChatRequest{Messages: messages, Tools: s.configuredToolDefinitionsForState(state), MaxCompletionTokens: completionTokens(state), ReasoningEffort: reasoningEffort(state)}
	var quote *agentdata.SpendQuote
	saved := false
	if state.Budget.Spend != nil || s.RequireSpendPolicy {
		saved, err = s.hasSavedOperation(ctx, r.ID, key)
		if err != nil {
			return err
		}
	}
	if !saved {
		if s.RequireSpendPolicy && state.Budget.Spend == nil {
			return s.stopUnavailableProvider(ctx, r, fmt.Errorf("%w: run has no frozen spend policy", agentdata.ErrSpendAdmission), round, "")
		}
		quote, err = quoteModel(s.Model, request, state.Budget.Spend)
		if err != nil {
			return s.stopUnavailableProvider(ctx, r, err, round, "")
		}
	}
	op, fresh, err := s.charge(ctx, r, key, inputHash, "model", CallCost{ModelCalls: 1}, quote)
	if err != nil {
		if errors.Is(err, agentdata.ErrSpendAdmission) {
			return s.stopUnavailableProvider(ctx, r, err, round, "")
		}
		return s.saveBoundary(ctx, r, "failed", err, round)
	}
	if !fresh {
		if op.Stage != "succeeded" {
			return s.saveBoundary(ctx, r, "reviewing", fmt.Errorf("unknown external model attempt; usage remains unknown"), round)
		}
		var response ChatResponse
		if err = json.Unmarshal([]byte(op.ResultJSON), &response); err != nil {
			return err
		}
		messages = append(messages, response.Message)
	} else {
		requestCtx, cancelRequest := deliveryRequestContext(ctx, r)
		response, err := s.Model.Chat(requestCtx, request)
		cancelRequest()
		if err == nil && state.Budget.LoopVersion >= 5 {
			err = rejectCommittedToolCallIDs(state, response.Message)
		}
		response.ObservedChildren = terminalChildFingerprint(state)
		response.DeliveryRecoveryPhase = state.HostProgress.Recovery.Phase
		response.ContextProfile = promptContextProfile(request)
		response.ContextProfile.Compaction = compaction
		response.Failure = captureFailure(err, attempt, true)
		if state.Budget.LoopVersion >= 5 && errors.Is(err, ErrInvalidProposal) {
			// A received but malformed proposal has no dispatched tool effects.
			// Give bounded correction opportunities; auth/quota/permission and
			// unknown external outcomes retain their own recovery contracts.
			response.Failure = rejectedProposalFeedback(err, response, request)
		}
		raw, _ := json.Marshal(response)
		usage, _ := json.Marshal(response.Usage)
		stage := "succeeded"
		if err != nil {
			stage = "unknown_external"
			var refusal *ProviderError
			if errors.Is(err, ErrProviderQuota) || errors.Is(err, ErrInferenceBoundary) || errors.Is(err, ErrInvalidProposal) || errors.As(err, &refusal) {
				stage = "failed" // A provider refusal is observed; usage can still be unknown.
			}
		}
		if saveErr := s.Data.CompleteOperation(ctx, r.ID, r.Token, op.ID, stage, string(raw), string(usage)); saveErr != nil {
			return saveErr
		}
		if err != nil {
			if response.Failure.Kind == "rejected_proposal" {
				return s.saveLoopRun(ctx, r, map[string]any{"status": "reviewing", "last_error": err.Error()}, round, err)
			}
			if response.Failure.stops() {
				return s.stopUnavailableProvider(ctx, r, err, round, "")
			}
			return s.scheduleProviderRetry(ctx, r, err, attempt, round, *response.Failure.RetryAt)
		}
		messages = append(messages, response.Message)
	}
	data, _ := json.Marshal(messages)
	return s.saveLoopRun(ctx, r, map[string]any{"status": "reviewing", "plan_json": string(data), "last_error": ""}, round, nil)
}

func (s *Service) stopUnavailableProvider(ctx context.Context, r agentdata.Run, cause error, round *LoopRound, plan string) error {
	result, err := s.Evaluate(ctx, r)
	if err != nil {
		result = TaskResult{}
	}
	status := "failed"
	if result.Coverage.PartialValidRecords > 0 {
		status = "partial"
	}
	ops, readErr := s.Data.Operations(ctx, r.ID)
	if readErr != nil {
		result.Gaps = append(result.Gaps, "停止时无法读取调查材料："+readErr.Error())
	}
	attachStoppedDelivery(&result, cause.Error(), ops)
	payload, _ := json.Marshal(result)
	updates := map[string]any{"status": status, "last_error": cause.Error(), "result_json": string(payload)}
	if plan != "" {
		updates["plan_json"] = plan
	}
	return s.saveLoopRun(ctx, r, updates, round, cause)
}
func (s *Service) Run(ctx context.Context) error {
	workers, err := strconv.Atoi(envDefault("GORELAY_AGENT_CONCURRENCY", "2"))
	if err != nil || workers < 1 || workers > 8 {
		return fmt.Errorf("GORELAY_AGENT_CONCURRENCY must be 1..8")
	}
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() { cancel(); wg.Wait() }()
	slots := make(chan struct{}, workers)
	active := map[string]bool{}
	var mu sync.Mutex
	timer := time.NewTicker(time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			if err := s.sweepExpiredDeliveries(ctx); err != nil {
				return err
			}
			if err := s.SweepMonitors(ctx); err != nil {
				return err
			}
			runs, err := s.Data.Pending(ctx, 8)
			if err != nil {
				return err
			}
			for _, r := range runs {
				mu.Lock()
				busy := active[r.ID]
				mu.Unlock()
				if busy {
					continue
				}
				select {
				case slots <- struct{}{}:
				default:
					continue
				}
				mu.Lock()
				active[r.ID] = true
				mu.Unlock()
				wg.Add(1)
				go func(id string) {
					defer wg.Done()
					defer func() { <-slots; mu.Lock(); delete(active, id); mu.Unlock() }()
					if err := s.Advance(ctx, id); err != nil && !errors.Is(err, ErrCorePending) && !errors.Is(err, agentdata.ErrConflict) {
						s.Data.AddTrace(ctx, id, "advance_error", map[string]string{"error": err.Error()})
					}
				}(r.ID)
			}
		}
	}
}
