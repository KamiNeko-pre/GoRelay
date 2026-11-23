package information

import (
	"context"
	"encoding/json"
	"go-lua-crawler/internal/agentdata"
)

type AssetCatalogEntry struct {
	ID           string          `json:"id"`
	Kind         string          `json:"kind"`
	Description  string          `json:"description"`
	URL          string          `json:"url"`
	Output       json.RawMessage `json:"output"`
	SourceID     string          `json:"source_id"`
	Version      string          `json:"version"`
	InputSchema  json.RawMessage `json:"input_schema,omitempty"`
	InputExample json.RawMessage `json:"input_example,omitempty"`
}
type AssetCatalogPage struct {
	Assets            []AssetCatalogEntry `json:"assets"`
	Total             int                 `json:"total"`
	NextOffset        *int                `json:"next_offset,omitempty"`
	CollectionVersion string              `json:"collection_version"`
}

func (s *Service) ListAcquisitionAssets(ctx context.Context, id string, page DocumentQuery) (AssetCatalogPage, error) {
	out := AssetCatalogPage{Assets: []AssetCatalogEntry{}}
	if page.Offset < 0 || page.Limit <= 0 {
		return out, businessError("INVALID_ASSET_PAGE", "assets", "offset 需非负且 limit 需为正整数", "从 offset=0 查询并按 next_offset 翻页")
	}
	_, err := s.Request(ctx, id)
	if err != nil {
		return out, err
	}
	if err := s.SyncLuaCatalog(ctx, id); err != nil {
		return out, err
	}
	entries, err := s.acquisitionCatalog(ctx)
	if err != nil {
		return out, err
	}
	out.Total = len(entries)
	out.CollectionVersion = agentdata.Hash(entries)
	start := min(page.Offset, len(entries))
	end := start + min(page.Limit, len(entries)-start)
	out.Assets = append(out.Assets, entries[start:end]...)
	if next := page.Offset + len(out.Assets); next < out.Total {
		out.NextOffset = &next
	}
	return out, nil
}

func (s *Service) acquisitionCatalog(ctx context.Context) ([]AssetCatalogEntry, error) {
	var rows []Asset
	if err := s.DB.WithContext(ctx).Where("active = ?", true).Order("id").Find(&rows).Error; err != nil {
		return nil, err
	}
	entries := []AssetCatalogEntry{}
	for _, a := range rows {
		entry := AssetCatalogEntry{ID: a.ID, Kind: a.Kind, Description: a.Description, URL: a.EntryURL, Output: validRaw(a.SpecJSON), SourceID: a.SourceID, Version: a.Version}
		if a.Kind == "lua" {
			var manifest struct {
				InputSchema  string `json:"input_schema"`
				InputExample string `json:"input_example"`
			}
			if err := json.Unmarshal([]byte(a.ManifestJSON), &manifest); err != nil {
				return nil, err
			}
			entry.InputSchema, entry.InputExample = validRaw(manifest.InputSchema), validRaw(manifest.InputExample)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}
