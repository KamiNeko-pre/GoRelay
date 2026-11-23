package information

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/aiapp"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Count consecutive inference failures, independent of changing host status text.
func informationModelAttempts(steps []Step, kind string, resumedAfter uint64) int {
	attempts := 0
	for i := len(steps) - 1; i >= 0; i-- {
		step := steps[i]
		if step.ID <= resumedAfter || step.Kind != kind {
			continue
		}
		if step.Stage == "succeeded" {
			break
		}
		if step.Stage != "failed" && step.Stage != "reserved" && step.Stage != "unknown_external" {
			continue
		}
		var response aiapp.ChatResponse
		if json.Unmarshal([]byte(step.ResultJSON), &response) == nil && response.Failure != nil && response.Failure.Kind == "invalid_proposal" {
			continue
		}
		attempts++
	}
	return attempts
}

type informationProvider struct {
	host    *informationHost
	purpose string
}

func (p *informationProvider) Chat(ctx context.Context, request aiapp.ChatRequest) (aiapp.ChatResponse, error) {
	h := p.host
	if err := h.refresh(ctx); err != nil {
		return aiapp.ChatResponse{}, err
	}
	if p.purpose == "agent" && h.replay != nil {
		step := *h.replay
		h.replay = nil
		h.currentModelStep = step.ID
		var response aiapp.ChatResponse
		err := json.Unmarshal([]byte(step.ResultJSON), &response)
		return response, err
	}
	kind := "eino_model"
	if p.purpose == "summary" {
		kind = "eino_summary"
	}
	var resumeID uint64
	if err := h.service.DB.WithContext(ctx).Model(&Step{}).Select("COALESCE(MAX(id), 0)").Where("request_id = ? AND eino_epoch = ? AND kind = ?", h.owner.RequestID, h.request.EinoEpoch, "eino_resume").Scan(&resumeID).Error; err != nil {
		return aiapp.ChatResponse{}, err
	}
	var steps []Step
	if err := h.modelSteps(ctx, kind).Select("id, kind, stage, result_json").Where("id > ?", resumeID).Order("id").Find(&steps).Error; err != nil {
		return aiapp.ChatResponse{}, err
	}
	attempt := informationModelAttempts(steps, kind, resumeID)
	hash := agentdata.Hash(map[string]any{"resume_generation": resumeID, "epoch": h.request.EinoEpoch, "cursor": h.modelCursor, "request": request})
	key := kind + ":" + hash
	if attempt > 0 {
		key += fmt.Sprintf(":attempt:%d", attempt)
	}
	step := Step{EinoEpoch: h.request.EinoEpoch, RequestID: h.owner.RequestID, Key: key, Kind: kind, InputHash: hash, InputJSON: string(mustMarshal(request)), Stage: "reserved"}
	fresh := false
	err := h.owned(ctx, func(tx *gorm.DB) error {
		q := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&step)
		if q.Error != nil {
			return q.Error
		}
		fresh = q.RowsAffected == 1
		if !fresh {
			return tx.Where(map[string]any{"request_id": step.RequestID, "key": step.Key}).First(&step).Error
		}
		return nil
	})
	if err != nil {
		return aiapp.ChatResponse{}, err
	}
	if p.purpose == "agent" {
		h.currentModelStep = step.ID
	}
	var response aiapp.ChatResponse
	if !fresh {
		if step.Stage == "reserved" || step.Stage == "unknown_external" {
			h.pause = &informationWait{Stage: "blocked_runtime", Reason: "Saved external model outcome is unknown. Reconcile its original receipt before resuming; no model call was repeated."}
			return response, ErrUnknownStep
		}
		if step.Stage == "failed" {
			return response, fmt.Errorf("%w: saved rejected proposal: %s", errInformationProposal, step.Error)
		}
		err = json.Unmarshal([]byte(step.ResultJSON), &response)
		return response, err
	}
	if err = h.event(ctx, "model_started", "模型正在规划下一步", "", "running"); err != nil {
		return response, err
	}
	started := time.Now()
	chatModel := h.service.Model
	if p.purpose == "summary" && h.service.AuxModel != nil {
		chatModel = h.service.AuxModel
	} else if p.purpose == "agent" && h.request.Purpose == "source_discovery" && h.service.AuxModel != nil {
		chatModel = h.service.AuxModel
	}
	response, err = chatModel.Chat(ctx, request)
	if response.DurationMS == 0 {
		response.DurationMS = time.Since(started).Milliseconds()
	}
	if p.purpose == "agent" && (errors.Is(err, aiapp.ErrInvalidProposal) || err == nil && response.FinishReason == "length") {
		if response.FinishReason == "length" {
			err = fmt.Errorf("%w: response was truncated; submit smaller tool calls using saved evidence IDs", errInformationProposal)
		} else {
			err = fmt.Errorf("%w: %v", errInformationProposal, err)
		}
	}
	if err == nil && p.purpose == "agent" {
		if response.Message.Role == "" {
			response.Message.Role = "assistant"
		}
		if err == nil && (response.Message.Role != "assistant" || len(response.Message.ToolCalls) == 0) {
			err = fmt.Errorf("%w: use submit_delivery with the report, outcome and selected saved evidence if ready; otherwise use an available tool for the next step", errInformationProposal)
		}
		seen := map[string]bool{}
		var steps []Step
		e := h.modelSteps(ctx, "eino_model").Select("result_json").Where("stage = ? AND id <> ?", "succeeded", step.ID).Find(&steps).Error
		if e != nil {
			return response, e
		}
		for _, old := range steps {
			var saved aiapp.ChatResponse
			if json.Unmarshal([]byte(old.ResultJSON), &saved) == nil {
				for _, c := range saved.Message.ToolCalls {
					seen[c.ID] = true
				}
			}
		}
		response.Message = aiapp.NormalizeModelToolCallIDs(response.Message, step.ID, seen)
	}
	stage := "succeeded"
	message := ""
	if err != nil {
		stage = "failed" // An unusable inference reply dispatched no domain tool.
		feedbackCause := err
		if errors.Is(err, errInformationProposal) {
			feedbackCause = fmt.Errorf("%w: %v", aiapp.ErrInvalidProposal, err)
		}
		response.Failure = aiapp.ModelFailureFeedback(feedbackCause, attempt)
		message = err.Error()
	}
	save := h.owned(context.WithoutCancel(ctx), func(tx *gorm.DB) error {
		return tx.Model(&Step{}).Where("id = ? AND stage = ?", step.ID, "reserved").Updates(map[string]any{"stage": stage, "result_json": string(mustMarshal(response)), "error": message}).Error
	})
	if save != nil {
		return response, save
	}
	if err != nil {
		if errors.Is(err, errInformationProposal) {
			h.request.FeedbackJSON = string(mustMarshal(map[string]any{"code": "MODEL_PROPOSAL_INVALID", "error": err.Error(), "recovery": []string{"Use an offered tool and its real parameters. Preserve saved sources and results; correct the named protocol defect rather than restarting the task."}}))
			if saveErr := h.saveState(context.WithoutCancel(ctx)); saveErr != nil {
				return response, saveErr
			}
			return response, err
		}
		wait := informationWait{Stage: "blocked_provider", Reason: err.Error()}
		if response.Failure != nil && response.Failure.Retryable && response.Failure.RetryAt != nil && ctx.Err() == nil {
			wait.Stage, wait.WakeAt = "waiting", response.Failure.RetryAt
		}
		return response, h.interrupt(ctx, wait)
	}
	if err = h.event(ctx, "assistant", "模型已提交下一步动作", "", "succeeded"); err != nil {
		return response, err
	}
	return response, nil
}

