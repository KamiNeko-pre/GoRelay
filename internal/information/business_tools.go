package information

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/eino-contrib/jsonschema"
	"go-lua-crawler/internal/aiapp"
)

const defaultMaterialPreviewLimit = 20 // Existing observation preview size, shared by read_materials branches.

type MaterialReadQuery struct {
	Kind               string            `json:"kind,omitempty" jsonschema:"enum=observations,enum=assets,enum=delivery,enum=documents,enum=excerpts,description=查询类别：observations（默认，查看已落库结构化观测）、excerpts（按关键词跨已保存快照原文检索精准摘录，传入 queries/query）、documents（已关联快照清单）、assets（已发布资产）、delivery（历史基线交付）"`
	Query              string            `json:"query,omitempty" jsonschema:"description=仅在 kind='excerpts' 时按单个关键词或短语检索快照原文"`
	Queries            []string          `json:"queries,omitempty" jsonschema:"description=仅在 kind='excerpts' 时按多个关键词一次性批量检索已保存快照，返回可直接引用的原文摘录与 doc_id/revision"`
	ObservationIDs     []string          `json:"observation_ids,omitempty" jsonschema:"description=按指定观测 ID 列表过滤结构化观测"`
	DocumentIDs        []string          `json:"document_ids,omitempty" jsonschema:"description=仅查询指定快照或引用它们的观测，使用回执中的 doc_id"`
	SourceIDs          []string          `json:"source_ids,omitempty" jsonschema:"description=仅查询指定来源入口 URL 对应的快照或观测，使用 discover_sources(saved_only=true) 返回的来源 ID"`
	DeliveryID         string            `json:"delivery_id,omitempty"`
	Table              string            `json:"table,omitempty"`
	LinkObservationIDs []string          `json:"link_observation_ids,omitempty"`
	LinkReferences     []StoredReference `json:"link_references,omitempty"`
	Cursor             string            `json:"cursor,omitempty"`
	Offset             int               `json:"offset,omitempty" jsonschema:"minimum=0"`
	Limit              int               `json:"limit,omitempty" jsonschema:"minimum=0,description=页面预览行数。省略时使用默认预览页；按 next_offset 或 next_cursor 继续。"`
	exactReferences    []StoredReference
}

func (q MaterialReadQuery) documentQuery() DocumentQuery {
	return DocumentQuery{Offset: q.Offset, Limit: q.Limit, documentIDs: q.DocumentIDs, sourceIDs: q.SourceIDs}
}

func (q MaterialReadQuery) observationQuery() ObservationQuery {
	return ObservationQuery{ObservationIDs: q.ObservationIDs, Cursor: q.Cursor, Limit: q.Limit, documents: q.documentQuery()}
}

func businessToolDefinitions() []aiapp.ToolDefinition {
	types := []struct {
		name, description string
		input             any
	}{
		{"update_plan", "制定或更新爬取计划（interpretation、逐来源 sources、coverage_rationale）。", WorkPlan{}},
		{"revise_requirement_interpretation", "仅在系统提取的任务要求有明显误解时修订。", RequirementInterpretation{}},
		{"read_materials", "读取已保存材料：observations（已落库结构化数据）、excerpts（按 queries 检索已捕获快照的精确原文片段与引文定位）、documents（快照列表）、assets/delivery。", MaterialReadQuery{}},
		{"submit_delivery", "数据落库后提交完整交付报告（outcome；report 须直接回答题面要求的所有对比说明、计算公式、选型建议与实际查询范围；附 acquisition_receipt_ids、references 或汇总表、gaps）。", DeliveryDraft{}},
	}
	defs := make([]aiapp.ToolDefinition, 0, len(types))
	reflector := jsonschema.Reflector{DoNotReference: true}
	for _, t := range types {
		defs = append(defs, aiapp.ToolDefinition{Type: "function", Function: aiapp.FunctionDefinition{Name: t.name, Description: t.description, Parameters: mustMarshal(reflector.Reflect(t.input))}})
	}
	return defs
}

func businessInputSchema(input any) any {
	reflector := jsonschema.Reflector{DoNotReference: true}
	return reflector.Reflect(input)
}

func truncateTextRunes(s string, maxRunes int) string {
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes]) + "..."
}

func compactObservationPageForModel(page ObservationPage, maxFieldRunes int) map[string]any {
	if maxFieldRunes <= 0 {
		maxFieldRunes = 800
	}
	items := make([]map[string]any, 0, len(page.Items))
	for _, o := range page.Items {
		fieldsCopy := make(map[string]any, len(o.Fields))
		truncated := []string{}
		keys := make([]string, 0, len(o.Fields))
		for k := range o.Fields {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := o.Fields[k]
			switch tv := v.(type) {
			case string:
				runes := []rune(tv)
				if len(runes) > maxFieldRunes {
					fieldsCopy[k] = string(runes[:maxFieldRunes]) + "..."
					truncated = append(truncated, k)
				} else {
					fieldsCopy[k] = tv
				}
			case []string:
				if len(tv) > 20 {
					fieldsCopy[k] = tv[:20]
					truncated = append(truncated, k)
				} else {
					fieldsCopy[k] = tv
				}
			default:
				fieldsCopy[k] = v
			}
		}
		entry := map[string]any{
			"id":              o.ID,
			"receipt_id":      o.ReceiptID,
			"record_position": o.RecordPosition,
			"fields":          fieldsCopy,
			"references":      o.References,
			"fetched_at":      o.FetchedAt,
		}
		if len(truncated) > 0 {
			entry["truncated_fields"] = truncated
		}
		items = append(items, entry)
	}
	out := map[string]any{
		"mode":               "observations",
		"items":              items,
		"total":              page.Total,
		"collection_version": page.CollectionVersion,
	}
	if page.NextCursor != "" {
		out["next_cursor"] = page.NextCursor
	}
	return out
}

