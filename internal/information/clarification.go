package information

import (
	"context"
	"encoding/json"
	"go-lua-crawler/internal/agentdata"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"strings"
)

// Clarify records a user addition without resetting facts, tools, or Eino's
// checkpoint. A running invocation sees it at its next host refresh.
func (s *Service) Clarify(ctx context.Context, id, guidance string) error {
	return s.AppendUserInput(ctx, id, guidance)
}

func (s *Service) AppendUserInput(ctx context.Context, id, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return businessError("USER_INPUT_REQUIRED", "task", "补充内容不能为空", "填写需要补充或更正的要求")
	}
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var r Request
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&r, "id = ?", id).Error; err != nil {
			return err
		}
		if terminal(r.Status) {
			return businessError("TASK_TERMINAL", "task", "已结束任务的要求不可改写", "创建后续任务并关联原交付")
		}
		var requirements TaskRequirements
		if err := json.Unmarshal([]byte(r.RequirementsJSON), &requirements); err != nil {
			return err
		}
		requirements.Additions = append(requirements.Additions, text)
		requirements.Revision++
		changes := map[string]any{"requirements_json": string(mustMarshal(requirements)), "requirements_revision": requirements.Revision}
		if r.Status == "needs_input" {
			changes["status"] = "queued"
			changes["stage"] = "queued"
			changes["wait_json"] = ""
			changes["wake_at"] = nil
		}
		if err := tx.Model(&Request{}).Where("id = ?", id).Updates(changes).Error; err != nil {
			return err
		}
		return tx.Create(&Step{RequestID: id, Key: "user_input:" + agentdata.Token(), Kind: "user_input", Stage: "succeeded", InputJSON: string(mustMarshal(map[string]string{"text": text})), ResultJSON: string(mustMarshal(requirements))}).Error
	})
}
