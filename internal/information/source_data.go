package information

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/knowledge"
	"go-lua-crawler/internal/mcpserver"
	"go-lua-crawler/internal/repository"
)

type sourceDataRow struct {
	id   string
	data mcpserver.DataRecord
}

func (b sourceBackend) ReadData(ctx context.Context, in mcpserver.DataInput) (mcpserver.DataPage, error) {
	out := mcpserver.DataPage{Records: []mcpserver.DataRecord{}}
	if _, err := b.store(); err != nil {
		return out, err
	}
	if in.SourceID == "" && in.JobID == "" {
		return out, fmt.Errorf("source_id or job_id is required; use an ID returned by the source catalog or collection")
	}
	if in.PageSize < 0 {
		return out, fmt.Errorf("page_size must be positive or omitted")
	}
	if in.PageSize == 0 {
		in.PageSize = 20
	}
	// Business rows come from immutable execution results, not knowledge heads.
	// A deferred/failed evidence projection must never hide saved acquisition.
	query := b.service.DB.WithContext(ctx).Model(&repository.CrawlResult{})
	if in.SourceID != "" {
		query = query.Where("JSON_CONTAINS(JSON_EXTRACT(payload, '$.records[*].provenance.source_id'), JSON_QUOTE(?))", in.SourceID)
	}

	if in.JobID != "" {
		ids := []uint64{}
		if strings.HasPrefix(in.JobID, savedJobPrefix) {
			job, err := b.savedSourceJob(ctx, in.JobID)
			if err != nil {
				return out, err
			}
			for _, child := range job.Tasks {
				ids = append(ids, child.TaskID)
			}
		} else if strings.HasPrefix(in.JobID, nativeJobPrefix) {
			// Native acquisitions are observations backed by host receipts, not
			// synthetic crawler tasks. They are loaded below from that authority.
		} else {
			store := agentdata.NewStore(b.service.DB)
			if _, err := store.Run(ctx, in.JobID); err != nil {
				return out, fmt.Errorf("acquisition job %q: %w", in.JobID, err)
			}
			links, err := store.Children(ctx, in.JobID)
			if err != nil {
				return out, err
			}
			for _, link := range links {
				ids = append(ids, link.TaskID)
			}
		}
		if len(ids) == 0 {
			query = query.Where("1 = 0")
		} else {
			query = query.Where("task_id IN ?", ids)
		}
	}
	var results []repository.CrawlResult
	if err := query.Order("task_id").Find(&results).Error; err != nil {
		return out, err
	}
	var taskIDs []uint64
	for _, result := range results {
		taskIDs = append(taskIDs, result.TaskID)
	}
	var versions []knowledge.DocumentVersion
	if len(taskIDs) > 0 {
		if err := b.service.DB.WithContext(ctx).Where("JSON_UNQUOTE(JSON_EXTRACT(json, '$.task_id')) IN ?", taskIDs).Order("doc_id,revision").Find(&versions).Error; err != nil {
			return out, err
		}
	}
	byTask := map[uint64][]knowledge.Document{}
	for _, version := range versions {
		var document knowledge.Document
		if err := json.Unmarshal([]byte(version.JSON), &document); err != nil {
			return out, err
		}
		byTask[document.TaskID] = append(byTask[document.TaskID], document)
	}
	var cursor struct {
		Revision string `json:"revision"`
		After    string `json:"after"`
	}
	if in.Cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(in.Cursor)
		if err != nil || json.Unmarshal(raw, &cursor) != nil {
			return out, fmt.Errorf("invalid cursor; copy next_cursor from a data page")
		}
	}
	var rows []sourceDataRow
	for _, result := range results {
		var payload aiapp.AcquisitionResult
		if err := json.Unmarshal([]byte(result.Payload), &payload); err != nil {
			return out, fmt.Errorf("read saved task %d: %w", result.TaskID, err)
		}
		for index, record := range payload.Records {
			if in.SourceID != "" && record.Provenance.SourceID != in.SourceID {
				continue
			}
			id := fmt.Sprintf("%020d:%020d", result.TaskID, index)
			refs := []mcpserver.DocumentReference{}

			for _, doc := range byTask[result.TaskID] {
				origin := "extracted_record"
				if strings.HasPrefix(doc.SourceKey, "captured-response:") {
					if !recordReferencesSource(record, doc.URL) {
						continue
					}
					origin = "source_response"
				} else if doc.RecordKey != record.Key {
					continue
				}
				var capturedAt *time.Time
				if !doc.FetchedAt.IsZero() {
					date := doc.FetchedAt
					capturedAt = &date
				}
				refs = append(refs, mcpserver.DocumentReference{ID: doc.ID, Revision: doc.Revision, URL: doc.URL, SnapshotURL: mcpserver.RequestBaseURL(ctx) + "/evidence#id=" + url.QueryEscape(doc.ID) + "&revision=" + url.QueryEscape(doc.Revision), Origin: origin, FetchedAt: capturedAt})
			}
			raw, err := json.Marshal(record.Provenance)
			if err != nil {
				return out, err
			}
			var provenance mcpserver.RecordProvenance
			if err := json.Unmarshal(raw, &provenance); err != nil {
				return out, err
			}
			rows = append(rows, sourceDataRow{id, mcpserver.DataRecord{Key: record.Key, SchemaVersion: record.SchemaVersion, Fields: record.Fields, Provenance: provenance, Documents: refs}})
		}
	}
	if in.JobID == "" || strings.HasPrefix(in.JobID, nativeJobPrefix) {
		native, err := b.nativeDataRows(ctx, in)
		if err != nil {
			return out, err
		}
		rows = append(rows, native...)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].id < rows[j].id })
	versionsForRead := make([]any, 0, len(rows))
	for _, row := range rows {
		versionsForRead = append(versionsForRead, []any{row.id, row.data.Documents})
	}
	out.DataRevision = agentdata.Hash([]any{in.SourceID, in.JobID, versionsForRead})
	if in.Cursor != "" && cursor.Revision != out.DataRevision {
		return out, fmt.Errorf("saved data changed; read again without the old cursor")
	}
	out.Total = len(rows)
	start := sort.Search(len(rows), func(i int) bool { return rows[i].id > cursor.After })
	end := start + min(in.PageSize, len(rows)-start)
	for _, entry := range rows[start:end] {
		out.Records = append(out.Records, entry.data)
	}
	if end < len(rows) && end > start {
		cursor.Revision, cursor.After = out.DataRevision, rows[end-1].id
		raw, _ := json.Marshal(cursor)
		out.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	return out, nil
}

