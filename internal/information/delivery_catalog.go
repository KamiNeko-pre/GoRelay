package information

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"

	"go-lua-crawler/internal/mcpserver"
)

func (b sourceBackend) FindDeliveries(ctx context.Context, input mcpserver.DeliveryQuery) (mcpserver.DeliveryPage, error) {
	out := mcpserver.DeliveryPage{Deliveries: []mcpserver.DeliveryEntry{}}
	if _, err := b.store(); err != nil {
		return out, err
	}
	if input.PageSize < 0 {
		return out, businessError("INVALID_DELIVERY_PAGE", "delivery_catalog", "page_size 不能为负数", "指定每页条数并按 next_cursor 继续")
	}
	if input.PageSize == 0 {
		input.PageSize = 20
	}
	terms := strings.Fields(strings.ToLower(input.Query))
	search := strings.Join(terms, " ")
	query := b.service.DB.WithContext(ctx).Table("information_deliveries d").Joins("JOIN information_requests r ON r.id = d.request_id")
	for _, word := range terms {
		query = query.Where("LOWER(JSON_UNQUOTE(JSON_EXTRACT(r.input_json, '$.goal'))) LIKE ?", "%"+word+"%")
	}
	if err := query.Count(&out.Total).Error; err != nil {
		return out, err
	}
	if input.Cursor != "" {
		var cursor struct {
			Query string
			ID    string
		}
		raw, err := base64.RawURLEncoding.DecodeString(input.Cursor)
		if err != nil || json.Unmarshal(raw, &cursor) != nil || cursor.Query != search || cursor.ID == "" {
			return out, businessError("DELIVERY_CURSOR_INVALID", "delivery_catalog", "交付目录游标与当前查询不匹配", "修改搜索条件后清空 cursor；翻页时使用同一查询及返回的 next_cursor")
		}
		var previous Delivery
		if err := b.service.DB.WithContext(ctx).First(&previous, "id = ?", cursor.ID).Error; err != nil {
			return out, businessError("DELIVERY_CURSOR_INVALID", "delivery_catalog", "交付目录游标不存在", "清空 cursor 重新搜索，随后使用返回的 next_cursor")
		}
		query = query.Where("d.created_at < ? OR (d.created_at = ? AND d.id < ?)", previous.CreatedAt, previous.CreatedAt, previous.ID)
	}
	var rows []struct {
		ID, RequestID, InputJSON, PayloadJSON, BaselineDeliveryID string
		CreatedAt                                                 time.Time
	}
	if err := query.Select("d.id, d.request_id, d.payload_json, d.created_at, r.input_json, r.baseline_delivery_id").Order("d.created_at DESC, d.id DESC").Limit(input.PageSize + 1).Find(&rows).Error; err != nil {
		return out, err
	}
	if len(rows) > input.PageSize {
		rows = rows[:input.PageSize]
		out.NextCursor = base64.RawURLEncoding.EncodeToString(mustMarshal(struct {
			Query string
			ID    string
		}{search, rows[len(rows)-1].ID}))
	}
	for _, row := range rows {
		var task Input
		var result struct {
			Outcome string `json:"outcome"`
		}
		if err := json.Unmarshal([]byte(row.InputJSON), &task); err != nil {
			return out, err
		}
		if err := json.Unmarshal([]byte(row.PayloadJSON), &result); err != nil {
			return out, err
		}
		out.Deliveries = append(out.Deliveries, mcpserver.DeliveryEntry{ID: row.ID, RequestID: row.RequestID, Goal: task.Goal, Outcome: result.Outcome, BaselineID: row.BaselineDeliveryID, CreatedAt: row.CreatedAt})
	}
	return out, nil
}

func (b sourceBackend) ReadDelivery(ctx context.Context, input mcpserver.DeliveryID) (map[string]any, error) {
	if _, err := b.store(); err != nil {
		return nil, err
	}
	if input.ID == "" {
		return nil, businessError("DELIVERY_ID_REQUIRED", "delivery_catalog", "缺少 delivery_id", "从 gorelay_find_deliveries 或任务结果复制实际 delivery_id")
	}
	return b.service.deliveryDisplay(ctx, input.ID)
}
