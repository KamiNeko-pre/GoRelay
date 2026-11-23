package information

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

type RequirementResponse struct {
	RequirementID  string   `json:"requirement_id"`
	Status         string   `json:"status" jsonschema:"enum=satisfied,enum=unmet,enum=unknown"`
	Explanation    string   `json:"explanation"`
	ObservationIDs []string `json:"observation_ids,omitempty"`
}

type DeliveryDraft struct {
	RequirementsRevision  uint64                     `json:"requirements_revision,omitempty"`
	Report                string                     `json:"report,omitempty" jsonschema:"description=完整交付报告：除说明采集来源与条数外，必须直接回答用户需求中全部非表格性的比较说明、计算规则/公式、选型建议与实际覆盖范围/缺口。"`
	Tables                []DeliveryTable            `json:"tables,omitempty" jsonschema:"description=可选的结构化汇总表（数据主体已自动落库，不必逐格抄录全表）。"`
	Actions               []string                   `json:"actions,omitempty"`
	References            []StoredReference          `json:"references,omitempty" jsonschema:"description=支持交付结论的已保存快照引用；传入 acquisition_receipt_ids 时会自动绑定对应引用。"`
	RequirementResponses  []RequirementResponse      `json:"requirement_responses,omitempty"`
	CompletionRationale   string                     `json:"completion_rationale,omitempty"`
	Gaps                  []string                   `json:"gaps,omitempty"`
	Outcome               string                     `json:"outcome,omitempty" jsonschema:"enum=completed,enum=partial,enum=no_match,description=交付状态：满足需求用 completed，部分完成用 partial（附 gaps），确认无匹配用 no_match。"`
	DiscoveredSources     []string                   `json:"discovered_sources,omitempty" jsonschema:"description=仅来源发现任务使用：本任务实际打开核验并选定的来源入口 URL。"`
	AcquisitionReceiptIDs []uint64                   `json:"acquisition_receipt_ids,omitempty" jsonschema:"description=选择要交付的采集回执 ID，直接使用该批次已保存的记录；无需逐行复制。"`
	CalculationInputs     *DeliveryCalculationInputs `json:"calculation_inputs,omitempty" jsonschema:"description=供本次交付的所有公式共用的已核验输入，每个变量只绑定一次。"`
	Calculations          []DeliveryCalculation      `json:"calculations,omitempty" jsonschema:"description=有派生结果时传入可复算公式及绑定原文的输入。report 和 tables 中用 {{id}} 引用宿主核验、复算后的数值。"`
}

func normalizeDeliveryOutcome(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "completed", "complete", "done", "success", "succeeded", "fulfilled", "finished", "ok":
		return "completed"
	case "partial", "incomplete", "partial_delivery":
		return "partial"
	case "no_match", "nomatch", "none", "not_found", "empty":
		return "no_match"
	}
	return strings.TrimSpace(raw)
}

type DeliveryContext struct {
	BaselineDeliveryID string
	NewCaptures        int64
}

func checkDelivery(requirements TaskRequirements, draft DeliveryDraft, evidence DeliveryContext) error {
	fail := func(message string) error {
		return businessError("DELIVERY_INCOMPLETE", "delivery", message, "补充简短执行报告或标注 partial/gaps 后重新提交")
	}
	draft.Outcome = normalizeDeliveryOutcome(draft.Outcome)
	if draft.RequirementsRevision == 0 {
		draft.RequirementsRevision = requirements.Revision
	}
	if draft.RequirementsRevision != requirements.Revision {
		return businessError("REQUIREMENTS_CHANGED", "delivery", "用户要求版本已变化", "读取当前要求后按新版本提交")
	}
	switch draft.Outcome {
	case "completed", "partial", "no_match":
	default:
		return fail("outcome 必须为 completed、partial 或 no_match")
	}
	if strings.TrimSpace(draft.Report) == "" && len(draft.Tables) == 0 && len(draft.Actions) == 0 {
		return fail("缺少简短执行报告或交付内容")
	}
	if draft.Outcome == "partial" && len(draft.Gaps) == 0 {
		return fail("部分交付必须说明具体缺口")
	}
	if evidence.BaselineDeliveryID != "" && draft.Outcome != "partial" && evidence.NewCaptures == 0 {
		return businessError("UPDATE_ACQUISITION_REQUIRED", "delivery", "本轮尚无新的实际获取回执", "请先执行采集工具检查来源，或改用 outcome='partial' 提交说明")
	}
	if draft.Outcome == "completed" && len(draft.References) == 0 {
		return businessError("DELIVERY_EVIDENCE_MISSING", "delivery", "交付未选择支持结论的已保存证据", "提交 read_materials 返回的原文引用或观测 ID；仍有缺口时提交 partial 并说明")
	}
	if draft.Outcome == "no_match" && len(draft.References) == 0 {
		return businessError("INVESTIGATION_EVIDENCE_MISSING", "delivery", "确认无匹配前需保留调查抓取回执", "先完成调查抓取并保留回执后再提交 no_match")
	}
	seenTitles := map[string]bool{}
	for _, table := range draft.Tables {
		if len(table.RowObservationIDs) > 0 && len(table.RowObservationIDs) != len(table.Rows) {
			return businessError("DELIVERY_ROW_REFERENCES_MISMATCH", "delivery", "表格行数与来源条目数不一致", "为每行提供一组观测 ID，或给摘要表提供整份报告的原文引用")
		}
		if table.Title != "" {
			if seenTitles[table.Title] {
				return fail("表格名称重复: " + table.Title)
			}
			seenTitles[table.Title] = true
		}
	}
	if draft.Outcome == "completed" {
		return checkExplicitOutput(requirements.ExplicitOutput, draft)
	}
	return nil
}

