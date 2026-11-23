package information

import (
	"context"
	"encoding/json"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"strings"
)

// requirementsFromInput is the only adapter for the external legacy spec.
// A collector schema is never stored in the parent task or used to admit tools.
func requirementsFromInput(in Input) (TaskRequirements, error) {
	requirements := TaskRequirements{Revision: 1, OriginalText: in.Goal, SourceScope: in.SourceScope}
	if strings.TrimSpace(in.Guidance) != "" {
		requirements.Additions = []string{in.Guidance}
	}
	if in.Spec == nil {
		return requirements, nil
	}
	spec := in.Spec
	unsupported := func(path string) (TaskRequirements, error) {
		err := businessError("UNSUPPORTED_REQUIREMENT", "requirements", "无法将旧结构要求转换为任务要求："+path, "将该要求写入 goal；采集字段和关联方式由 Agent 在具体获取工具中设置")
		err.Details = map[string]any{"path": path}
		return requirements, err
	}
	for _, item := range []struct {
		path    string
		present bool
	}{
		{"spec.goal", spec.Goal != "" && strings.TrimSpace(spec.Goal) != in.Goal},
		{"spec.entities", len(spec.Entities) > 0}, {"spec.entity_field", spec.EntityField != ""},
		{"spec.join_keys", len(spec.JoinKeys) > 0}, {"spec.source_namespace", spec.SourceNamespace != ""},
		{"spec.max_age_seconds", spec.MaxAgeSeconds != 0},
		{"spec.require_published_capability", spec.RequirePublishedCapability},
		{"spec.require_extractive_summary", spec.RequireExtractiveSummary},
		{"spec.requirements", len(spec.Requirements) > 0}, {"spec.required_combinations", len(spec.RequiredCombinations) > 0},
		{"spec.require_field_support", spec.RequireFieldSupport},
	} {
		if item.present {
			return unsupported(item.path)
		}
	}
	fields := []string{}
	for _, field := range spec.RequiredFields {
		fields = append(fields, field.Name)
	}
	if len(fields) > 0 {
		original, _ := json.Marshal(spec.RequiredFields)
		requirements.ExplicitOutput = append(requirements.ExplicitOutput, OutputRequirement{ID: "input.fields", Origin: "user_input", SourceText: string(original), Kind: "fields", Target: "结果", Fields: fields})
	}
	if spec.MinRecords != 0 {
		if spec.MinRecords < 0 {
			return unsupported("spec.min_records")
		}
		minimum := spec.MinRecords
		requirements.ExplicitOutput = append(requirements.ExplicitOutput, OutputRequirement{ID: "input.count", Origin: "user_input", Kind: "count", Target: "结果", MinCount: &minimum})
	}
	if spec.RequireSummary {
		requirements.ExplicitOutput = append(requirements.ExplicitOutput, OutputRequirement{ID: "input.report", Origin: "user_input", Kind: "artifact", Target: "report"})
	}
	return requirements, nil
}

