package information

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/aiapp"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// AcquisitionRequest describes one source operation. It carries no parent
// quotas, recommendations, report requirements, or research approval state.
type AcquisitionRequest struct {
	Goal     string                   `json:"goal"`
	Fields   []aiapp.FieldRequirement `json:"fields"`
	JoinKeys []string                 `json:"join_keys,omitempty"`
}
type acquisitionSpecKey struct{}
type acquisitionReceiptKey struct{}

func acquisitionOperationKey(ctx context.Context, requestID string) (string, error) {
	receiptID, ok := ctx.Value(acquisitionReceiptKey{}).(uint64)
	if !ok || receiptID == 0 {
		return "", fmt.Errorf("acquisition host receipt is missing")
	}
	return fmt.Sprintf("information:%s:receipt:%d", requestID, receiptID), nil
}

// The admitted operation owns its child. Recovery and accounting read this
// relation instead of maintaining another history in the task context.
func (s *Service) bindAcquisitionChild(ctx context.Context, r *Request, childID string) error {
	receiptID, ok := ctx.Value(acquisitionReceiptKey{}).(uint64)
	if !ok || receiptID == 0 || childID == "" {
		return fmt.Errorf("acquisition child requires its host receipt")
	}
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := requireNativeCommitOwner(tx, ctx, *r, Step{RequestID: r.ID}); err != nil {
			return err
		}
		var step Step
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&step, "id = ? AND request_id = ?", receiptID, r.ID).Error; err != nil {
			return err
		}
		if step.ChildRunID != "" {
			if step.ChildRunID != childID {
				return agentdata.ErrConflict
			}
			return nil
		}
		return tx.Model(&Step{}).Where("id = ?", step.ID).Update("child_run_id", childID).Error
	})
	if err == nil {
		r.ChildRunID = childID
	}
	return err
}

func acquisitionValidationError(err error) *BusinessError {
	errMsg := err.Error()
	action := "在 acquisition 中提供本次采集目标、实际字段及类型；关联键只能引用本次字段"
	details := map[string]any{
		"supported_field_types": []string{"string", "source_text", "number", "integer", "boolean", "string_array"},
	}
	if strings.Contains(errMsg, "unsupported field type") {
		action = "字段类型不受支持。支持的字段类型为：string, number, integer, boolean, string_array, source_text。日期、URL、时间或普通文本请指定为 'string'；整数指定为 'integer'；浮点数指定为 'number'。请调整 acquisition.fields 中的 type 后重新调用。"
	} else if strings.Contains(errMsg, "join key") {
		action = "关联键必须引用 fields 列表中已声明的字段名。请检查 join_keys 拼写并确保其在 fields 中已定义。"
	} else if strings.Contains(errMsg, "goal is required") {
		action = "缺少采集目标。请在 acquisition.goal 中提供具体的采集目标描述。"
	} else if strings.Contains(errMsg, "empty or duplicate required field") {
		action = "字段名不能为空且不能重复。请检查 fields 列表中每个字段的 name 属性。"
	}
	bErr := businessError("INVALID_ACQUISITION_REQUEST", "acquisition", errMsg, action)
	bErr.Details = details
	return bErr
}

// ensureAcquisitionFromFieldNames populates missing goal/fields on an optional
// AcquisitionRequest using the field names already declared in api.fields or
// browser.fields, avoiding redundant schema errors.
func ensureAcquisitionFromFieldNames(req *AcquisitionRequest, defaultGoal string, fieldNames []string) AcquisitionRequest {
	var out AcquisitionRequest
	if req != nil {
		out = *req
	}
	if strings.TrimSpace(out.Goal) == "" {
		out.Goal = strings.TrimSpace(defaultGoal)
	}
	if len(out.Fields) == 0 && len(fieldNames) > 0 {
		sorted := make([]string, 0, len(fieldNames))
		seen := map[string]bool{}
		for _, name := range fieldNames {
			name = strings.TrimSpace(name)
			if name == "" || name == "source_url" || seen[name] {
				continue
			}
			seen[name] = true
			sorted = append(sorted, name)
		}
		sort.Strings(sorted)
		for _, name := range sorted {
			out.Fields = append(out.Fields, aiapp.FieldRequirement{Name: name, Type: "string"})
		}
	}
	return out
}

func withLocalAcquisition(ctx context.Context, request AcquisitionRequest) (context.Context, error) {
	for i, f := range request.Fields {
		t := strings.ToLower(strings.TrimSpace(f.Type))
		switch t {
		case "date", "datetime", "time", "url", "text", "markdown", "html", "link":
			request.Fields[i].Type = "string"
		case "float", "double", "decimal":
			request.Fields[i].Type = "number"
		case "int":
			request.Fields[i].Type = "integer"
		case "bool":
			request.Fields[i].Type = "boolean"
		case "array", "list":
			request.Fields[i].Type = "string_array"
		}
	}
	spec := aiapp.TaskSpec{Goal: request.Goal, ContractVersion: "v2", SchemaVersion: "v1", RequiredFields: request.Fields, JoinKeys: request.JoinKeys}
	if err := spec.Validate(); err != nil {
		return ctx, acquisitionValidationError(err)
	}
	return context.WithValue(ctx, acquisitionSpecKey{}, spec), nil
}
func localAcquisition(ctx context.Context) (aiapp.TaskSpec, error) {
	spec, ok := ctx.Value(acquisitionSpecKey{}).(aiapp.TaskSpec)
	if !ok {
		return spec, businessError("ACQUISITION_REQUEST_REQUIRED", "acquisition", "缺少本次获取的局部字段和目标", "提供 acquisition.goal、acquisition.fields 及必要的 join_keys，无需冻结整个任务")
	}
	return spec, nil
}
func withAssetAcquisition(ctx context.Context, asset Asset) (context.Context, error) {
	var spec aiapp.TaskSpec
	if err := json.Unmarshal([]byte(asset.SpecJSON), &spec); err != nil {
		return ctx, err
	}
	return withLocalAcquisition(ctx, AcquisitionRequest{Goal: spec.Goal, Fields: spec.RequiredFields, JoinKeys: spec.JoinKeys})
}
