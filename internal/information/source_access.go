package information

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/knowledge"
	"go-lua-crawler/internal/mcpserver"
	task "go-lua-crawler/internal/task"
)

func recordSchema(raw string) (map[string]any, error) {
	var spec aiapp.TaskSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		return nil, err
	}
	properties := map[string]any{}
	var required []string
	for _, field := range spec.RequiredFields {
		schema := map[string]any{"type": field.Type}
		switch field.Type {
		case "source_text":
			schema = map[string]any{"type": "string", "description": "Text extracted from the source"}
		case "string_array":
			schema = map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
		}
		properties[field.Name] = schema
		required = append(required, field.Name)
	}
	return map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": properties, "required": required}}, nil
}

func (b sourceBackend) FindSources(ctx context.Context, in mcpserver.SourceQuery) (mcpserver.SourcePage, error) {
	out := mcpserver.SourcePage{Sources: []mcpserver.Source{}}
	if _, err := b.store(); err != nil {
		return out, err
	}
	if in.PageSize < 0 {
		return out, fmt.Errorf("page_size must be positive or omitted")
	}
	if in.PageSize == 0 {
		in.PageSize = 20
	}
	store := agentdata.NewStore(b.service.DB)
	versions, err := store.ListSources(ctx)
	if err != nil {
		return out, err
	}
	catalog := map[string]mcpserver.Source{}
	for _, version := range versions {
		artifact, err := store.Artifact(ctx, version.ArtifactHash)
		if err != nil {
			return out, err
		}
		entry := mcpserver.Source{ID: version.SourceID, Kind: "lua", Role: "acquisition", State: "active", Description: version.Description, Version: version.ArtifactHash, CanExecute: true}
		if err := json.Unmarshal([]byte(artifact.InputSchema), &entry.InputSchema); err != nil {
			return out, fmt.Errorf("source %s has invalid saved input schema: %w", entry.ID, err)
		}
		var example map[string]any
		if err := json.Unmarshal([]byte(artifact.InputExampleJSON), &example); err != nil {
			return out, fmt.Errorf("source %s has invalid saved input example: %w", entry.ID, err)
		}
		entry.EntryURL, _ = example["url"].(string)
		if entry.EntryURL == "" {
			entry.EntryURL, _ = example["target_url"].(string)
		}
		entry.InputExample, _ = example["params"].(map[string]any)
		if entry.InputExample == nil {
			// Current program trials save PARAMS directly, while older source
			// artifacts wrap them with url/params. Preserve both stored formats.
			entry.InputExample = example
		}
		if entry.EntryURL == "" {
			var proof struct {
				Receipts []struct {
					RequestedURL string `json:"requested_url"`
					URL          string `json:"url"`
				} `json:"receipts"`
			}
			if json.Unmarshal([]byte(version.ProofJSON), &proof) == nil && len(proof.Receipts) > 0 {
				entry.EntryURL = proof.Receipts[0].RequestedURL
				if entry.EntryURL == "" {
					entry.EntryURL = proof.Receipts[0].URL
				}
			}
		}
		entry.OutputSchema, err = recordSchema(artifact.OutputSchema)
		if err != nil {
			return out, fmt.Errorf("source %s has invalid saved output contract: %w", entry.ID, err)
		}
		catalog[entry.ID] = entry
	}
	var native []Asset
	if err := b.service.DB.WithContext(ctx).Where("active = ? AND kind IN ?", true, []string{"api", "browser"}).Order("verified_at").Find(&native).Error; err != nil {
		return out, err
	}
	for _, asset := range native {
		schema, err := recordSchema(asset.SpecJSON)
		if err != nil {
			return out, fmt.Errorf("source %s has invalid saved output contract: %w", asset.SourceID, err)
		}
		catalog[asset.SourceID] = mcpserver.Source{ID: asset.SourceID, Kind: asset.Kind, Role: "acquisition", State: "active", Description: asset.Description, EntryURL: asset.EntryURL, Version: asset.Version, CanExecute: true, OutputSchema: schema}
	}
	var discovery []DiscoveryCapability
	if err := b.service.DB.WithContext(ctx).Where("state = ?", "ready").Order("`key`").Find(&discovery).Error; err != nil {
		return out, err
	}
	for _, capability := range discovery {
		entry, found := catalog[capability.SourceID]
		if !found || entry.Version != capability.Version {
			continue
		}
		entry.Role, entry.State = "source_discovery", "ready"
		catalog[entry.ID] = entry
		// Search capabilities stay discoverable even when the topic query does
		// not appear in the generic search program's description.
		out.Discovery = append(out.Discovery, entry)
	}
	known, err := b.service.knownSources(ctx, nil)
	if err != nil {
		return out, err
	}
	for _, candidate := range known {
		// A discovered page title is user-facing metadata; adapter descriptions
		// remain technical instructions. Reuse a title only for the same URL.
		for sourceID, entry := range catalog {
			if entry.Title == "" && entry.EntryURL == candidate.URL {
				entry.Title = candidate.Title
				catalog[sourceID] = entry
			}
		}
		id := "discovered:" + candidate.ID
		catalog[id] = mcpserver.Source{ID: id, Title: candidate.Title, Kind: "discovered_url", Role: "candidate", State: candidate.State, Description: strings.TrimSpace(candidate.Title + "\nDiscovery context: " + candidate.Query), EntryURL: candidate.URL, CanExecute: false}
	}
	var heads []knowledge.Head
	if err := b.service.DB.WithContext(ctx).Where("active = ? AND source <> ?", true, "information-capability").Order("id").Find(&heads).Error; err != nil {
		return out, err
	}
	for _, head := range heads {
		entry, ok := catalog[head.Source]
		if !ok {
			entry = mcpserver.Source{ID: head.Source, Title: savedDocumentTitle(head), Kind: "saved_documents", Role: "reference", State: "saved", Description: head.Title, EntryURL: head.URL}
		}
		if entry.OutputSchema == nil && head.TaskID != 0 {
			var savedTask task.CrawlTask
			if err := b.service.DB.WithContext(ctx).Select("program_params").Where("id = ?", head.TaskID).Limit(1).Find(&savedTask).Error; err != nil {
				return out, err
			}
			var input struct {
				Spec *aiapp.TaskSpec `json:"spec"`
			}
			if json.Unmarshal([]byte(savedTask.ProgramParams), &input) == nil && input.Spec != nil {
				entry.OutputSchema, err = recordSchema(string(mustMarshal(input.Spec)))
				if err != nil {
					return out, err
				}
			}
		}
		entry.SavedDocuments++
		if head.FetchedAt != nil && (entry.LatestCaptureAt == nil || head.FetchedAt.After(*entry.LatestCaptureAt)) {
			entry.LatestCaptureAt = head.FetchedAt
		}
		catalog[head.Source] = entry
	}
	var all []mcpserver.Source
	query := strings.ToLower(strings.TrimSpace(in.Query))
	out.Counts = map[string]int{"saved": 0, "reusable": 0, "discovered": 0}
	for _, entry := range catalog {
		if query != "" && !strings.Contains(strings.ToLower(entry.ID+" "+entry.Title+" "+entry.Description+" "+entry.EntryURL), query) {
			continue
		}
		if entry.SavedDocuments > 0 {
			out.Counts["saved"]++
		}
		if entry.CanExecute {
			out.Counts["reusable"]++
		}
		if entry.Role == "candidate" {
			out.Counts["discovered"]++
		}
		if in.View == "saved" && entry.SavedDocuments == 0 || in.View == "reusable" && !entry.CanExecute || in.View == "discovered" && entry.Role != "candidate" {
			continue
		}
		all = append(all, entry)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	out.Total = len(all)
	start := sort.Search(len(all), func(i int) bool { return all[i].ID > in.Cursor })
	end := start + min(in.PageSize, len(all)-start)
	out.Sources = append(out.Sources, all[start:end]...)
	if end < len(all) && end > start {
		out.NextCursor = all[end-1].ID
	}
	return out, nil
}
