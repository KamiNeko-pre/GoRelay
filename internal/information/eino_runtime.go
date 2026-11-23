package information

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/adk/middlewares/summarization"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/agentstatus"
	"go-lua-crawler/internal/aiapp"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Payload is Eino's opaque checkpoint; SQL owns task identity and receipts.
type InformationCheckpoint struct {
	ID        string `gorm:"primaryKey;size:96"`
	RequestID string `gorm:"size:32;index"`
	Payload   []byte `gorm:"type:longblob"`
	UpdatedAt time.Time
}

func (InformationCheckpoint) TableName() string { return "information_eino_checkpoints" }
func informationCheckpointID(id string) string  { return "information:eino:v1:" + id }

type informationCheckpointStore struct{ host *informationHost }

func (s *informationCheckpointStore) Get(ctx context.Context, key string) (data []byte, found bool, err error) {
	err = s.host.owned(ctx, func(tx *gorm.DB) error {
		var row InformationCheckpoint
		e := tx.First(&row, "id = ? AND request_id = ?", key, s.host.owner.RequestID).Error
		if errors.Is(e, gorm.ErrRecordNotFound) {
			return nil
		}
		if e != nil {
			return e
		}
		data, found = row.Payload, true
		return nil
	})
	return
}
func (s *informationCheckpointStore) Set(ctx context.Context, key string, data []byte) error {
	return s.host.owned(ctx, func(tx *gorm.DB) error {
		return tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "id"}}, DoUpdates: clause.AssignmentColumns([]string{"payload", "updated_at"})}).Create(&InformationCheckpoint{ID: key, RequestID: s.host.owner.RequestID, Payload: data, UpdatedAt: time.Now()}).Error
	})
}

type batchCallGate struct {
	waits []<-chan struct{}
	done  chan struct{}
}

// owner is immutable: reading SQL cannot grant an old worker a new lease token.
type informationHost struct {
	*adk.BaseChatModelAgentMiddleware
	mu               sync.Mutex
	service          *Service
	summary          adk.ChatModelAgentMiddleware
	owner            requestExecutionOwner
	request          Request
	input            Input
	pause            *informationWait
	finished         bool
	replay           *Step
	invalidProposals int
	currentModelStep uint64
	modelCursor      uint64
	batchGates       map[string]batchCallGate
	outputCounts     *automaticOutputCounts
}
type informationDependency struct {
	Terminal bool   `json:"terminal,omitempty"`
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	Version  string `json:"version"`
}
type informationWait struct {
	Stage        string                  `json:"stage"`
	Reason       string                  `json:"reason"`
	Dependencies []informationDependency `json:"dependencies,omitempty"`
	WakeAt       *time.Time              `json:"wake_at,omitempty"`
}

const informationSQLCursor = "information_sql_model_cursor_v1"
const informationFactsPrefix = "HOST_INFORMATION_FACTS_V1\n"

func (h *informationHost) owned(ctx context.Context, fn func(*gorm.DB) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return h.service.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var r Request
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&r, "id = ?", h.owner.RequestID).Error; err != nil {
			return err
		}
		if r.LeaseToken != h.owner.Token || r.LeaseUntil == nil || !r.LeaseUntil.After(time.Now()) || r.Status == "cancelled" || r.Status == "expired" {
			return agentdata.ErrConflict
		}
		return fn(tx)
	})
}
func (h *informationHost) refresh(ctx context.Context) error {
	r, err := h.service.Request(ctx, h.owner.RequestID)
	if err != nil {
		return err
	}
	if r.LeaseToken != h.owner.Token || r.LeaseUntil == nil || !r.LeaseUntil.After(time.Now()) || r.Status == "cancelled" || r.Status == "expired" {
		return agentdata.ErrConflict
	}
	h.request = r
	if err = json.Unmarshal([]byte(r.InputJSON), &h.input); err != nil {
		return err
	}

	return nil
}
func (h *informationHost) saveState(ctx context.Context) error {
	return h.owned(ctx, func(tx *gorm.DB) error {
		return tx.Model(&Request{}).Where("id = ? AND lease_token = ?", h.owner.RequestID, h.owner.Token).Updates(map[string]any{"child_run_id": h.request.ChildRunID, "result_json": h.request.ResultJSON, "status_json": h.request.StatusJSON, "feedback_json": h.request.FeedbackJSON, "route_progress_json": h.request.RouteProgressJSON}).Error
	})
}
func (h *informationHost) event(ctx context.Context, kind, title, name, status string) error {
	return h.owned(ctx, func(tx *gorm.DB) error {
		data := map[string]string{"type": kind, "title": title, "tool": name, "status": status}
		return tx.Create(&Step{EinoEpoch: h.request.EinoEpoch, RequestID: h.owner.RequestID, Key: "agent_event:" + agentdata.Token(), Kind: "agent_event", Stage: status, ResultJSON: string(mustMarshal(data))}).Error
	})
}
func (h *informationHost) interrupt(ctx context.Context, w informationWait) error {
	h.mu.Lock()
	if h.pause != nil && len(w.Dependencies) > 0 && len(h.pause.Dependencies) > 0 {
		h.pause.Dependencies = append(h.pause.Dependencies, w.Dependencies...)
	} else {
		h.pause = &w
	}
	h.mu.Unlock()
	return compose.Interrupt(ctx, w.Reason)
}
func (h *informationHost) saveMessages(ctx context.Context, messages []*schema.Message) error {
	return h.owned(ctx, func(tx *gorm.DB) error {
		return tx.Model(&Request{}).Where("id = ? AND lease_token = ?", h.owner.RequestID, h.owner.Token).Updates(map[string]any{
			"eino_messages_json":  string(mustMarshal(aiapp.ChatMessages(messages))),
			"route_progress_json": h.request.RouteProgressJSON,
		}).Error
	})
}
func (h *informationHost) modelSteps(ctx context.Context, kind string) *gorm.DB {
	return h.service.DB.WithContext(ctx).Model(&Step{}).
		Where("request_id = ? AND eino_epoch = ? AND kind = ?", h.owner.RequestID, h.request.EinoEpoch, kind)
}

