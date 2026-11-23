package information

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go-lua-crawler/internal/knowledge"
	"go-lua-crawler/internal/mcpserver"
	"gorm.io/gorm"
	"net/url"
	"strings"
)

// deliveryDisplay adapts a saved result to the current workbench's wire shape.
// It never merges rows, filters them, or rechecks delivery sufficiency.
func (s *Service) deliveryDisplay(ctx context.Context, id string) (map[string]any, error) {
	delivery, draft, err := s.SavedDelivery(ctx, id)
	if err != nil {
		return nil, err
	}
	rows := []map[string]any{}
	var request Request
	if err := s.DB.WithContext(ctx).First(&request, "id = ?", delivery.RequestID).Error; err != nil {
		return nil, err
	}
	var input Input
	if err := json.Unmarshal([]byte(request.InputJSON), &input); err != nil {
		return nil, err
	}
	metadata := map[string]map[string]any{}
	documents := func(refs []StoredReference, scope []string) ([]map[string]any, error) {
		out := []map[string]any{}
		seen := map[string]bool{}
		for _, ref := range refs {
			key := ref.DocID + "@" + ref.Revision
			if seen[key] {
				continue
			}
			seen[key] = true
			accessKey := key + "|" + strings.Join(scope, ",")
			item, exists := metadata[accessKey]
			if !exists {
				doc, err := knowledge.NewStore(s.DB).ResolveDocument(ctx, ref.DocID, ref.Revision, knowledge.RetrievalScope{URLHosts: scope})
				if err != nil {
					if !errors.Is(err, knowledge.ErrInvalidReference) && !errors.Is(err, gorm.ErrRecordNotFound) {
						return nil, err
					}
					item = map[string]any{"doc_id": ref.DocID, "revision": ref.Revision, "available": false, "unavailable_reason": "该依据已撤销、超出访问范围或原文版本不存在"}
				} else {
					item = map[string]any{"doc_id": doc.ID, "revision": doc.Revision, "available": true, "title": doc.Title, "url": doc.URL, "fetched_at": doc.FetchedAt, "snapshot_url": mcpserver.RequestBaseURL(ctx) + "/evidence#id=" + url.QueryEscape(doc.ID) + "&revision=" + url.QueryEscape(doc.Revision)}
				}
				metadata[accessKey] = item
			}
			out = append(out, item)
		}
		return out, nil
	}
	refByObservation := make(map[string][]StoredReference)
	if ids := collectDeliveryObservationIDs(draft); len(ids) > 0 {
		var facts []observationRow
		if err := s.DB.WithContext(ctx).Select("id, references_json").Where("id IN ?", ids).Find(&facts).Error; err != nil {
			return nil, err
		}
		for _, fact := range facts {
			var refs []StoredReference
			if err := json.Unmarshal([]byte(fact.ReferencesJSON), &refs); err != nil {
				return nil, err
			}
			refByObservation[fact.ID] = refs
		}
	}
	for _, table := range draft.Tables {
		for i, row := range table.Rows {
			refs := []StoredReference{}
			for _, id := range table.RowObservationIDs[i] {
				refs = append(refs, refByObservation[id]...)
			}
			docs, err := documents(refs, input.SourceScope)
			if err != nil {
				return nil, err
			}
			rows = append(rows, map[string]any{"key": fmt.Sprintf("%s:%d", table.Title, i), "table_title": table.Title, "fields": row, "observation_ids": table.RowObservationIDs[i], "evidence_documents": docs, "references": refs})
		}
	}
	docs, err := documents(draft.References, input.SourceScope)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"delivery_id": delivery.ID, "request_id": delivery.RequestID, "goal": input.Goal, "outcome": draft.Outcome, "report": draft.Report, "tables": draft.Tables, "records": rows, "references": draft.References, "evidence_documents": docs, "actions": draft.Actions, "gaps": draft.Gaps, "requirement_responses": draft.RequirementResponses, "completion_rationale": draft.CompletionRationale}
	if request.BaselineDeliveryID != "" {
		comparison, err := s.CompareDeliveries(ctx, request.BaselineDeliveryID, delivery.ID)
		if err != nil {
			return nil, err
		}
		baseline, old, err := s.SavedDelivery(ctx, request.BaselineDeliveryID)
		if err != nil {
			return nil, err
		}
		parent, err := s.Request(ctx, baseline.RequestID)
		if err != nil {
			return nil, err
		}
		var original Input
		if err := json.Unmarshal([]byte(parent.InputJSON), &original); err != nil {
			return nil, err
		}
		refs := append([]StoredReference{}, old.References...)
		ids := []string{}
		for _, table := range old.Tables {
			for _, rowIDs := range table.RowObservationIDs {
				ids = append(ids, rowIDs...)
			}
		}
		if len(ids) > 0 {
			var facts []observationRow
			if err := s.DB.WithContext(ctx).Where("id IN ?", ids).Order("id").Find(&facts).Error; err != nil {
				return nil, err
			}
			for _, row := range facts {
				fact, err := decodeObservation(row)
				if err != nil {
					return nil, err
				}
				refs = append(refs, fact.References...)
			}
		}
		before, err := documents(refs, original.SourceScope)
		if err != nil {
			return nil, err
		}
		out["comparison"], out["changes"], out["baseline_evidence_documents"] = comparison, comparison.Changes, before
	}
	return out, nil
}

func (s *Service) observationPreview(ctx context.Context, id string) (map[string]any, error) {
	var count int64
	if err := s.DB.WithContext(ctx).Model(&taskObservation{}).Where("request_id = ?", id).Count(&count).Error; err != nil {
		return nil, err
	}
	rows := []map[string]any{}
	if count > 0 {
		page, err := s.QueryObservations(ctx, id, ObservationQuery{Limit: int(count)})
		if err != nil {
			return nil, err
		}
		for _, o := range page.Items {
			rows = append(rows, map[string]any{"key": o.ID, "fields": o.Fields, "references": o.References})
		}
	}
	return map[string]any{"records": rows, "total": count, "preview": true}, nil
}
