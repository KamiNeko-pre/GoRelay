package information

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"go-lua-crawler/internal/recorddiff"
)

type DeliveryChange struct {
	Table string `json:"table"`
	recorddiff.BusinessChange
}

type DeliveryComparison struct {
	BaselineDeliveryID string           `json:"baseline_delivery_id"`
	BaselineRequestID  string           `json:"baseline_request_id"`
	CurrentDeliveryID  string           `json:"current_delivery_id"`
	Notice             string           `json:"notice,omitempty"`
	Changes            []DeliveryChange `json:"changes"`
}

// Comparison reads immutable deliveries. It never reclassifies saved facts or
// changes whether either task was delivered.
func (s *Service) CompareDeliveries(ctx context.Context, baselineID, currentID string) (DeliveryComparison, error) {
	before, old, err := s.SavedDelivery(ctx, baselineID)
	if err != nil {
		return DeliveryComparison{}, err
	}
	_, current, err := s.SavedDelivery(ctx, currentID)
	if err != nil {
		return DeliveryComparison{}, err
	}
	result := compareDeliveryTables(old, current)
	result.BaselineDeliveryID, result.BaselineRequestID, result.CurrentDeliveryID = before.ID, before.RequestID, currentID
	return result, nil
}

func compareDeliveryTables(before, after DeliveryDraft) DeliveryComparison {
	result := DeliveryComparison{Changes: []DeliveryChange{}}
	notices := []string{}
	if before.Outcome == "partial" || after.Outcome == "partial" {
		notices = append(notices, "存在部分交付，差异只反映已取得的数据；未出现的记录不代表来源已删除。")
	}
	old, current := map[string]DeliveryTable{}, map[string]DeliveryTable{}
	names := map[string]bool{}
	for _, table := range before.Tables {
		old[table.Title], names[table.Title] = table, true
	}
	for _, table := range after.Tables {
		current[table.Title], names[table.Title] = table, true
	}
	ordered := []string{}
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	for _, name := range ordered {
		a, b := old[name], current[name]
		var changes []recorddiff.BusinessChange
		useIdentity := false
		keys := b.JoinFields
		if len(keys) == 0 {
			keys = a.JoinFields
		}
		if len(keys) > 0 && (len(a.JoinFields) == 0 || len(b.JoinFields) == 0 || sameViewIDs(a.JoinFields, b.JoinFields)) {
			var err error
			changes, err = recorddiff.Diff(a.Rows, b.Rows, keys)
			useIdentity = err == nil
		}
		if !useIdentity {
			changes = compareWholeRows(a.Rows, b.Rows)
			notices = append(notices, name+"按完整记录比较；改动会显示为一条移除和一条新增，不推测它们属于同一对象。")
		}
		for _, change := range changes {
			result.Changes = append(result.Changes, DeliveryChange{Table: name, BusinessChange: change})
		}
	}
	result.Notice = strings.Join(notices, " ")
	return result
}

func compareWholeRows(before, after []map[string]any) []recorddiff.BusinessChange {
	counts := map[string]int{}
	values := map[string]map[string]any{}
	for sign, rows := range [][]map[string]any{before, after} {
		for _, row := range rows {
			raw := string(mustMarshal(row))
			values[raw] = row
			counts[raw] += 2*sign - 1
		}
	}
	keys := []string{}
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	changes := []recorddiff.BusinessChange{}
	for _, key := range keys {
		count := counts[key]
		identity, _ := json.Marshal([]any{values[key]})
		for count > 0 {
			changes = append(changes, recorddiff.BusinessChange{Key: string(identity), Kind: "record_added", After: values[key]})
			count--
		}
		for count < 0 {
			changes = append(changes, recorddiff.BusinessChange{Key: string(identity), Kind: "record_removed", Before: values[key]})
			count++
		}
	}
	return changes
}
