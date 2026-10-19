package aiapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/knowledge"
	"go-lua-crawler/internal/task"
	"gorm.io/gorm"
)

// This is a host-captured acquisition recipe, not another model conversation.
// Bindings originate only in successful, registered, typed child work.
type MonitorPlan struct {
	Version       int              `json:"version"`
	ConfigVersion uint64           `json:"config_version"`
	Spec          TaskSpec         `json:"spec"`
	Bindings      []MonitorBinding `json:"bindings"`
}

type MonitorBinding struct {
	SourceID     string         `json:"source_id"`
	ArtifactHash string         `json:"artifact_hash"`
	URL          string         `json:"url"`
	Params       map[string]any `json:"params"`
}

func (s *Service) captureMonitorPlan(ctx context.Context, m Monitor, r agentdata.Run) (string, error) {
	spec, err := specOf(r)
	if err != nil {
		return "", err
	}
	// Factual reuse may acquire evidence for semantic tasks, but may not replace
	// their reasoning/summary requirements with deterministic field completion.
	if spec.RequireSummary || summaryRequired(r) || len(spec.Requirements) > 0 {
		return "", nil
	}
	rows, err := s.ChildResults(ctx, r.ID)
	if err != nil {
		return "", err
	}
	rows, err = s.effectiveChildren(ctx, r.ID, rows)
	if err != nil {
		return "", err
	}
	plan := MonitorPlan{Version: 1, ConfigVersion: m.Version, Spec: spec, Bindings: []MonitorBinding{}}
	selected := []ChildResult{}
	seen := map[string]bool{}
	for _, row := range rows {
		if row.Task.Status != task.StatusSucceeded || row.Link.SourceID == "" || row.Task.ProgramHash == "" {
			continue
		}
		head, e := s.Data.SourceHead(ctx, row.Link.SourceID)
		if e != nil {
			return "", e
		}
		if head.Status != "active" || head.ArtifactHash != row.Link.ArtifactHash {
			continue
		}
		artifact, e := s.Data.Artifact(ctx, head.ArtifactHash)
		if e != nil {
			return "", e
		}
		if artifact.Kind != "adapter" || artifact.PolicyVersion != "public-read-v1" {
			continue
		}
		if e = checkCapabilityForRun(r, artifact); e != nil {
			continue
		}
		var params childParameters
		if e = json.Unmarshal([]byte(row.Task.ProgramParams), &params); e != nil {
			return "", e
		}
		if params.SourceID != row.Link.SourceID || agentdata.Hash(params.Spec) != agentdata.Hash(spec) {
			continue
		}
		if e = ValidateParameters(artifact.InputSchema, params.Params); e != nil {
			continue
		}
		binding := MonitorBinding{SourceID: row.Link.SourceID, ArtifactHash: artifact.Hash, URL: row.Task.URL, Params: params.Params}
		key := agentdata.Hash(binding)
		if seen[key] {
			continue
		}
		seen[key] = true
		plan.Bindings = append(plan.Bindings, binding)
		selected = append(selected, row)
	}
	if len(plan.Bindings) == 0 {
		return "", nil
	}
	// Cache only if the reusable subset itself delivers the frozen contract.
	result, err := evaluateRows(spec, selected)
	if err != nil {
		return "", err
	}
	if !taskDeliverySatisfied(&result) {
		return "", nil
	}
	raw, err := json.Marshal(plan)
	return string(raw), err
}