func checkExplicitOutput(requirements []OutputRequirement, draft DeliveryDraft) error {
	var gaps []string
	for _, requirement := range requirements {
		target := strings.TrimSpace(requirement.Target)
		if requirement.Kind == "artifact" && target == "report" {
			if strings.TrimSpace(draft.Report) == "" {
				gaps = append(gaps, "缺少执行报告")
			}
			continue
		}

		var rows []map[string]any
		found := false
		for _, table := range draft.Tables {
			if target == "结果" || target == strings.TrimSpace(table.Title) {
				rows = append(rows, table.Rows...)
				found = true
			}
		}
		if !found {
			gaps = append(gaps, fmt.Sprintf("缺少交付表 %q（%s）", target, requirement.ID))
			continue
		}

		switch requirement.Kind {
		case "fields":
			for _, field := range requirement.Fields {
				missing := 0
				for _, row := range rows {
					if !deliveryValuePresent(row[field]) {
						missing++
					}
				}
				if len(rows) == 0 {
					gaps = append(gaps, fmt.Sprintf("表 %q 没有包含字段 %q 的记录", target, field))
				} else if missing > 0 {
					gaps = append(gaps, fmt.Sprintf("表 %q 有 %d 行缺少字段 %q", target, missing, field))
				}
			}
		case "count":
			if requirement.MinCount == nil {
				continue
			}
			count := countDeliveryRows(rows, deliveryCountFields(requirements, requirement.Target), requirement.CountBy)
			if count < *requirement.MinCount {
				unit := "记录"
				if len(requirement.CountBy) > 0 {
					unit = "按 " + strings.Join(requirement.CountBy, "、") + " 去重"
				}
				gaps = append(gaps, fmt.Sprintf("表 %q 至少需要 %d 项（%s），实有 %d", target, *requirement.MinCount, unit, count))
			}
		}
	}
	if len(gaps) == 0 {
		return nil
	}
	err := businessError("DELIVERY_OUTPUT_INCOMPLETE", "delivery", strings.Join(gaps, "。"),
		"根据缺口补采或整理表格后重新提交 completed。已取得部分数据时可提交 partial，列出未满足的要求")
	err.Details = map[string]any{"gaps": gaps}
	return err
}

func deliveryCountFields(requirements []OutputRequirement, target string) []string {
	var fields []string
	for _, requirement := range requirements {
		if requirement.Kind == "fields" && (requirement.Target == target || requirement.Target == "结果") {
			fields = append(fields, requirement.Fields...)
		}
	}
	return fields
}

type automaticOutputCounts struct {
	factsRevision, requirementsRevision uint64
	counts                              map[string]int
}

func (h *informationHost) automaticOutputCounts(ctx context.Context, requirements TaskRequirements) (map[string]int, error) {
	cache := h.outputCounts
	if cache != nil && cache.factsRevision == h.request.FactsRevision && cache.requirementsRevision == requirements.Revision {
		return cache.counts, nil
	}
	var automatic []OutputRequirement
	for _, requirement := range requirements.ExplicitOutput {
		if requirement.Kind == "count" && requirement.MinCount != nil &&
			(requirement.Target == "结果" || requirement.Target == defaultDeliveryTableTitle) {
			automatic = append(automatic, requirement)
		}
	}
	if len(automatic) == 0 {
		return nil, nil
	}
	base, err := h.service.observationQuery(ctx, h.request.ID, ObservationQuery{})
	if err != nil {
		return nil, err
	}
	var encoded []string
	if err := base.Pluck("fields_json", &encoded).Error; err != nil {
		return nil, err
	}
	rows := make([]map[string]any, 0, len(encoded))
	for _, fields := range encoded {
		var row map[string]any
		if err := json.Unmarshal([]byte(fields), &row); err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	counts := make(map[string]int, len(automatic))
	for _, requirement := range automatic {
		counts[requirement.ID] = countDeliveryRows(rows, deliveryCountFields(requirements.ExplicitOutput, requirement.Target), requirement.CountBy)
	}
	h.outputCounts = &automaticOutputCounts{h.request.FactsRevision, requirements.Revision, counts}
	return counts, nil
}

func countDeliveryRows(rows []map[string]any, requiredFields, countBy []string) int {
	count := 0
	seen := map[string]bool{}
	for _, row := range rows {
		hasData := false
		for _, value := range row {
			if deliveryValuePresent(value) {
				hasData = true
				break
			}
		}
		if !hasData {
			continue
		}
		for _, field := range requiredFields {
			if !deliveryValuePresent(row[field]) {
				hasData = false
				break
			}
		}
		if !hasData {
			continue
		}
		if len(countBy) > 0 {
			identity := make([]any, 0, len(countBy))
			for _, key := range countBy {
				value := row[key]
				if !deliveryValuePresent(value) {
					break
				}
				if text, ok := value.(string); ok {
					value = strings.TrimSpace(text)
				}
				identity = append(identity, value)
			}
			if len(identity) != len(countBy) {
				continue
			}
			encoded, err := json.Marshal(identity)
			if err != nil || seen[string(encoded)] {
				continue
			}
			seen[string(encoded)] = true
		}
		count++
	}
	return count
}

func deliveryValuePresent(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(v) != ""
	case []any:
		return len(v) > 0
	case []string:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	default:
		return true
	}
}
