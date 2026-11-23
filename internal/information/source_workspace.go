package information

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/knowledge"
	"go-lua-crawler/internal/mcpserver"
	"go-lua-crawler/internal/recorddiff"
	"go-lua-crawler/internal/repository"
)

type sourceJobPage struct {
	Jobs       []mcpserver.SourceJob `json:"jobs"`
	Total      int                   `json:"total_jobs"`
	NextCursor string                `json:"next_cursor,omitempty"`
}

func (b sourceBackend) listSourceJobs(ctx context.Context, source, cursor string, size int) (out sourceJobPage, cause error) {
	out.Jobs = []mcpserver.SourceJob{}
	if _, err := b.store(); err != nil {
		return out, err
	}
	if size < 0 {
		return out, fmt.Errorf("page_size must be positive or omitted")
	}
	if size == 0 {
		size = 20
	}
	db := b.service.DB.WithContext(ctx)
	query := db.Model(&agentdata.Run{}).Where("request_key LIKE ?", "mcp-collect:%")
	if source != "" {
		links := db.Model(&agentdata.RunTask{}).Select("run_id").Where("source_id = ?", source)
		query = query.Where("id IN (?)", links)
	}
	var runs []agentdata.Run
	if err := query.Select("id, created_at").Find(&runs).Error; err != nil {
		return out, err
	}
	type entry struct {
		id string
		at time.Time
	}
	entries := make([]entry, 0, len(runs))
	for _, run := range runs {
		entries = append(entries, entry{run.ID, run.CreatedAt})
	}
	var native []Request
	q := db.Model(&Request{}).Where("purpose = ?", nativeSourcePurpose)
	if source != "" {
		q = q.Where("JSON_UNQUOTE(JSON_EXTRACT(native_job_json, '$.source_id')) = ?", source)
	}
	if err := q.Select("id, created_at").Find(&native).Error; err != nil {
		return out, err
	}
	for _, job := range native {
		entries = append(entries, entry{nativeJobPrefix + job.ID, job.CreatedAt})
	}
	// Include acquisitions created from the natural-language task page. Exclude
	// physical tasks already represented by an MCP job to avoid double entries.
	covered := map[uint64]bool{}
	var links []agentdata.RunTask
	if len(runs) > 0 {
		ids := []string{}
		for _, run := range runs {
			ids = append(ids, run.ID)
		}
		if err := db.Where("run_id IN ?", ids).Find(&links).Error; err != nil {
			return out, err
		}
		for _, link := range links {
			covered[link.TaskID] = true
		}
	}
	var saved []repository.CrawlResult
	results := db.Model(&repository.CrawlResult{}).Where("JSON_LENGTH(JSON_EXTRACT(payload, '$.records')) > 0")
	if source != "" {
		results = results.Where("JSON_CONTAINS(JSON_EXTRACT(payload, '$.records[*].provenance.source_id'), JSON_QUOTE(?))", source)
	}
	if err := results.Select("task_id, created_at").Find(&saved).Error; err != nil {
		return out, err
	}
	for _, result := range saved {
		if !covered[result.TaskID] {
			entries = append(entries, entry{fmt.Sprintf("%s%d", savedJobPrefix, result.TaskID), result.CreatedAt})
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].at.Equal(entries[j].at) {
			return entries[i].id > entries[j].id
		}
		return entries[i].at.After(entries[j].at)
	})
	out.Total = len(entries)
	start := 0
	if cursor != "" {
		for start < len(entries) && entries[start].id != cursor {
			start++
		}
		if start == len(entries) {
			return out, fmt.Errorf("job cursor no longer exists; reload the source history")
		}
		start++
	}
	end := start + min(size, len(entries)-start)
	for _, entry := range entries[start:end] {
		job, err := b.GetJob(ctx, mcpserver.JobInput{JobID: entry.id})
		if err != nil {
			return out, err
		}
		if err := b.decorateSourceJob(ctx, &job); err != nil {
			return out, err
		}
		out.Jobs = append(out.Jobs, job)
	}
	if end < len(entries) && end > start {
		out.NextCursor = entries[end-1].id
	}
	return out, nil
}

type savedSourceDocument struct {
	ID          string     `json:"doc_id"`
	Title       string     `json:"title"`
	URL         string     `json:"url"`
	SnapshotURL string     `json:"snapshot_url"`
	Revision    string     `json:"revision"`
	FetchedAt   *time.Time `json:"fetched_at,omitempty"`
}