func (s *Service) advanceEino(parent context.Context, id string) error {
	r, err := s.Request(parent, id)
	if err != nil {
		return err
	}
	if terminal(r.Status) {
		return nil
	}
	if r.deadlineReached(time.Now()) {
		now := time.Now()
		q := s.DB.WithContext(parent).Model(&Request{}).Where("id = ? AND status NOT IN ? AND deadline IS NOT NULL AND deadline <= ? AND (lease_until IS NULL OR lease_until <= ?)", id, terminalStages(), now, now).Updates(map[string]any{"status": "expired", "stage": "expired", "last_error": "information request reached its caller deadline", "status_json": string(mustMarshal(agentstatus.Of(agentstatus.TaskDeadline))), "wake_at": nil, "wait_json": ""})
		if q.Error != nil || q.RowsAffected != 1 {
			return q.Error
		}
		return s.cancelDiscoveryWork(parent, r)
	}
	if ready, e := s.informationReady(parent, r); e != nil {
		return e
	} else if !ready {
		return nil
	}
	if r.Status == "needs_input" || r.Status == "failed" {
		return nil
	}
	token := agentdata.Token()
	now := time.Now()
	until := now.Add(2 * time.Minute)
	q := s.DB.WithContext(parent).Model(&Request{}).Where("id = ? AND (lease_until IS NULL OR lease_until <= ?) AND status IN ?", id, now, []string{"queued", "running", "waiting_dependency"}).Updates(map[string]any{"lease_token": token, "lease_until": until, "status": "running"})
	if q.Error != nil {
		return q.Error
	}
	if q.RowsAffected != 1 {
		return agentdata.ErrConflict
	}
	h := &informationHost{BaseChatModelAgentMiddleware: &adk.BaseChatModelAgentMiddleware{}, service: s, owner: requestExecutionOwner{id, token}}
	ctx, cancel := r.RequestContext(parent)
	ctx = context.WithValue(ctx, requestExecutionOwnerKey{}, h.owner)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				q := s.DB.WithContext(ctx).Model(&Request{}).Where("id = ? AND lease_token = ? AND stage NOT IN ?", id, token, []string{"cancelled", "expired"}).Update("lease_until", time.Now().Add(2*time.Minute))
				if q.Error != nil || q.RowsAffected != 1 {
					cancel()
					return
				}
			}
		}
	}()
	defer func() { cancel(); <-done }()
	defer func() {
		_ = s.DB.WithContext(context.WithoutCancel(parent)).Model(&Request{}).Where("id = ? AND lease_token = ?", id, token).Updates(map[string]any{"lease_token": "", "lease_until": nil}).Error
	}()
	if err = h.refresh(ctx); err != nil {
		return err
	}
	if err = h.saveState(ctx); err != nil {
		return err
	}
	policy, err := informationPromptPolicy(h.request)
	if err == nil && h.request.ContextPolicyJSON != string(mustMarshal(policy)) {
		frozen := string(mustMarshal(policy))
		err = h.owned(ctx, func(tx *gorm.DB) error {
			return tx.Model(&Request{}).Where("id = ?", id).Update("context_policy_json", frozen).Error
		})
		if err == nil {
			h.request.ContextPolicyJSON = frozen
		}
	}
	if err != nil {
		return h.finishInvocation(parent, err)
	}
	var thinkingCfg *aiapp.ThinkingConfig
	if policy.Thinking != "" {
		thinkingCfg = &aiapp.ThinkingConfig{Type: policy.Thinking}
	}
	provider := &informationProvider{host: h, purpose: "agent"}
	model := aiapp.NewEinoChatProviderModel(provider, aiapp.ChatRequest{MaxCompletionTokens: policy.MaxOutputTokens, ReasoningEffort: policy.ReasoningEffort, Thinking: thinkingCfg})
	summary, err := summarization.New(ctx, &summarization.Config{Model: aiapp.NewEinoChatProviderModel(&informationProvider{host: h, purpose: "summary"}, aiapp.ChatRequest{MaxCompletionTokens: policy.MaxOutputTokens, ReasoningEffort: policy.ReasoningEffort, Thinking: thinkingCfg}), Trigger: &summarization.TriggerCondition{ContextTokens: policy.CompactAtTokens}, UserInstruction: fmt.Sprintf("Summarize execution history within %d tokens. Preserve caller intent, approved contract, evidence references, source frontier, research batches, failures, and pending tool calls. Host facts are reinjected separately; do not invent completion.", policy.CompactToTokens)})
	if err != nil {
		return h.finishInvocation(parent, err)
	}
	h.summary = summary
	tools, err := h.tools(ctx)
	if err != nil {
		return h.finishInvocation(parent, err)
	}
	agent, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{Name: "information", Description: "Adaptive source-backed information acquisition and delivery", Model: model, ToolsConfig: adk.ToolsConfig{ToolsNodeConfig: compose.ToolsNodeConfig{Tools: tools, ExecuteSequentially: false, UnknownToolsHandler: h.unknownTool}}, Handlers: []adk.ChatModelAgentMiddleware{h}, ModelRetryConfig: &adk.ModelRetryConfig{MaxRetries: math.MaxInt, ShouldRetry: h.retryProposal}, MaxIterations: math.MaxInt})
	if err != nil {
		return h.finishInvocation(parent, err)
	}
	store := &informationCheckpointStore{host: h}
	checkpointID := informationCheckpointID(id)
	runner := adk.NewRunner(ctx, adk.RunnerConfig{Agent: agent, CheckPointStore: store})
	_, exists, err := store.Get(ctx, checkpointID)
	if err != nil {
		return err
	}
	opts := []adk.AgentRunOption{adk.WithCheckPointID(checkpointID), adk.WithAfterToolCallsHook(h.afterTools)}
	var iter *adk.AsyncIterator[*adk.AgentEvent]
	if exists {
		iter, err = runner.Resume(ctx, checkpointID, opts...)
	} else {
		var history []aiapp.Message
		if h.request.EinoMessagesJSON != "" {
			if err = json.Unmarshal([]byte(h.request.EinoMessagesJSON), &history); err != nil {
				return h.finishInvocation(parent, err)
			}
		}
		if len(history) == 0 {
			userGoal := strings.TrimSpace(h.input.Goal)
			if userGoal == "" {
				userGoal = "Fulfil the original request using the host facts and real domain tools."
			}
			history = []aiapp.Message{
				{Role: "system", Content: informationAgentInstructions},
				{Role: "user", Content: userGoal},
			}
		}
		// SQL may have committed the proposal before the framework saved a checkpoint.
		// Re-enter Eino before that assistant batch; durable exact-call receipts repair
		// partial dispatch without asking the model to recreate the paid proposal.
		for i := len(history) - 1; i >= 0; i-- {
			if history[i].Role != "assistant" {
				continue
			}
			if len(history[i].ToolCalls) > 0 {
				replied := map[string]bool{}
				for _, m := range history[i+1:] {
					if m.Role == "tool" {
						replied[m.ToolCallID] = true
					}
				}
				pending := false
				for _, c := range history[i].ToolCalls {
					pending = pending || !replied[c.ID]
				}
				if pending {
					history = history[:i]
				}
			}
			break
		}
		iter = runner.Run(ctx, aiapp.EinoMessages(history), opts...)
	}
	if err != nil {
		return h.finishInvocation(parent, err)
	}
	for {
		event, ok := iter.Next()
		if !ok {
			break
		}
		if event.Err != nil {
			var retry *adk.WillRetryError
			if errors.As(event.Err, &retry) {
				continue
			}
			err = event.Err
			break
		}
		if event.Action != nil && event.Action.Interrupted != nil {
			break
		}
	}
	return h.finishInvocation(parent, err)
}
func (h *informationHost) finishInvocation(ctx context.Context, cause error) error {
	if errors.Is(cause, agentdata.ErrConflict) || errors.Is(cause, context.Canceled) {
		return cause
	}
	stage := "failed"
	reason := "Agent returned without an accepted submit_delivery; saved facts remain available."
	if h.finished {
		stage = h.request.Stage
		reason = ""
	} else if h.request.deadlineReached(time.Now()) {
		stage, reason = "expired", "information request reached its caller deadline"
		h.request.StatusJSON = string(mustMarshal(agentstatus.Of(agentstatus.TaskDeadline)))
	} else if h.pause != nil {
		stage, reason = h.pause.Stage, h.pause.Reason
	} else if errors.Is(cause, errInformationProposal) {
		stage, reason = "failed", cause.Error()
	} else if cause != nil {
		stage, reason = "blocked_runtime", cause.Error()
		h.request.StatusJSON = string(mustMarshal(aiapp.OperationalStatus(cause)))
	}
	var wake *time.Time
	wait := ""
	if h.pause != nil && stage != "expired" {
		wake = h.pause.WakeAt
		wait = string(mustMarshal(h.pause))
	}
	err := h.owned(context.WithoutCancel(ctx), func(tx *gorm.DB) error {
		return tx.Model(&Request{}).Where("id = ? AND lease_token = ?", h.owner.RequestID, h.owner.Token).Updates(map[string]any{"status": lifecycleAfterInvocation(stage, h.finished, h.pause), "stage": stage, "last_error": reason, "wake_at": wake, "wait_json": wait, "child_run_id": h.request.ChildRunID, "result_json": h.request.ResultJSON, "status_json": h.request.StatusJSON, "feedback_json": h.request.FeedbackJSON, "route_progress_json": h.request.RouteProgressJSON}).Error
	})
	if err != nil {
		return err
	}
	if stage == "expired" {
		if err := h.service.cancelDiscoveryWork(context.WithoutCancel(ctx), h.request); err != nil {
			return errors.Join(cause, err)
		}
	}
	if errors.Is(cause, errInformationProposal) {
		return nil
	}
	return cause
}