// ReviseRequirementInterpretation changes only the model's extraction of an
// explicit sentence. User input and every previous interpretation remain saved.
func (s *Service) ReviseRequirementInterpretation(ctx context.Context, id string, receiptID uint64, input RequirementInterpretation) (RequirementInterpretationChange, error) {
	var change RequirementInterpretationChange
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var request Request
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&request, "id = ?", id).Error; err != nil {
			return err
		}
		var receipt Step
		if err := tx.First(&receipt, "id = ? AND request_id = ?", receiptID, id).Error; err != nil {
			return err
		}
		var requirements TaskRequirements
		if err := json.Unmarshal([]byte(request.RequirementsJSON), &requirements); err != nil {
			return err
		}
		for _, old := range requirements.InterpretationChanges {
			if old.ReceiptID == receiptID {
				change = old
				return nil
			}
		}
		if terminal(request.Status) {
			return businessError("TASK_TERMINAL", "requirements", "任务已结束", "为新的用户要求建立任务，已有交付保持不变")
		}
		if input.RequirementsRevision != requirements.Revision {
			return businessError("REQUIREMENTS_CHANGED", "requirements", "要求版本已变化", "读取当前 requirements_revision 后修正本次解释")
		}
		if input.RequirementID == "" || strings.TrimSpace(input.SourceText) == "" || strings.TrimSpace(input.Reason) == "" {
			return businessError("INVALID_REQUIREMENT_INTERPRETATION", "requirements", "缺少 requirement_id、原句或修正原因", "引用用户原句，说明修正前后的目标产物、数量单位或字段含义")
		}
		foundSentence := strings.Contains(requirements.OriginalText, input.SourceText)
		for _, addition := range requirements.Additions {
			foundSentence = foundSentence || strings.Contains(addition, input.SourceText)
		}
		if !foundSentence {
			normInput := cleanSentenceForMatch(input.SourceText)
			if normInput != "" {
				if strings.Contains(cleanSentenceForMatch(requirements.OriginalText), normInput) {
					foundSentence = true
				}
				for _, addition := range requirements.Additions {
					if strings.Contains(cleanSentenceForMatch(addition), normInput) {
						foundSentence = true
						break
					}
				}
			}
		}
		if !foundSentence {
			return businessError("REQUIREMENT_SOURCE_NOT_FOUND", "requirements", "source_text 不是保存的用户原句", "从 original_text 或 additions 复制对应原句；计划推断只写入 update_plan")
		}
		next := []OutputRequirement{}
		for _, old := range requirements.ExplicitOutput {
			if old.ID != input.RequirementID {
				next = append(next, old)
				continue
			}
			if old.Origin == "user_input" {
				return businessError("EXPLICIT_INPUT_IMMUTABLE", "requirements", "结构化用户要求不能由模型覆盖", "保留显式用户要求；仅修正从原话提取的解释，需要用户改变要求时使用 ask_user")
			}
			previous := old
			change.Previous = &previous
		}
		if input.Output == nil && change.Previous == nil {
			return businessError("REQUIREMENT_NOT_FOUND", "requirements", "没有可移除的旧解释", "使用已有 requirement_id，或提供 output 新增原句的显式要求解释")
		}
		if input.Output != nil {
			output := *input.Output
			output.ID, output.Origin, output.SourceText = input.RequirementID, "interpretation", input.SourceText
			// Auto-infer kind if not explicitly artifact/fields/count
			switch output.Kind {
			case "artifact", "fields", "count":
			default:
				if len(output.Fields) > 0 {
					output.Kind = "fields"
				} else if output.MinCount != nil && *output.MinCount > 0 {
					output.Kind = "count"
				} else if output.Target != "" {
					output.Kind = "artifact"
				}
			}
			valid := output.Target != ""
			switch output.Kind {
			case "artifact":
			case "fields":
				valid = valid && len(output.Fields) > 0
			case "count":
				valid = valid && output.MinCount != nil && *output.MinCount > 0
			default:
				valid = false
			}
			if !valid {
				return businessError("INVALID_OUTPUT_REQUIREMENT", "requirements", "output 的产物、字段或明确数量不完整", "kind 使用 artifact/fields/count，指定 target；数量使用用户明确值及 count_by 单位，模糊充分性留在计划中")
			}
			change.Current = &output
			next = append(next, output)
		}
		requirements.Revision++
		change.ReceiptID, change.Revision, change.SourceText, change.Reason = receiptID, requirements.Revision, input.SourceText, input.Reason
		requirements.ExplicitOutput = next
		requirements.InterpretationChanges = append(requirements.InterpretationChanges, change)
		return tx.Model(&Request{}).Where("id = ?", id).Updates(map[string]any{"requirements_json": string(mustMarshal(requirements)), "requirements_revision": requirements.Revision}).Error
	})
	return change, err
}

func cleanSentenceForMatch(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "。，,、；;：:!！?？\"'“”‘’（）()")
	return strings.Join(strings.Fields(s), " ")
}