// Older projections use BindRecords' [source, task, business values...] key
// when the source did not supply a title. Only unwrap that known fallback;
// never expose storage identity as a document heading.
func savedDocumentTitle(head knowledge.Head) string {
	if head.Title != "" && head.Title != head.RecordKey {
		if parsed, err := url.Parse(head.Title); err != nil || parsed.Host == "" {
			return head.Title
		}
	}
	if head.RecordKey != "" && head.Title == head.RecordKey {
		var values []any
		if json.Unmarshal([]byte(head.RecordKey), &values) == nil && len(values) > 2 && values[0] == head.Source {
			var names []string
			for _, value := range values[2:] {
				if name, ok := value.(string); ok && strings.TrimSpace(name) != "" {
					if parsed, err := url.Parse(name); err == nil && parsed.Host != "" {
						continue
					}
					names = append(names, name)
				}
			}
			if len(names) > 0 {
				return strings.Join(names, " · ")
			}
		}
	}
	if parsed, err := url.Parse(head.URL); err == nil && parsed.Hostname() != "" {
		return parsed.Hostname() + " 的保存资料"
	}
	return "保存的资料"
}

func (b sourceBackend) sourceDocuments(ctx context.Context, in mcpserver.DataInput) (map[string]any, error) {
	if _, err := b.store(); err != nil {
		return nil, err
	}
	if in.PageSize < 0 {
		return nil, fmt.Errorf("page_size must be positive or omitted")
	}
	if in.PageSize == 0 {
		in.PageSize = 20
	}
	q := b.service.DB.WithContext(ctx).Model(&knowledge.Head{}).Where("active = ? AND source <> ?", true, "information-capability")
	if in.SourceID != "" {
		q = q.Where("source = ?", in.SourceID)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, err
	}
	var heads []knowledge.Head
	if err := q.Where("id > ?", in.Cursor).Order("id").Limit(in.PageSize + 1).Find(&heads).Error; err != nil {
		return nil, err
	}
	next := ""
	if len(heads) > in.PageSize {
		heads = heads[:in.PageSize]
		next = heads[len(heads)-1].ID
	}
	docs := []savedSourceDocument{}
	for _, head := range heads {
		docs = append(docs, savedSourceDocument{ID: head.ID, Title: savedDocumentTitle(head), URL: head.URL, Revision: head.Revision, FetchedAt: head.FetchedAt, SnapshotURL: "/evidence#id=" + url.QueryEscape(head.ID) + "&revision=" + url.QueryEscape(head.Revision)})
	}
	return map[string]any{"documents": docs, "total_documents": total, "next_cursor": next}, nil
}

type sourceDifference struct {
	Before  mcpserver.SourceJob         `json:"before"`
	After   mcpserver.SourceJob         `json:"after"`
	Changes []recorddiff.BusinessChange `json:"changes"`
	Notice  string                      `json:"notice,omitempty"`
}

func (b sourceBackend) sourceDifference(ctx context.Context, beforeID, afterID string) (out sourceDifference, cause error) {
	var err error
	out.Changes = []recorddiff.BusinessChange{}
	out.Before, err = b.GetJob(ctx, mcpserver.JobInput{JobID: beforeID})
	if err != nil {
		return out, err
	}
	out.After, err = b.GetJob(ctx, mcpserver.JobInput{JobID: afterID})
	if err != nil {
		return out, err
	}
	if out.Before.SourceID != out.After.SourceID {
		return out, fmt.Errorf("select two acquisitions of the same source")
	}
	complete := func(job mcpserver.SourceJob) bool { return job.Status == "succeeded" || job.Status == "completed" }
	if !complete(out.Before) || !complete(out.After) {
		out.Notice = "获取尚未完整成功；已有结果仍可查看，完成后再比较增删变化。"
		return out, nil
	}
	params := func(value map[string]any) string {
		if value == nil {
			value = map[string]any{}
		}
		return string(mustMarshal(value))
	}
	if out.Before.URL != out.After.URL || params(out.Before.Params) != params(out.After.Params) {
		out.Notice = "这两次获取的地址或参数不同。可以分别查看结果，不能直接当作同一范围的增删变化。"
		return out, nil
	}
	if len(out.Before.JoinKeys) == 0 || string(mustMarshal(out.Before.JoinKeys)) != string(mustMarshal(out.After.JoinKeys)) {
		out.Notice = "两次获取缺少一致的记录关联字段，请先对齐字段；原始结果仍可分别查看。"
		return out, nil
	}
	read := func(id string) ([]map[string]any, error) {
		rows := []map[string]any{}
		in := mcpserver.DataInput{JobID: id, PageSize: 100}
		for {
			page, err := b.ReadData(ctx, in)
			if err != nil {
				return nil, err
			}
			for _, record := range page.Records {
				fields := map[string]any{}
				for key, value := range record.Fields {
					fields[key] = value
				}
				rows = append(rows, fields)
			}
			if page.NextCursor == "" {
				return rows, nil
			}
			in.Cursor = page.NextCursor
		}
	}
	before, err := read(beforeID)
	if err != nil {
		return out, err
	}
	after, err := read(afterID)
	if err != nil {
		return out, err
	}
	out.Changes, err = recorddiff.Diff(before, after, out.Before.JoinKeys)
	if err != nil {
		out.Changes = []recorddiff.BusinessChange{}
		out.Notice = "记录标识需要对齐：" + err.Error()
	}
	return out, nil
}