const informationStagePrefix = "[STAGE "
const informationProposalDiagnosticPrefix = "Host rejected the tool proposal: "

// informationStableHistoryMessages preserves messages[0] as the single
// immutable system prompt and strips previous ephemeral runtime snapshots so
// historical user/assistant/tool turns remain byte-stable for KV prefix caching.
func informationStableHistoryMessages(history []*schema.Message) []*schema.Message {
	messages := make([]*schema.Message, 0, len(history)+1)
	messages = append(messages, schema.SystemMessage(informationAgentInstructions))
	replacedPolicy := false
	for _, m := range history {
		if m == nil {
			continue
		}
		if m.Role == schema.System {
			if strings.HasPrefix(m.Content, informationFactsPrefix) || strings.HasPrefix(m.Content, informationStagePrefix) || strings.HasPrefix(m.Content, "[STATUS:") || strings.HasPrefix(m.Content, loopCorrectionPrefix) {
				continue
			}
			// Eino stores the original host policy as the first system message.
			// Normalize any legacy proposal diagnostics to user role so messages[0]
			// stays the sole system message.
			if !replacedPolicy && !strings.HasPrefix(m.Content, informationProposalDiagnosticPrefix) {
				replacedPolicy = true
				continue
			}
			if strings.HasPrefix(m.Content, informationProposalDiagnosticPrefix) {
				messages = append(messages, schema.UserMessage(m.Content))
				continue
			}
		}
		if m.Role == schema.User && strings.HasPrefix(m.Content, informationFactsPrefix) {
			continue
		}
		messages = append(messages, m)
	}
	return messages
}

