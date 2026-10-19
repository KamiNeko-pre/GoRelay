package aiapp

import (
	"context"
	"encoding/json"
	"fmt"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/knowledge"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"sort"
	"strings"
	"time"
)

// Acquired pages are investigation material, not accepted records or answers.
// All stop paths use this projection without making another model request.
func attachStoppedDelivery(result *TaskResult, reason string, ops []agentdata.Operation) {
	// Re-render host-only summaries from canonical fields, never append a second
	// copy to the previous rendering. A verified cited answer remains intact.
	if result.Answer == nil {
		result.Summary = ""
	}
	attachHostStopDelivery(result, reason)
	seen := map[string]bool{}
	var sources []string
	result.AcquiredMaterials = nil
	for _, op := range ops {
		if op.Kind != "inspect_source" || op.Stage != "succeeded" {
			continue
		}
		var reply ToolResult
		var page sourceInspection
		if json.Unmarshal([]byte(op.ResultJSON), &reply) != nil || reply.Error != "" || json.Unmarshal(reply.Data, &page) != nil {
			continue
		}
		r := page.Source.Receipt
		if r.Status < 200 || r.Status >= 300 || r.URL == "" || seen[r.URL] {
			continue
		}
		seen[r.URL] = true
		result.AcquiredMaterials = append(result.AcquiredMaterials, map[string]any{"url": r.URL, "handle": reply.CallID, "status": "acquired_not_analysed", "read_with": map[string]any{"handle": reply.CallID, "view": "dom"}})
		sources = append(sources, r.URL)
	}
	if len(sources) > 0 && result.Answer == nil {
		result.Summary += "\n已获取的调查材料（尚未在此清单中证明业务判断或推荐成立）：\n"
		for _, url := range sources {
			result.Summary += "- " + url + "\n"
		}
	}
}

// Cancellation revokes the execution token before taking a read-only delivery
// snapshot. No inference or new acquisition is permitted during this step.
func (s *Service) CancelWithDelivery(ctx context.Context, id string) error {
	return s.Data.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var r agentdata.Run
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", id).First(&r).Error; err != nil {
			return err
		}
		if r.IsTerminal() {
			return agentdata.ErrConflict
		}
		scoped := *s
		scoped.Data = agentdata.NewStore(tx)
		scoped.Knowledge = knowledge.NewStore(tx)
		result, evalErr := scoped.Evaluate(ctx, r)
		if evalErr != nil {
			result = TaskResult{Gaps: []string{"结果核对未完成：" + evalErr.Error()}}
		}
		ops, err := scoped.Data.Operations(ctx, id)
		if err != nil {
			return err
		}
		attachStoppedDelivery(&result, "任务已取消，未继续调用模型；以下仅为停止时保存的成果。", ops)
		raw, err := json.Marshal(result)
		if err != nil {
			return err
		}
		return tx.Model(&agentdata.Run{}).Where("id = ?", id).Updates(map[string]any{"status": "cancelled", "cancelled": true, "token": "", "lease_until": nil, "result_json": string(raw)}).Error
	})
}

// This is a presentation of saved source fields, never a model conclusion or
// a replacement for the cited-answer completion contract.
func attachHostStopDelivery(result *TaskResult, reason string) {
	if reason != "" {
		present := false
		for _, gap := range result.Gaps {
			if gap == reason {
				present = true
				break
			}
		}
		if !present {
			result.Gaps = append(result.Gaps, reason)
		}
	}
	if result.Summary != "" {
		return
	}
	var text strings.Builder
	fmt.Fprintf(&text, "任务未完整完成。已保留 %d 条来源记录；以下为保存字段，不是完整分析结论。\n", len(result.Records))
	for i, row := range result.Records {
		fmt.Fprintf(&text, "\n记录 %d：\n", i+1)
		fields := make([]string, 0, len(row.Fields))
		for field := range row.Fields {
			fields = append(fields, field)
		}
		sort.Strings(fields)
		for _, field := range fields {
			value, _ := json.Marshal(row.Fields[field])
			preview := []rune(string(value))
			if len(preview) > 500 {
				fmt.Fprintf(&text, "- %s：%s…（完整字段已保存）\n", field, string(preview[:500]))
			} else {
				fmt.Fprintf(&text, "- %s：%s\n", field, value)
			}
		}
		for _, source := range row.Sources {
			fmt.Fprintf(&text, "- 来源：%s\n", source.URL)
		}
	}
	if len(result.Records) == 0 {
		text.WriteString("没有可交付的来源记录。\n")
	}
	if len(result.Gaps) > 0 {
		text.WriteString("\n缺口或停止原因：\n")
		for _, gap := range result.Gaps {
			fmt.Fprintf(&text, "- %s\n", gap)
		}
	}
	result.Summary = text.String()
}

// The row lock serializes expiry against task/model receipt commits. Expiry
// does not require a valid execution lease and never starts another model call.
func (s *Service) expireWithDelivery(ctx context.Context, id string) error {
	return s.Data.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var r agentdata.Run
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", id).First(&r).Error; err != nil {
			return err
		}
		if r.IsTerminal() || !r.Expired(time.Now()) {
			return nil
		}
		scoped := *s
		scoped.Data = agentdata.NewStore(tx)
		scoped.Knowledge = knowledge.NewStore(tx)
		result, e := scoped.Evaluate(ctx, r)
		if e != nil {
			_ = json.Unmarshal([]byte(r.ResultJSON), &result)
			result.Gaps = append(result.Gaps, "结果核对未完成："+e.Error())
		}
		ops, e := scoped.Data.Operations(ctx, id)
		if e != nil {
			return e
		}
		attachStoppedDelivery(&result, "任务总时限已到，停止执行并保留已有成果。", ops)
		raw, e := json.Marshal(result)
		if e != nil {
			return e
		}
		return tx.Model(&agentdata.Run{}).Where("id = ?", id).Updates(map[string]any{"status": "expired", "token": "", "lease_until": nil, "result_json": string(raw)}).Error
	})
}
func (s *Service) sweepExpiredDeliveries(ctx context.Context) error {
	var runs []agentdata.Run
	if err := s.Data.DB.WithContext(ctx).Where("deadline <= ? AND status NOT IN ?", time.Now(), []string{"completed", "partial", "failed", "cancelled", "expired"}).Order("deadline").Limit(8).Find(&runs).Error; err != nil {
		return err
	}
	for _, r := range runs {
		if err := s.expireWithDelivery(ctx, r.ID); err != nil {
			return err
		}
	}
	return nil
}
