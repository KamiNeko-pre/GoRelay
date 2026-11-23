package information

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"go-lua-crawler/internal/agentdata"
)

type sourceDirectoryQuery struct {
	Query  string `json:"query"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
}

func (h *informationHost) bindCurrentRead(ctx context.Context, name, raw string) (string, error) {
	switch name {
	case "run_lua":
		if validateInformationToolArgs(name, raw) != nil {
			return raw, nil
		}
		var args luaRunArgs
		if err := decodeInformationArgs(raw, &args); err != nil {
			return raw, err
		}
		if args.SourceRef != nil {
			return raw, nil
		}
		targets, err := resolveLuaTargets(args, h.input.EntryURLs, h.input.SourceScope)
		if err != nil {
			return raw, err
		}
		args.URL, args.URLs, args.Params, args.Targets = "", nil, nil, targets
		return string(mustMarshal(args)), nil
	case "submit_delivery":
		if h.service == nil || h.service.DB == nil {
			return raw, nil
		}
		if validateInformationToolArgs("submit_delivery", raw) != nil {
			return raw, nil
		}
		var argsMap map[string]any
		decoder := json.NewDecoder(strings.NewReader(cleanJSONString(raw)))
		decoder.UseNumber()
		if err := decoder.Decode(&argsMap); err != nil || argsMap == nil {
			return raw, nil
		}
		var reqs TaskRequirements
		if h.request.RequirementsJSON != "" {
			_ = json.Unmarshal([]byte(h.request.RequirementsJSON), &reqs)
		}
		if reqs.Revision == 0 {
			reqs.Revision = h.request.RequirementsRevision
		}
		var draft DeliveryDraft
		if err := decodeInformationArgs(raw, &draft); err != nil {
			return raw, nil
		}
		normalizeDeliveryDraft(reqs, &draft)
		if _, has := argsMap["requirements_revision"]; !has || argsMap["requirements_revision"] == nil {
			argsMap["requirements_revision"] = draft.RequirementsRevision
		} else if n, ok := toolArgumentNumber(argsMap["requirements_revision"]); ok && n.Sign() == 0 {
			argsMap["requirements_revision"] = draft.RequirementsRevision
		}
		if draft.Outcome != "" {
			argsMap["outcome"] = draft.Outcome
		}
		if len(draft.Gaps) > 0 {
			if existing, ok := argsMap["gaps"].([]any); !ok || len(existing) == 0 {
				argsMap["gaps"] = draft.Gaps
			}
		}
		bound, err := json.Marshal(argsMap)
		if err != nil {
			return raw, err
		}
		return string(bound), nil
	}
	return raw, nil
}

// Read identity belongs to the host and to this tool's actual data set. It is
// neither a model parameter nor a digest of unrelated task state.
func (h *informationHost) currentReadVersion(ctx context.Context, name, raw string) (string, error) {
	if validateInformationToolArgs(name, raw) != nil {
		return "", nil
	}
	s := h.service
	switch name {
	case "discover_sources":
		var args struct {
			Query     string `json:"query"`
			SavedOnly bool   `json:"saved_only"`
		}
		if err := decodeInformationArgs(raw, &args); err != nil || !args.SavedOnly {
			return "", err
		}
		sources, err := s.knownSources(ctx, h.input.SourceScope)
		if err != nil {
			return "", err
		}
		page, err := sourceDirectoryPage(sources, args.Query, 0, 0)
		if err != nil {
			return "", err
		}
		return page["collection_version"].(string), nil
	case "read_materials":
		var args MaterialReadQuery
		if err := decodeInformationArgs(raw, &args); err != nil {
			return "", err
		}
		kind := strings.ToLower(strings.TrimSpace(args.Kind))
		hasExcerptQuery := strings.TrimSpace(args.Query) != "" || len(args.Queries) > 0
		if kind == "" && hasExcerptQuery && args.DeliveryID == "" && len(args.ObservationIDs) == 0 {
			kind = "excerpts"
		}
		if len(args.LinkObservationIDs) > 0 || len(args.LinkReferences) > 0 ||
			args.DeliveryID != "" || kind == "delivery" {
			return "", nil
		}
		if kind == "documents" || kind == "excerpts" {
			rows, err := s.taskDocuments(ctx, h.request.ID, args.documentQuery())
			return agentdata.Hash(rows), err
		}
		if kind == "assets" {
			if err := s.SyncLuaCatalog(ctx, h.request.ID); err != nil {
				return "", err
			}
			assets, err := s.acquisitionCatalog(ctx)
			return agentdata.Hash(assets), err
		}
		base, err := s.observationQuery(ctx, h.request.ID, args.observationQuery())
		if err != nil {
			return "", err
		}
		ids := []string{}
		err = base.Order("id").Pluck("id", &ids).Error
		return agentdata.Hash(ids), err
	}
	return "", nil
}

func sourceDirectoryPage(sources []DiscoveredSource, query string, offset, limit int) (map[string]any, error) {
	if offset < 0 || limit < 0 {
		return nil, fmt.Errorf("offset and limit must be nonnegative")
	}
	if limit == 0 {
		limit = len(sources)
	}
	rows := []DiscoveredSource{}
	terms := strings.Fields(strings.ToLower(query))
	for _, source := range sources {
		text := strings.ToLower(source.Title + " " + source.URL + " " + source.Query)
		match := true
		for _, term := range terms {
			match = match && strings.Contains(text, term)
		}
		if match {
			rows = append(rows, source)
		}
	}
	total := len(rows)
	if offset > total {
		offset = total
	}
	end := total
	if limit < total-offset {
		end = offset + limit
	}
	return map[string]any{"sources": rows[offset:end], "total": total, "offset": offset, "next_offset": end, "has_more": end < total, "collection_version": agentdata.Hash(rows), "meaning": "stored source entries are reusable candidates, not proof of current business facts"}, nil
}