func buildRuntimeContextMessage(facts any, directive, correction string) *schema.Message {
	var b strings.Builder
	b.WriteString(informationFactsPrefix)
	b.Write(mustMarshal(facts))
	if directive != "" {
		b.WriteString("\n\n")
		b.WriteString(directive)
	}
	if correction != "" {
		b.WriteString("\n\n")
		b.WriteString(correction)
	}
	return schema.UserMessage(b.String())
}

func (h *informationHost) BeforeModelRewriteState(ctx context.Context, st *adk.ChatModelAgentState, mc *adk.ModelContext) (context.Context, *adk.ChatModelAgentState, error) {
	if err := h.refresh(ctx); err != nil {
		return ctx, st, err
	}
	h.mu.Lock()
	paused := h.pause
	finished := h.finished
	h.mu.Unlock()
	if paused != nil {
		return ctx, st, compose.Interrupt(ctx, paused.Reason)
	}
	if finished {
		return ctx, st, compose.Interrupt(ctx, "host accepted submit_delivery")
	}
	if err := h.prepareReplay(ctx, st.Messages); err != nil {
		return ctx, st, err
	}
	if h.replay == nil && h.service.Model == nil {
		return ctx, st, h.interrupt(ctx, informationWait{Stage: "blocked_provider", Reason: "Information model provider is unavailable; restore it and resume the original request."})
	}
	var correction string
	if h.replay == nil {
		var stop bool
		var err error
		correction, stop, err = h.reviewProgress(ctx)
		if err != nil {
			return ctx, st, err
		}
		if stop {
			return ctx, st, h.interrupt(ctx, informationWait{Stage: "failed", Reason: "After correction, the Agent repeated the same actions without obtaining new evidence. Existing results are preserved."})
		}
	}
	facts, err := h.facts(ctx)
	if err != nil {
		return ctx, st, err
	}
	scopedTools, directive := h.stateMachineTools(facts)
	st.Messages = informationStableHistoryMessages(st.Messages)
	st.ToolInfos, err = aiapp.EinoToolInfos(scopedTools)
	if err != nil {
		return ctx, st, err
	}
	// Refresh host policy before Eino summarizes; replay needs no inference.
	// Summarize pure historical turns before appending the volatile runtime
	// snapshot so live host facts/status remain intact at the message tail.
	if h.replay == nil && h.summary != nil {
		ctx, st, err = h.summary.BeforeModelRewriteState(ctx, st, mc)
		if err != nil {
			return ctx, st, err
		}
	}
	st.Messages = append(st.Messages, buildRuntimeContextMessage(facts, directive, correction))
	if err = h.saveMessages(ctx, st.Messages); err != nil {
		return ctx, st, err
	}
	return ctx, st, nil
}