// Project native facts into the public data contract without creating a second
// persisted result. Both partial and complete receipts retain their useful rows.
func (b sourceBackend) nativeDataRows(ctx context.Context, in mcpserver.DataInput) ([]sourceDataRow, error) {
	db := b.service.DB.WithContext(ctx)
	steps := db.Model(&Step{}).Select("id").Where("execution_kind IN ? OR (execution_kind = '' AND kind IN ?)", []string{"trial_api", "trial_browser", "execute_asset", "execute_browser"}, []string{"trial_api", "trial_browser", "execute_asset", "execute_browser"})
	if in.JobID != "" {
		requestID := strings.TrimPrefix(in.JobID, nativeJobPrefix)
		request, err := b.service.Request(ctx, requestID)
		if err != nil {
			return nil, err
		}
		if request.Purpose != nativeSourcePurpose {
			return nil, fmt.Errorf("job_id is not a native source job")
		}
		steps = steps.Where("request_id = ?", requestID)
	}
	var facts []observationRow
	if err := db.Where("receipt_id IN (?)", steps).Order("receipt_id, record_position").Find(&facts).Error; err != nil {
		return nil, err
	}
	store := knowledge.NewStore(b.service.DB)
	var rows []sourceDataRow
	var lastReceiptID uint64
	var records []mcpserver.DataRecord
	for _, fact := range facts {
		if fact.ReceiptID != lastReceiptID {
			var step Step
			if err := db.First(&step, fact.ReceiptID).Error; err != nil {
				return nil, err
			}
			var receipt struct {
				Records []mcpserver.DataRecord `json:"records"`
			}
			if err := json.Unmarshal([]byte(executionResult(step).ResultJSON), &receipt); err != nil {
				return nil, err
			}
			records, lastReceiptID = receipt.Records, fact.ReceiptID
		}
		if fact.RecordPosition < 0 || fact.RecordPosition >= len(records) {
			return nil, fmt.Errorf("observation %s has no matching record in receipt %d", fact.ID, fact.ReceiptID)
		}
		record := records[fact.RecordPosition]
		if in.SourceID != "" && record.Provenance.SourceID != in.SourceID {
			continue
		}
		observation, err := decodeObservation(fact)
		if err != nil {
			return nil, err
		}
		record.Fields, record.Documents = observation.Fields, []mcpserver.DocumentReference{}
		seen := map[string]bool{}
		for _, ref := range observation.References {
			key := ref.DocID + ":" + ref.Revision
			if seen[key] {
				continue
			}
			seen[key] = true
			doc, err := store.ResolveReference(ctx, ref, knowledge.RetrievalScope{})
			if err != nil {
				return nil, err
			}
			record.Documents = append(record.Documents, mcpserver.DocumentReference{ID: doc.ID, Revision: doc.Revision, URL: doc.URL, SnapshotURL: mcpserver.RequestBaseURL(ctx) + "/evidence#id=" + url.QueryEscape(doc.ID) + "&revision=" + url.QueryEscape(doc.Revision), Origin: "source_response", FetchedAt: &doc.FetchedAt})
		}
		rows = append(rows, sourceDataRow{fmt.Sprintf("native:%020d:%020d", fact.ReceiptID, fact.RecordPosition), record})
	}
	return rows, nil
}

func recordReferencesSource(record aiapp.Record, sourceURL string) bool {
	if record.Provenance.URL == sourceURL {
		return true
	}
	for _, support := range record.Provenance.FieldSupport {
		for _, ref := range support.EvidenceRefs {
			if ref == sourceURL || strings.HasPrefix(ref, sourceURL+"#") {
				return true
			}
		}
	}
	return false
}