// Resume changes only scheduling state. The original goal, contract, facts,
// frontier, batches, child IDs and opaque checkpoint remain intact.
func (s *Service) Resume(ctx context.Context, id string) error {
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current Request
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&current, "id = ?", id).Error; err != nil {
			return err
		}
		if current.CurrentDeliveryID != "" || current.Status == "cancelled" || current.Status == "expired" || current.Status == "completed" {
			return agentdata.ErrConflict
		}
		if current.LeaseUntil != nil && current.LeaseUntil.After(time.Now()) {
			return agentdata.ErrConflict
		}
		if current.Status == "needs_input" {
			return businessError("USER_INPUT_REQUIRED", "task", "任务需要具体补充信息", "通过补充要求入口回答保存的问题，再接续任务")
		}
		if current.deadlineReached(time.Now()) {
			return businessError("TASK_DEADLINE_EXPIRED", "task", "用户指定的原截止时间已到", "创建具有新截止时间的后续任务")
		}
		// Explicit user retry preserves checkpoints, prior receipts, facts and waits.
		// Actual dependency readiness is still checked before the runner is claimed.
		if err := tx.Create(&Step{EinoEpoch: current.EinoEpoch, RequestID: id, Key: "eino_resume:" + agentdata.Token(), Kind: "eino_resume", Stage: "succeeded", ResultJSON: string(mustMarshal(map[string]any{"previous_status": current.Status, "evidence_preserved": true}))}).Error; err != nil {
			return err
		}
		changes := map[string]any{"status": "queued", "last_error": "", "status_json": "", "lease_token": "", "lease_until": nil}
		if current.Stage == "blocked_provider" || current.Stage == "blocked_runtime" {
			changes["stage"], changes["wait_json"], changes["wake_at"] = "queued", "", nil
		}
		return tx.Model(&Request{}).Where("id = ?", id).Updates(changes).Error
	})
}