func normalizeWorkPlanRoute(route string) string {
	switch strings.ToLower(strings.TrimSpace(route)) {
	case "api", "json", "rest", "graphql", "endpoint":
		return "api"
	case "lua", "script", "program", "crawl":
		return "lua"
	case "browser", "playwright", "dom", "css", "render", "rendered":
		return "browser"
	case "asset", "reuse", "published":
		return "asset"
	default:
		return "direct"
	}
}

func (h *informationHost) stateMachineTools(facts map[string]any) ([]aiapp.ToolDefinition, string) {
	allDefs := informationToolDefinitions()
	var obsCount, succeededDocCount int64
	if sf, ok := facts["saved_facts"].(map[string]any); ok {
		if o, ok := sf["observations"].(int64); ok {
			obsCount = o
		}
		if sd, ok := sf["succeeded_documents"].(int64); ok {
			succeededDocCount = sd
		}
	}

	var browserGrantCount int64
	if h.service != nil && h.service.DB != nil {
		_ = h.service.DB.Model(&BrowserGrant{}).Where("request_id = ?", h.request.ID).Count(&browserGrantCount).Error
	}

	var plan WorkPlan
	if h.request.PlanJSON != "" {
		_ = json.Unmarshal([]byte(h.request.PlanJSON), &plan)
	}
	route := normalizeWorkPlanRoute(plan.Route)
	allowBrowser := browserGrantCount > 0

	tools := make([]aiapp.ToolDefinition, 0, len(allDefs))
	for _, d := range allDefs {
		if d.Function.Name == "browser_extract" && !allowBrowser {
			continue
		}
		if d.Function.Name == "ask_user" && h.request.Purpose == "source_discovery" {
			continue
		}
		if d.Function.Name == "discover_sources" && h.request.Purpose == "source_discovery" && h.service != nil && h.service.Search == nil {
			d.Function.Description = "Look up stored source entries with saved_only=true. No external search provider is configured; query public indexes or directories via acquire_sources or run_lua to discover and verify candidate URLs."
			d.Function.Parameters = mustMarshal(informationObject(map[string]any{
				"saved_only": map[string]any{"type": "boolean", "enum": []bool{true}},
				"query":      map[string]any{"type": "string"},
				"offset":     map[string]any{"type": "integer", "minimum": 0},
				"limit":      map[string]any{"type": "integer", "minimum": 0},
			}, "saved_only"))
		}
		tools = append(tools, d)
	}

	if h.request.Purpose == "source_discovery" && h.request.PlanJSON == "" {
		return tools, "[STATUS: PLAN SOURCE DISCOVERY]\nIdentify and verify entry URLs using search, saved sources, or public indexes (acquire_sources / run_lua), then call submit_delivery with discovered_sources."
	}
	if h.request.PlanJSON == "" && obsCount == 0 && succeededDocCount == 0 {
		return tools, "[STATUS: PLAN & ACQUIRE]\nReuse matching saved assets, or plan and start independent API or Lua acquisition concurrently. Inspect only when a specific missing fact prevents acquisition. Acquire known entry URLs directly. Use discover_sources only for relevant entries that are still unknown."
	}
	if obsCount == 0 && succeededDocCount == 0 {
		if allowBrowser {
			return tools, fmt.Sprintf("[STATUS: ACQUIRE / RECOVER (route=%s, browser_unlocked=%v)]\nUse the saved response or tool diagnostics to repair HTTP config or Lua extraction. Use browser_extract only for URLs in browser_approved_urls.", route, browserGrantCount > 0)
		}
		return tools, fmt.Sprintf("[STATUS: ACQUIRE (route=%s)]\nExtract independent sources concurrently through a verified API or Lua. Probe only when a specific missing fact prevents extraction.", route)
	}
	return tools, fmt.Sprintf("[STATUS: MATERIALS READY (route=%s, observations=%d, captures=%d)]\nVerify extracted records satisfy the request; extract any remaining sources concurrently or call submit_delivery with outcome, report, and acquisition_receipt_ids.", route, obsCount, succeededDocCount)
}

func (h *informationHost) AfterModelRewriteState(ctx context.Context, st *adk.ChatModelAgentState, _ *adk.ModelContext) (context.Context, *adk.ChatModelAgentState, error) {
	if h.currentModelStep > 0 {
		if err := adk.SetRunLocalValue(ctx, informationSQLCursor, h.currentModelStep); err != nil {
			return ctx, st, err
		}
	}
	h.configureBatchGates(st.Messages)
	return ctx, st, h.saveMessages(ctx, st.Messages)
}