func (s *Service) validateMonitorPlan(ctx context.Context, r agentdata.Run, check MonitorCheck, plan MonitorPlan) error {
	if plan.Version != 1 || plan.ConfigVersion != check.ConfigVersion || len(plan.Bindings) == 0 {
		return fmt.Errorf("invalid saved acquisition plan version")
	}
	spec, err := specOf(r)
	if err != nil {
		return err
	}
	if agentdata.Hash(spec) != agentdata.Hash(plan.Spec) || spec.RequireSummary || summaryRequired(r) || len(spec.Requirements) > 0 {
		return fmt.Errorf("saved acquisition contract differs or needs semantic review")
	}
	for _, b := range plan.Bindings {
		head, err := s.Data.SourceHead(ctx, b.SourceID)
		if err != nil {
			return err
		}
		if head.Status != "active" || head.ArtifactHash != b.ArtifactHash {
			return fmt.Errorf("saved source %s is %s or changed version", b.SourceID, head.Status)
		}
		allowed, err := s.canUseCapability(ctx, r, b.SourceID, b.ArtifactHash)
		if err != nil {
			return err
		}
		if !allowed {
			return fmt.Errorf("capability reuse disabled for this run")
		}
		a, err := s.Data.Artifact(ctx, b.ArtifactHash)
		if err != nil {
			return err
		}
		if a.Kind != "adapter" || a.PolicyVersion != "public-read-v1" {
			return fmt.Errorf("saved program lacks current adapter policy")
		}
		if err = checkCapabilityForRun(r, a); err != nil {
			return err
		}
		if err = ValidateParameters(a.InputSchema, b.Params); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) monitorReplan(ctx context.Context, r agentdata.Run, check MonitorCheck, plan MonitorPlan, round *LoopRound, cause error) error {
	round.Action = "monitor_replan"
	round.Reason = cause.Error()
	feedback, _ := json.Marshal(map[string]any{"error": cause.Error(), "saved_bindings": plan.Bindings, "preserve_successful_children": true})
	call := ToolCall{ID: fmt.Sprintf("monitor-feedback-%d", check.ID), Type: "function", Function: FunctionCall{Name: "monitor_plan_feedback", Arguments: `{}`}}
	receipt := ToolResult{CallID: call.ID, Name: call.Function.Name, Error: cause.Error(), Data: feedback}
	reply, _ := json.Marshal(receipt)
	// Execution observations must not masquerade as another user goal: user
	// history is intentionally removed during compaction. Pair a durable host
	// feedback receipt so the next Agent turn retains the actual recovery cause.
	messages := []Message{{Role: "assistant", ToolCalls: []ToolCall{call}}, {Role: "tool", ToolCallID: call.ID, Content: string(reply)}}
	raw, _ := json.Marshal(messages)
	return s.Data.WithOwnedRun(ctx, r.ID, r.Token, func(store *agentdata.Store) error {
		if err := store.DB.Model(&MonitorCheck{}).Where("id = ? AND mode = ?", check.ID, "plan").Updates(map[string]any{"mode": "agent", "fallback_reason": cause.Error()}).Error; err != nil {
			return err
		}
		scoped := *s
		scoped.Data = store
		scoped.Knowledge = knowledge.NewStore(store.DB)
		op, _, err := store.ReserveOperation(ctx, r.ID, r.Token, "tool:"+call.ID, agentdata.Hash(feedback), call.Function.Name, nil)
		if err != nil {
			return err
		}
		if err := store.CompleteOperation(ctx, r.ID, r.Token, op.ID, "failed", string(reply), ""); err != nil {
			return err
		}
		return scoped.saveLoopRun(ctx, r, map[string]any{"status": "reviewing", "plan_json": string(raw), "last_error": cause.Error()}, round, nil)
	})
}

// A saved plan changes action selection only. Submission still uses the same
// durable tool, budget receipt, source checks, Outbox and Worker as Agent actions.
func (s *Service) advanceMonitorPlan(ctx context.Context, r agentdata.Run, state LoopState, round *LoopRound) (bool, error) {
	var check MonitorCheck
	err := s.Data.DB.WithContext(ctx).Where("run_id = ? AND mode = ?", r.ID, "plan").First(&check).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	var plan MonitorPlan
	if err = json.Unmarshal([]byte(check.PlanJSON), &plan); err == nil {
		err = s.validateMonitorPlan(ctx, r, check, plan)
	}
	if err != nil {
		return true, s.monitorReplan(ctx, r, check, plan, round, err)
	}
	submitted := map[string]bool{}
	for _, op := range state.Operations {
		if op.Kind == "submit_acquisition" && op.Stage == "succeeded" {
			submitted[op.Key] = true
		}
	}
	queued := false
	for i, b := range plan.Bindings {
		callID := fmt.Sprintf("monitor-plan-%d", i)
		if submitted["tool:"+callID] {
			continue
		}
		args, _ := json.Marshal(acquireArgs{SourceID: b.SourceID, URL: b.URL, Params: b.Params})
		call := ToolCall{ID: callID, Type: "function", Function: FunctionCall{Name: "submit_acquisition", Arguments: string(args)}}
		_, err = s.executeDurableTool(ctx, r, call)
		observation := LoopToolObservation{CallID: callID, Name: call.Function.Name}
		if err != nil {
			observation.Error = err.Error()
		}
		round.Tools = append(round.Tools, observation)
		if err != nil {
			return true, s.monitorReplan(ctx, r, check, plan, round, err)
		}
		queued = true
	}
	if queued {
		round.Action = "monitor_submit"
		return true, s.saveLoopRun(ctx, r, map[string]any{"status": "waiting"}, round, nil)
	}
	for _, row := range state.TaskResults {
		if !row.Task.Status.IsTerminal() {
			round.Action = "wait"
			return true, s.saveLoopRun(ctx, r, map[string]any{"status": "waiting"}, round, nil)
		}
	}
	result, err := s.Evaluate(ctx, r)
	if err != nil {
		return true, s.monitorReplan(ctx, r, check, plan, round, err)
	}
	if !taskDeliverySatisfied(&result) {
		return true, s.monitorReplan(ctx, r, check, plan, round, fmt.Errorf("saved acquisition produced stale, conflicting or missing required facts"))
	}
	round.Action = "monitor_finish"
	round.RequestedStatus = "completed"
	raw, _ := json.Marshal(result)
	return true, s.saveLoopRun(ctx, r, map[string]any{"status": "completed", "result_json": string(raw), "last_error": ""}, round, nil)
}