func (h *informationHost) dispatchBusiness(ctx context.Context, name, raw string, step Step) (any, error) {
	s := h.service
	switch name {
	case "revise_requirement_interpretation":
		var input RequirementInterpretation
		if err := decodeInformationArgs(raw, &input); err != nil {
			return nil, err
		}
		return s.ReviseRequirementInterpretation(ctx, h.request.ID, step.ID, input)
	case "update_plan":
		var plan WorkPlan
		if err := decodeInformationArgs(raw, &plan); err != nil {
			return nil, err
		}
		if err := s.UpdatePlan(ctx, h.request.ID, plan); err != nil {
			return nil, err
		}
		h.request.PlanJSON = string(mustMarshal(plan))
		return plan, nil
	case "read_materials":
		var q MaterialReadQuery
		if err := decodeInformationArgs(raw, &q); err != nil {
			return nil, err
		}
		if q.Limit == 0 {
			q.Limit = defaultMaterialPreviewLimit
		}
		if len(q.LinkObservationIDs) > 0 || len(q.LinkReferences) > 0 {
			if err := s.LinkMaterials(ctx, h.request.ID, MaterialLinks{ObservationIDs: q.LinkObservationIDs, Documents: q.LinkReferences}); err != nil {
				return nil, err
			}
			return map[string]any{"linked": true}, nil
		}
		kind := strings.ToLower(strings.TrimSpace(q.Kind))
		hasExcerptQuery := strings.TrimSpace(q.Query) != "" || len(q.Queries) > 0
		if kind == "" && hasExcerptQuery && q.DeliveryID == "" && len(q.ObservationIDs) == 0 {
			kind = "excerpts"
		}
		if hasExcerptQuery && kind != "excerpts" {
			return nil, businessError("INVALID_MATERIAL_FILTER", "facts", "query/queries 关键词摘录检索仅适用于 kind='excerpts'", "设置 kind='excerpts' 并传入 queries=[...]")
		}
		if (len(q.DocumentIDs) > 0 || len(q.SourceIDs) > 0) &&
			(q.DeliveryID != "" || kind == "delivery" || kind == "assets") {
			return nil, businessError("INVALID_MATERIAL_FILTER", "facts", "来源和文档过滤适用于 observations、documents 或 excerpts", "使用 kind='observations'、kind='documents' 或 kind='excerpts' 按来源查询")
		}
		if q.DeliveryID != "" || kind == "delivery" {
			return s.QueryDelivery(ctx, SavedDeliveryQuery{DeliveryID: q.DeliveryID, Table: q.Table, Offset: q.Offset, Limit: q.Limit})
		}
		if kind == "excerpts" {
			return s.QueryTaskDocumentExcerpts(ctx, h.request.ID, h.input.SourceScope, q)
		}
		if kind == "documents" {
			return s.QueryTaskDocuments(ctx, h.request.ID, q.documentQuery())
		}
		if kind == "assets" {
			return s.ListAcquisitionAssets(ctx, h.request.ID, DocumentQuery{Offset: q.Offset, Limit: q.Limit})
		}
		if kind != "" && kind != "observations" {
			return nil, businessError("INVALID_READ_KIND", "facts", fmt.Sprintf("不支持的 read_materials.kind=%q", kind), "仅支持 observations / excerpts / documents / delivery / assets")
		}
		query := q.observationQuery()
		obsPage, err := s.QueryObservations(ctx, h.request.ID, query)
		if err != nil {
			return nil, err
		}
		maxRunes := 480
		if len(q.ObservationIDs) > 0 {
			maxRunes = 720
		}
		out := compactObservationPageForModel(obsPage, maxRunes)
		if len(q.DocumentIDs) > 0 || len(q.SourceIDs) > 0 {
			out["filters"] = map[string]any{"document_ids": q.DocumentIDs, "source_ids": q.SourceIDs}
		}
		return out, nil
	case "submit_delivery":
		var draft DeliveryDraft
		if err := decodeInformationArgs(raw, &draft); err != nil {
			return nil, err
		}
		delivery, err := s.SubmitDelivery(ctx, h.request.ID, step.ID, draft)
		if err != nil {
			return nil, err
		}
		h.request.CurrentDeliveryID = delivery.ID
		h.request.Status = "completed"
		h.request.Stage = "completed"
		h.mu.Lock()
		h.finished = true
		h.mu.Unlock()
		return map[string]any{"delivery_id": delivery.ID, "outcome": normalizeDeliveryOutcome(draft.Outcome)}, nil
	}
	return nil, businessError("UNKNOWN_TOOL", "tools", "未知业务工具", "选择当前公布的工具名称和参数")
}