func (h *informationHost) configureBatchGates(messages []*schema.Message) {
	var gates map[string]batchCallGate
	if n := len(messages); n > 0 && messages[n-1] != nil && messages[n-1].Role == schema.Assistant && len(messages[n-1].ToolCalls) > 1 {
		calls := messages[n-1].ToolCalls
		var planDones, barrierDones, stopChs []<-chan struct{}
		needsOrder := false
		for _, tc := range calls {
			switch tc.Function.Name {
			case "update_plan", "ask_user", "submit_delivery":
				needsOrder = true
			}
		}
		if needsOrder {
			gates = make(map[string]batchCallGate, len(calls))
			for _, tc := range calls {
				done := make(chan struct{})
				var waits []<-chan struct{}
				waits = append(waits, stopChs...)
				switch tc.Function.Name {
				case "ask_user", "submit_delivery":
					waits = append(waits, barrierDones...)
				case "update_plan":
					// Runs upfront unless preceded by an earlier ask_user.
				default:
					waits = append(waits, planDones...)
				}
				gates[tc.ID] = batchCallGate{waits: waits, done: done}
				barrierDones = append(barrierDones, done)
				if tc.Function.Name == "update_plan" {
					planDones = append(planDones, done)
				}
				if tc.Function.Name == "ask_user" || tc.Function.Name == "submit_delivery" {
					stopChs = append(stopChs, done)
				}
			}
		}
	}
	h.mu.Lock()
	h.batchGates = gates
	h.mu.Unlock()
}
func (h *informationHost) afterTools(ctx context.Context) error {
	var messages []*schema.Message
	if err := compose.ProcessState(ctx, func(_ context.Context, st *adk.State) error { messages = st.Messages; return nil }); err != nil {
		return err
	}
	return h.saveMessages(ctx, messages)
}
func (h *informationHost) prepareReplay(ctx context.Context, messages []*schema.Message) error {
	value, found, err := adk.GetRunLocalValue(ctx, informationSQLCursor)
	if err != nil {
		return err
	}
	cursor, _ := value.(uint64)
	query := h.modelSteps(ctx, "eino_model").Select("id, result_json").Where("stage = ?", "succeeded")
	if found {
		query = query.Where("id > ?", cursor)
	}
	var steps []Step
	if err := query.Order("id").Find(&steps).Error; err != nil {
		return err
	}
	if !found {
		visible := map[string]bool{}
		for _, m := range messages {
			if m != nil {
				for _, c := range m.ToolCalls {
					visible[c.ID] = true
				}
			}
		}
		for _, step := range steps {
			var response aiapp.ChatResponse
			if json.Unmarshal([]byte(step.ResultJSON), &response) != nil {
				continue
			}
			all := len(response.Message.ToolCalls) > 0
			for _, c := range response.Message.ToolCalls {
				all = all && visible[c.ID]
			}
			if all && step.ID > cursor {
				cursor = step.ID
			}
		}
		if err = adk.SetRunLocalValue(ctx, informationSQLCursor, cursor); err != nil {
			return err
		}
	}
	h.modelCursor = cursor
	for _, step := range steps {
		if step.ID > cursor {
			var response aiapp.ChatResponse
			if err = json.Unmarshal([]byte(step.ResultJSON), &response); err != nil {
				return err
			}
			if len(response.Message.ToolCalls) > 0 {
				saved := step
				h.replay = &saved
				return nil
			}
		}
	}
	return nil
}
func (h *informationHost) facts(ctx context.Context) (map[string]any, error) {
	sources, err := h.service.knownSources(ctx, h.input.SourceScope)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	out := map[string]any{"current_time": now.Format(time.RFC3339), "current_date": now.Format("2006-01-02"), "request_id": h.request.ID, "original_request": h.input.Goal, "requirements": validRaw(h.request.RequirementsJSON), "requirements_revision": h.request.RequirementsRevision, "plan": validRaw(h.request.PlanJSON), "facts_revision": h.request.FactsRevision, "source_scope": h.input.SourceScope, "entry_urls": h.input.EntryURLs, "recent_feedback": validRaw(h.request.FeedbackJSON)}
	var plan WorkPlan
	if h.request.PlanJSON != "" {
		_ = json.Unmarshal([]byte(h.request.PlanJSON), &plan)
	}
	var reqs TaskRequirements
	hasReqs := h.request.RequirementsJSON != "" && json.Unmarshal([]byte(h.request.RequirementsJSON), &reqs) == nil
	if h.request.Purpose != "source_discovery" && (plan.NeedsCalculation || (hasReqs && taskRequiresCalculation(reqs))) {
		if quantities := parsedPromptQuantities(h.input.Goal); len(quantities) > 0 {
			out["prompt_quantities"] = quantities
		}
	}
	if h.request.BaselineDeliveryID != "" {
		baselineRow, baseline, err := h.service.SavedDelivery(ctx, h.request.BaselineDeliveryID)
		if err != nil {
			return nil, err
		}
		tables := []map[string]any{}
		for _, table := range baseline.Tables {
			tables = append(tables, map[string]any{"title": table.Title, "columns": table.Columns, "row_count": len(table.Rows)})
		}
		var baseURLs []string
		if baselineRow.RequestID != "" {
			var docURLs []string
			if err := h.service.DB.WithContext(ctx).Table("information_task_documents d").
				Select("DISTINCT COALESCE(NULLIF(h.url, ''), d.source_url) AS u").
				Joins("LEFT JOIN knowledge_heads h ON h.id = d.doc_id").
				Where("d.request_id = ? AND d.status IN ?", baselineRow.RequestID, []string{"succeeded", reusedResponseStatus}).
				Order("u ASC").
				Pluck("u", &docURLs).Error; err == nil {
				for _, u := range docURLs {
					if trimmed := strings.TrimSpace(u); trimmed != "" {
						baseURLs = append(baseURLs, trimmed)
					}
				}
			}
		}
		baselineSummary := map[string]any{
			"delivery_id": h.request.BaselineDeliveryID,
			"outcome":     baseline.Outcome,
			"tables":      tables,
			"instruction": "Baseline delivery for comparison. Use read_materials(delivery_id=...) for prior row values, and verify current sources with fresh acquisition.",
		}
		if packedURLs := packStringsByByteBudget(baseURLs, 1600); len(packedURLs) > 0 {
			baselineSummary["source_urls"] = packedURLs
			if len(packedURLs) < len(baseURLs) {
				baselineSummary["source_urls_total"] = len(baseURLs)
			}
		}
		out["update_baseline"] = baselineSummary
	}
	var observationCount, documentCount, validDocumentCount, failedDocumentCount int64
	if err := h.service.DB.WithContext(ctx).Model(&taskObservation{}).Where("request_id = ?", h.request.ID).Count(&observationCount).Error; err != nil {
		return nil, err
	}
	type taskDocSummaryRow struct {
		ReceiptID   uint64 `json:"receipt_id"`
		ResponseKey string `json:"response_key"`
		DocID       string `json:"doc_id"`
		Revision    string `json:"revision"`
		Status      string `json:"status"`
		HTTPStatus  int    `json:"http_status"`
		BodyValid   bool   `json:"body_valid"`
		RecordCount int    `json:"record_count"`
		URL         string `json:"url"`
	}
	var docRows []taskDocSummaryRow
	if err := h.service.DB.WithContext(ctx).Table("information_task_documents d").
		Select("d.receipt_id, d.response_key, d.doc_id, d.revision, d.status, d.http_status, d.body_valid, d.record_count, COALESCE(h.url, '') AS url").
		Joins("LEFT JOIN knowledge_heads h ON h.id = d.doc_id").
		Where("d.request_id = ?", h.request.ID).
		Order("d.receipt_id ASC, d.response_key ASC").
		Find(&docRows).Error; err != nil {
		return nil, err
	}
	documentCount = int64(len(docRows))
	coverage := make([]map[string]any, 0, min(len(docRows), 16))
	for _, row := range docRows {
		td := taskDocument{
			ReceiptID:   row.ReceiptID,
			ResponseKey: row.ResponseKey,
			DocID:       row.DocID,
			Revision:    row.Revision,
			Status:      row.Status,
			HTTPStatus:  row.HTTPStatus,
			BodyValid:   row.BodyValid,
			RecordCount: row.RecordCount,
		}
		valid := isValidDocumentForCompleted(td)
		if valid {
			validDocumentCount++
		} else {
			failedDocumentCount++
		}
		if len(coverage) < 16 {
			extStatus := "failed"
			if row.RecordCount > 0 {
				extStatus = "extracted"
			} else if valid {
				extStatus = "pending_extraction"
			}
			entry := map[string]any{
				"doc_id":            row.DocID,
				"revision":          row.Revision,
				"status":            row.Status,
				"http_status":       row.HTTPStatus,
				"valid":             valid,
				"record_count":      row.RecordCount,
				"extraction_status": extStatus,
			}
			if row.URL != "" {
				entry["url"] = row.URL
			}
			coverage = append(coverage, entry)
		}
	}
	out["source_coverage"] = coverage
	if len(docRows) > len(coverage) {
		out["source_coverage_total"] = len(docRows)
		out["source_coverage_omitted"] = len(docRows) - len(coverage)
		out["source_coverage_hint"] = "Call read_materials(kind='documents', offset=..., limit=...) to inspect omitted captured documents."
	}
	if hasReqs {
		counts, err := h.automaticOutputCounts(ctx, reqs)
		if err != nil {
			return nil, err
		}
		reqProgress := make([]map[string]any, 0, len(reqs.ExplicitOutput))
		for _, r := range reqs.ExplicitOutput {
			progress := map[string]any{
				"id":        r.ID,
				"kind":      r.Kind,
				"target":    r.Target,
				"fields":    r.Fields,
				"min_count": r.MinCount,
				"count_by":  r.CountBy,
			}
			if count, ok := counts[r.ID]; ok && r.MinCount != nil {
				progress["available_in_saved_rows"] = count
				progress["shortfall"] = max(0, *r.MinCount-count)
			}
			reqProgress = append(reqProgress, progress)
		}
		out["requirements_progress"] = map[string]any{
			"revision":         reqs.Revision,
			"explicit_outputs": reqProgress,
			"observations":     observationCount,
			"valid_documents":  validDocumentCount,
			"failed_documents": failedDocumentCount,
		}
	}
	var obsRows []observationRow
	if err := h.service.DB.WithContext(ctx).
		Table("information_observations o").
		Select("o.*").
		Joins("JOIN information_task_observations t ON t.observation_id = o.id").
		Where("t.request_id = ?", h.request.ID).
		Order("o.receipt_id ASC, o.record_position ASC").
		Limit(100).
		Find(&obsRows).Error; err != nil {
		return nil, err
	}
	obsItems := make([]Observation, 0, len(obsRows))
	for _, row := range obsRows {
		if decoded, e := decodeObservation(row); e == nil {
			obsItems = append(obsItems, decoded)
		}
	}
	savedFacts := map[string]any{
		"observations":        observationCount,
		"documents":           documentCount,
		"succeeded_documents": validDocumentCount,
		"valid_documents":     validDocumentCount,
		"failed_documents":    failedDocumentCount,
	}
	if previews := compactObservationPreviews(obsItems, 2400); len(previews) > 0 {
		savedFacts["observation_previews"] = previews
		shownIDs := make(map[string]bool, len(previews))
		for _, p := range previews {
			if id, ok := p["observation_id"].(string); ok && id != "" {
				shownIDs[id] = true
			}
		}
		const omittedIndexBudgetBytes = 2400
		omittedIndex := make([]map[string]any, 0)
		omittedIndexBytes := 2
		omittedIndexTruncated := false
		for _, o := range obsItems {
			if shownIDs[o.ID] {
				continue
			}
			keys := make([]string, 0, len(o.Fields))
			for k := range o.Fields {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			idxEntry := map[string]any{"observation_id": o.ID, "keys": keys}
			if src, ok := o.Fields["source_url"].(string); ok && src != "" {
				idxEntry["source_url"] = src
			}
			encoded, err := json.Marshal(idxEntry)
			if err != nil {
				continue
			}
			if len(omittedIndex) > 0 && omittedIndexBytes+len(encoded)+1 > omittedIndexBudgetBytes {
				omittedIndexTruncated = true
				break
			}
			omittedIndexBytes += len(encoded) + 1
			omittedIndex = append(omittedIndex, idxEntry)
		}
		savedFacts["observation_previews_shown"] = len(previews)
		if int64(len(previews)) < observationCount {
			savedFacts["observation_previews_omitted"] = observationCount - int64(len(previews))
			if len(omittedIndex) > 0 {
				savedFacts["omitted_observation_index"] = omittedIndex
			}
			if omittedIndexTruncated || int64(len(obsItems)) < observationCount {
				savedFacts["omitted_observation_index_truncated"] = true
			}
			savedFacts["observation_query_hint"] = "Call read_materials(kind='observations', observation_ids=[...]) or read_materials(kind='observations', document_ids=[...], limit=50) to inspect omitted observations cleanly."
		}
	}
	out["saved_facts"] = savedFacts

	var recent []Step
	if err := h.service.DB.WithContext(ctx).
		Where("request_id = ? AND eino_epoch = ? AND kind NOT IN ?", h.request.ID, h.request.EinoEpoch, []string{"agent_event", "route_semantic", "eino_model", "eino_summary", "eino_resume"}).
		Order("id DESC").
		Limit(8).
		Find(&recent).Error; err == nil && len(recent) > 0 {
		summaries := make([]map[string]any, 0, len(recent))
		for i := len(recent) - 1; i >= 0; i-- {
			st := recent[i]
			entry := map[string]any{"id": st.ID, "tool": st.Kind, "stage": st.Stage}
			if st.Error != "" {
				entry["error"] = truncateRunes(st.Error, 180)
			}
			summaries = append(summaries, entry)
		}
		out["recent_steps"] = summaries
	}
	out["saved_source_directory"] = map[string]any{
		"total":            len(sources),
		"search_available": h.service.Search != nil,
		"hint":             "Use discover_sources(saved_only=true, query=...) to browse stored source entries.",
	}
	if h.service != nil && h.service.DB != nil {
		if catalog, cErr := h.service.acquisitionCatalog(ctx); cErr == nil && len(catalog) > 0 {
			matchingAssets := make([]map[string]any, 0, min(len(catalog), 12))
			for _, entry := range catalog {
				if len(h.input.SourceScope) > 0 && !urlInScope(entry.URL, h.input.SourceScope) {
					continue
				}
				matchingAssets = append(matchingAssets, map[string]any{
					"asset_id":    entry.ID,
					"kind":        entry.Kind,
					"url":         entry.URL,
					"description": truncateRunes(entry.Description, 120),
				})
				if len(matchingAssets) >= 12 {
					break
				}
			}
			if len(matchingAssets) > 0 {
				out["saved_assets"] = matchingAssets
			}
		}
	}
	return out, nil
}

var errInformationProposal = errors.New("invalid information tool proposal")

func (h *informationHost) retryProposal(ctx context.Context, rc *adk.RetryContext) *adk.RetryDecision {
	if !errors.Is(rc.Err, errInformationProposal) {
		return &adk.RetryDecision{}
	}
	diagnostic := informationProposalDiagnosticPrefix + rc.Err.Error()
	messages := make([]*schema.Message, 0, len(rc.InputMessages)+1)
	for _, message := range rc.InputMessages {
		if (message.Role == schema.System || message.Role == schema.User) && strings.HasPrefix(message.Content, informationProposalDiagnosticPrefix) {
			if message.Content == diagnostic {
				return &adk.RetryDecision{}
			}
			continue
		}
		messages = append(messages, message)
	}
	messages = append(messages, schema.UserMessage(diagnostic))
	return &adk.RetryDecision{Retry: true, ModifiedInputMessages: messages, PersistModifiedInputMessages: true, Backoff: time.Nanosecond, RejectReason: "invalid domain tool proposal"}
}
