package information

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/knowledge"
)

func sortStoredReferences(refs []StoredReference) {
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Field != refs[j].Field {
			return refs[i].Field < refs[j].Field
		}
		if refs[i].DocID != refs[j].DocID {
			return refs[i].DocID < refs[j].DocID
		}
		return refs[i].Revision < refs[j].Revision
	})
}

func ptrBool(v bool) *bool { return &v }

func acquisitionResponseKey(index int) string { return fmt.Sprintf("response:%d", index) }

// acquisitionEnvelope binds fields to actual captured responses. A JSON copy of
// the output row is not substituted for the source that produced it.
func acquisitionEnvelope(receiptID uint64, rows []aiapp.Record, sources []acquisition.Response, saved ...map[int]knowledge.Document) ([]AcquiredResponse, []AcquiredRecord, error) {
	var savedByIndex map[int]knowledge.Document
	if len(saved) > 0 {
		savedByIndex = saved[0]
	}
	responses := []AcquiredResponse{}
	validIndexes := []int{}
	byURL := map[string][]int{}
	byCapture := map[int]int{}
	digests := map[string]string{}
	for i, source := range sources {
		if source.Receipt.FetchedAt.IsZero() {
			continue
		}
		isSuccess := source.Receipt.Status >= 200 && source.Receipt.Status < 300
		isFailure := source.Receipt.Status >= 400 && source.Receipt.URL != ""
		if !isSuccess && !isFailure {
			continue
		}
		key := acquisitionResponseKey(i)
		doc, reused := savedByIndex[i]
		if !reused {
			body := source.Body
			if isFailure && strings.TrimSpace(body) == "" {
				body = fmt.Sprintf("HTTP %d capturing %s", source.Receipt.Status, source.Receipt.URL)
			} else if trimmed := strings.TrimSpace(body); trimmed != "" && strings.Contains(trimmed, "<") && !strings.HasPrefix(trimmed, "{") && !strings.HasPrefix(trimmed, "[") {
				if cleaned, cleanErr := acquisition.CleanText(trimmed); cleanErr == nil && strings.TrimSpace(cleaned) != "" {
					body = cleaned
				}
			}
			title := source.Receipt.URL
			if parsed, err := url.Parse(source.Receipt.URL); err == nil && parsed.Hostname() != "" {
				title = parsed.Hostname()
			}
			var err error
			doc, err = knowledge.NewDocument(knowledge.Input{Source: "information-response", SourceKey: fmt.Sprintf("receipt:%d:%s", receiptID, key), URL: source.Receipt.URL, Title: title, Body: body, RawBody: source.Body, Format: "text", FetchedAt: source.Receipt.FetchedAt})
			if err != nil {
				return nil, nil, err
			}
		}
		_, requiresBrowser := browserSourceDiagnosis(source)
		bodyValid := isSuccess && !requiresBrowser && strings.TrimSpace(doc.Body) != ""
		idx := len(responses)
		responses = append(responses, AcquiredResponse{Key: key, Document: doc, HTTPStatus: source.Receipt.Status, BodyValid: ptrBool(bodyValid), Blocked: requiresBrowser, RequestProfile: source.Receipt.RequestProfile, SnapshotKey: source.Receipt.SnapshotKey, SnapshotHeaders: string(mustMarshal(source.Receipt.DiagnosticHeaders())), reused: reused})
		if isSuccess && !requiresBrowser {
			byCapture[i+1] = idx
			byURL[doc.URL] = append(byURL[doc.URL], idx)
			digests[doc.ID] = agentdata.Hash(source.Body)
			validIndexes = append(validIndexes, idx)
		}
	}
	records := make([]AcquiredRecord, 0, len(rows))
	for i, row := range rows {
		indexes := byURL[row.Provenance.URL]
		if row.Provenance.ResponseIndex > 0 {
			if idx, found := byCapture[row.Provenance.ResponseIndex]; found && responses[idx].Document.URL == row.Provenance.URL {
				indexes = []int{idx}
			} else {
				indexes = nil
			}
		}
		if len(indexes) == 0 {
			err := businessError("ACQUISITION_SOURCE_MISSING", "acquisition", "实际执行记录没有对应的已保存原始响应", "从原执行回执恢复 source_evidence；不要重新请求网站或用输出记录冒充原文")
			err.Details = map[string]any{"record_position": i, "url": row.Provenance.URL}
			return nil, nil, err
		}
		primary := responses[indexes[0]]
		for _, idx := range indexes {
			if responses[idx].Document.FetchedAt.Equal(row.Provenance.FetchedAt) {
				primary = responses[idx]
				break
			}
		}
		refs := []StoredReference{{DocID: primary.Document.ID, Revision: primary.Document.Revision}}
		for field, support := range row.Provenance.FieldSupport {
			if _, exact := support.ActualLocator.(string); exact && row.Provenance.ResponseIndex > 0 {
				refs = append(refs, StoredReference{DocID: primary.Document.ID, Revision: primary.Document.Revision, Field: field})
				continue
			}
			for _, idx := range validIndexes {
				response := responses[idx]
				matched := support.SourceDigest != "" && support.SourceDigest == digests[response.Document.ID]
				for _, digest := range support.SourceDigests {
					matched = matched || digest == digests[response.Document.ID]
				}
				for _, ref := range support.EvidenceRefs {
					matched = matched || strings.HasPrefix(ref, response.Document.URL+"#")
				}
				if matched {
					refs = append(refs, StoredReference{DocID: response.Document.ID, Revision: response.Document.Revision, Field: field})
				}
			}
		}
		sortStoredReferences(refs)
		records = append(records, AcquiredRecord{Position: i, ResponseKey: primary.Key, Fields: row.Fields, References: refs, FetchedAt: row.Provenance.FetchedAt})
	}
	return responses, records, nil
}

func (s *Service) ingestAcquiredRecords(ctx context.Context, requestID string, receiptID uint64, rows []aiapp.Record, sources []acquisition.Response, saved ...map[int]knowledge.Document) ([]Observation, error) {
	responses, records, err := acquisitionEnvelope(receiptID, rows, sources, saved...)
	if err != nil {
		return nil, err
	}
	return s.IngestAcquisition(ctx, requestID, receiptID, responses, records)
}
