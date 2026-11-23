package information

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/aiapp"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type DiscoveryCapability struct {
	Kind           string     `json:"kind,omitempty" gorm:"size:16"`
	BrowserAssetID string     `json:"browser_asset_id,omitempty" gorm:"size:64"`
	QueryParameter string     `json:"query_parameter,omitempty" gorm:"size:128"`
	Key            string     `json:"key" gorm:"primaryKey;size:64"`
	Generation     uint64     `json:"generation"`
	BuildRequestID string     `json:"build_request_id" gorm:"size:32"`
	VerifyRunID    string     `json:"verify_run_id,omitempty" gorm:"size:32"`
	State          string     `json:"state" gorm:"size:32"`
	SourceID       string     `json:"source_id,omitempty" gorm:"size:64"`
	Version        string     `json:"version,omitempty" gorm:"size:64"`
	TargetURL      string     `json:"target_url,omitempty" gorm:"type:text"`
	FirstQuery     string     `json:"first_query" gorm:"type:text"`
	VerifyQuery    string     `json:"verify_query" gorm:"type:text"`
	FirstURLsJSON  string     `json:"-" gorm:"type:longtext"`
	LastError      string     `json:"last_error,omitempty" gorm:"type:text"`
	VerifiedAt     *time.Time `json:"verified_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

func (DiscoveryCapability) TableName() string { return "information_discovery_capabilities" }

type DiscoveredSource struct {
	ID                 string     `json:"id" gorm:"primaryKey;size:64"`
	URL                string     `json:"url" gorm:"type:text"`
	Title              string     `json:"title" gorm:"type:text"`
	Query              string     `json:"query,omitempty" gorm:"type:text"`
	DiscoveryRequestID string     `json:"discovery_request_id,omitempty" gorm:"size:32"`
	State              string     `json:"state" gorm:"size:32;index"`
	VerifiedAt         *time.Time `json:"verified_at,omitempty"`
	LastSeenAt         time.Time  `json:"last_seen_at"`
	CreatedAt          time.Time  `json:"created_at"`
}

func (DiscoveredSource) TableName() string { return "information_discovered_sources" }

// sourceIdentity normalizes candidate URLs, upgrading protocol-relative URLs
// and unwrapping search-engine redirect URLs (such as DuckDuckGo ?uddg=...).
func sourceIdentity(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if strings.HasPrefix(trimmed, "//") {
		trimmed = "https:" + trimmed
	}
	u, err := url.Parse(trimmed)
	if err != nil || u.User != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("candidate requires an absolute HTTP(S) URL without credentials")
	}
	if strings.Contains(strings.ToLower(u.Hostname()), "duckduckgo.com") && strings.HasPrefix(u.Path, "/l/") {
		if target := u.Query().Get("uddg"); target != "" {
			if decoded, err := url.Parse(target); err == nil && (decoded.Scheme == "http" || decoded.Scheme == "https") && decoded.Hostname() != "" {
				u = decoded
			}
		}
	}
	u.Fragment = ""
	u.Host = strings.ToLower(u.Host)
	if u.Path == "" {
		u.Path = "/"
	}
	return u.String(), nil
}

func (s *Service) rememberSources(ctx context.Context, requestID, query string, candidates []aiapp.SearchCandidate, verified bool) error {
	for _, candidate := range candidates {
		raw, err := sourceIdentity(candidate.URL)
		if err != nil {
			continue
		}
		now := time.Now()
		state := "candidate"
		var at *time.Time
		if verified {
			state = "verified"
			at = &now
		}
		row := DiscoveredSource{ID: agentdata.Hash(raw), URL: raw, Title: candidate.Title, Query: query, DiscoveryRequestID: requestID, State: state, VerifiedAt: at, LastSeenAt: now}
		updates := map[string]any{"last_seen_at": now}
		if candidate.Title != "" {
			updates["title"] = candidate.Title
		}
		if query != "" {
			updates["query"] = query
			updates["discovery_request_id"] = requestID
		}
		if verified {
			updates["state"] = state
			updates["verified_at"] = at
		}
		if err := s.DB.WithContext(ctx).Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "id"}}, DoUpdates: clause.Assignments(updates)}).Create(&row).Error; err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) knownSources(ctx context.Context, scope []string) ([]DiscoveredSource, error) {
	var rows []DiscoveredSource
	if err := s.DB.WithContext(ctx).Order("verified_at DESC, last_seen_at DESC, id").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := []DiscoveredSource{}
	for _, row := range rows {
		if urlInScope(row.URL, scope) {
			out = append(out, row)
		}
	}
	return out, nil
}

func searchCandidates(rows []aiapp.Record) ([]aiapp.SearchCandidate, error) {
	out := []aiapp.SearchCandidate{}
	seen := map[string]bool{}
	for _, row := range rows {
		raw, _ := row.Fields["url"].(string)
		title, _ := row.Fields["title"].(string)
		u, err := sourceIdentity(raw)
		if err != nil || strings.TrimSpace(title) == "" {
			continue
		}
		if seen[u] {
			continue
		}
		seen[u] = true
		out = append(out, aiapp.SearchCandidate{URL: u, Title: title, Discovery: row.Provenance.URL})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("search returned no valid candidate links")
	}
	return out, nil
}

func discoverySubtaskGoal(query string) string {
	return fmt.Sprintf("数据源发现子任务：围绕 %q 查找并核验真实可访问的官方网页或公开 API 入口（若无外部搜索接口，可用 acquire_sources / run_lua 查询公开目录或索引页并验证可达性）。最后调用 submit_delivery 提交简短报告，并在 discovered_sources 中列出核验选定的入口 URL。", query)
}

func confirmedDiscoveryReferences(tx *gorm.DB, requestID string, scope []string, draft *DeliveryDraft) ([]StoredReference, error) {
	if len(draft.DiscoveredSources) == 0 {
		if draft.Outcome == "completed" {
			return nil, businessError("DISCOVERY_SOURCES_MISSING", "delivery", "来源发现尚未提交核验过的入口", "在 discovered_sources 列出已打开的入口，或提交 no_match / partial 并写明实际情况")
		}
		return nil, nil
	}
	var documents []struct {
		Document taskDocument `gorm:"embedded"`
		URL      string
	}
	if err := tx.Table("information_task_documents d").Select("d.*, h.url").Joins("JOIN knowledge_heads h ON h.id = d.doc_id").Where("d.request_id = ? AND d.receipt_id > 0", requestID).Order("d.receipt_id, d.response_key").Find(&documents).Error; err != nil {
		return nil, err
	}
	byURL := map[string]StoredReference{}
	for _, doc := range documents {
		u, err := sourceIdentity(doc.URL)
		if err == nil && isValidDocumentForCompleted(doc.Document) {
			byURL[u] = StoredReference{DocID: doc.Document.DocID, Revision: doc.Document.Revision}
		}
	}
	selected := make([]string, 0, len(draft.DiscoveredSources))
	refs := make([]StoredReference, 0, len(draft.DiscoveredSources))
	for _, raw := range draft.DiscoveredSources {
		u, err := sourceIdentity(raw)
		ref, ok := byURL[u]
		if err != nil || !ok || !urlInScope(u, scope) {
			return nil, businessError("DISCOVERY_SOURCE_NOT_CHECKED", "delivery", "没有找到入口的有效抓取："+raw, "用 acquire_sources 打开该入口，提交成功响应的最终 URL；无法访问的候选放进 gaps")
		}
		selected = append(selected, u)
		refs = append(refs, ref)
	}
	draft.DiscoveredSources = selected
	return refs, nil
}

type discoveryChildLink struct {
	ChildRequestID string `json:"child_request_id"`
}

type verifiedDiscoverySource struct {
	URL       string           `json:"url"`
	SourceRef *StoredReference `json:"source_ref,omitempty"`
}

// Attach only the public GET snapshots the child explicitly selected and cited.
// A candidate without such a snapshot remains useful as a URL to fetch normally.
func (h *informationHost) attachDiscoverySources(ctx context.Context, step Step, childID string, draft DeliveryDraft) ([]verifiedDiscoverySource, error) {
	selected := make(map[string]*StoredReference, len(draft.DiscoveredSources))
	for _, raw := range draft.DiscoveredSources {
		if target, err := sourceIdentity(raw); err == nil && urlInScope(target, h.input.SourceScope) {
			selected[target] = nil
		}
	}
	if len(selected) == 0 {
		return nil, nil
	}
	cited := make(map[[2]string]bool, len(draft.References))
	for _, ref := range draft.References {
		cited[[2]string{ref.DocID, ref.Revision}] = true
	}
	err := h.owned(ctx, func(tx *gorm.DB) error {
		var captures []taskDocument
		if err := tx.Where("request_id = ? AND snapshot_key <> '' AND body_valid = ? AND http_status BETWEEN 200 AND 299 AND status IN ?", childID, true, []string{"succeeded", reusedResponseStatus}).Order("receipt_id DESC, response_key").Find(&captures).Error; err != nil {
			return err
		}
		var linked int64
		for _, capture := range captures {
			target, err := sourceIdentity(capture.SourceURL)
			ref, wanted := selected[target]
			if err != nil || !wanted || ref != nil || !cited[[2]string{capture.DocID, capture.Revision}] {
				continue
			}
			capture.ResponseKey = agentdata.Hash([]string{childID, capture.DocID, capture.Revision})
			capture.RequestID, capture.ReceiptID, capture.Status, capture.RecordCount = step.RequestID, step.ID, reusedResponseStatus, 0
			result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&capture)
			if result.Error != nil {
				return result.Error
			}
			linked += result.RowsAffected
			selected[target] = &StoredReference{DocID: capture.DocID, Revision: capture.Revision}
		}
		if linked == 0 {
			return nil
		}
		return tx.Model(&Request{}).Where("id = ?", step.RequestID).UpdateColumn("facts_revision", gorm.Expr("facts_revision + ?", linked)).Error
	})
	if err != nil {
		return nil, err
	}
	out := make([]verifiedDiscoverySource, 0, len(selected))
	for _, raw := range draft.DiscoveredSources {
		target, err := sourceIdentity(raw)
		if ref, ok := selected[target]; err == nil && ok {
			out = append(out, verifiedDiscoverySource{URL: target, SourceRef: ref})
			delete(selected, target)
		}
	}
	return out, nil
}

func (s *Service) discoveryChildren(ctx context.Context, requestID string) ([]Request, error) {
	var steps []Step
	if err := s.DB.WithContext(ctx).Select("execution_result_json").Where("request_id = ? AND execution_kind = ?", requestID, "source_discovery").Order("id").Find(&steps).Error; err != nil {
		return nil, err
	}
	children := make([]Request, 0, len(steps))
	for _, step := range steps {
		var link discoveryChildLink
		if err := json.Unmarshal([]byte(step.ExecutionResultJSON), &link); err != nil {
			return nil, err
		}
		child, err := s.Request(ctx, link.ChildRequestID)
		if err != nil {
			return nil, err
		}
		children = append(children, child)
	}
	return children, nil
}

func (s *Service) cancelDiscoveryWork(ctx context.Context, r Request) error {
	children, err := s.discoveryChildren(ctx, r.ID)
	if err != nil {
		return err
	}
	for _, child := range children {
		if !terminal(child.Status) {
			if err := s.Cancel(ctx, child.ID); err != nil && !agentdataErrConflict(err) {
				return err
			}
		}
	}
	return nil
}

func agentdataErrConflict(err error) bool {
	return err != nil && strings.Contains(err.Error(), agentdata.ErrConflict.Error())
}
