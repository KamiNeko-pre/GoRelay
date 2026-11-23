package information

import "context"

type SavedDeliveryQuery struct {
	DeliveryID string `json:"delivery_id"`
	Table      string `json:"table,omitempty"`
	Offset     int    `json:"offset,omitempty"`
	Limit      int    `json:"limit,omitempty"`
}

// QueryDelivery reads immutable saved work on demand. A table page does not
// copy historical observations into the current task's acquisition set.
func (s *Service) QueryDelivery(ctx context.Context, query SavedDeliveryQuery) (map[string]any, error) {
	if query.DeliveryID == "" {
		return nil, businessError("DELIVERY_ID_REQUIRED", "delivery", "缺少 delivery_id", "使用 update_baseline 或已保存结果提供的实际交付编号")
	}
	delivery, draft, err := s.SavedDelivery(ctx, query.DeliveryID)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"delivery_id": delivery.ID, "request_id": delivery.RequestID, "outcome": draft.Outcome, "references": draft.References}
	if query.Table == "" {
		tables := []map[string]any{}
		for _, table := range draft.Tables {
			tables = append(tables, map[string]any{"title": table.Title, "columns": table.Columns, "row_count": len(table.Rows), "join_fields": table.JoinFields})
		}
		out["report"], out["actions"], out["gaps"], out["tables"] = draft.Report, draft.Actions, draft.Gaps, tables
		return out, nil
	}
	if query.Offset < 0 || query.Limit <= 0 {
		return nil, businessError("INVALID_DELIVERY_PAGE", "delivery", "读取表格时 offset 需非负且 limit 需为正整数", "使用查询返回的真实表名，从 offset=0 开始指定 limit，并按 next_offset 继续")
	}
	for _, table := range draft.Tables {
		if table.Title != query.Table {
			continue
		}
		total := len(table.Rows)
		start := min(query.Offset, total)
		end := start + min(query.Limit, total-start)
		table.Rows, table.RowObservationIDs = table.Rows[start:end], table.RowObservationIDs[start:end]
		out["table"], out["total"] = table, total
		if end < total {
			out["next_offset"] = end
		}
		return out, nil
	}
	return nil, businessError("DELIVERY_TABLE_NOT_FOUND", "delivery", "保存结果中没有该表："+query.Table, "省略 table 查询真实表名，再按返回名称读取；不猜测或重建旧结果")
}
