package knowledge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"go-lua-crawler/internal/agentdata"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrEvidenceCursorChanged = errors.New("saved evidence changed since this page was retrieved; search again without the old cursor")

type EvidenceQuery struct {
	Query    string         `json:"query"`
	Scope    RetrievalScope `json:"scope,omitempty"`
	PageSize int            `json:"page_size,omitempty"`
	Cursor   string         `json:"cursor,omitempty"`
}

type EvidenceSnapshot struct {
	Evidence
	Current      bool   `json:"current" jsonschema:"This is the latest saved revision; it does not prove the live website is unchanged or the fact is current today."`
	SourceActive bool   `json:"source_active"`
	Origin       string `json:"origin"`
}

type EvidencePage struct {
	Entries        []EvidenceSnapshot `json:"entries"`
	Total          int                `json:"total_passages"`
	CorpusRevision string             `json:"corpus_revision"`
	NextCursor     string             `json:"next_cursor,omitempty"`
}

type evidenceCursor struct {
	Revision string `json:"revision"`
	Offset   int    `json:"offset"`
}

// EvidenceRevision reads only the saved corpus identity. Tool admission uses
// it without running retrieval or writing citation passages a second time.
func (s *Store) EvidenceRevision(ctx context.Context, query EvidenceQuery) (string, error) {
	var heads []Head
	if err := s.DB.WithContext(ctx).Select("id, revision, source, url").Where("active = ? AND source <> ?", true, "information-capability").Order("id").Find(&heads).Error; err != nil {
		return "", err
	}
	identities := make([][2]string, 0, len(heads))
	for _, head := range heads {
		if query.Scope.accepts(head) {
			identities = append(identities, [2]string{head.ID, head.Revision})
		}
	}
	return savedCorpusRevision(query.Query, identities), nil
}

func savedCorpusRevision(query string, identities [][2]string) string {
	return agentdata.Hash([]any{LexicalTokenizerVersion, strings.ToLower(strings.TrimSpace(query)), identities})
}

// SearchEvidence reads captured document revisions, including those whose
// asynchronous vector generation is still pending. It never publishes vectors.
func (s *Store) SearchEvidence(ctx context.Context, query EvidenceQuery) (EvidencePage, error) {
	return s.searchEvidenceInternal(ctx, query, -1, 0)
}

// SearchEvidenceWindow retrieves lexical evidence hits starting at a concrete
// 0-based hit offset and returns at most limit entries without coupling pagination
// to an internal fixed page cursor.
func (s *Store) SearchEvidenceWindow(ctx context.Context, query EvidenceQuery, offset, limit int) (EvidencePage, error) {
	if offset < 0 {
		return EvidencePage{}, fmt.Errorf("offset must be nonnegative")
	}
	if limit <= 0 {
		limit = 20
	}
	return s.searchEvidenceInternal(ctx, query, offset, limit)
}

func (s *Store) searchEvidenceInternal(ctx context.Context, query EvidenceQuery, explicitOffset, explicitLimit int) (EvidencePage, error) {
	var rows []struct {
		ID           string
		Revision     string
		DocumentJSON string
	}
	err := s.DB.WithContext(ctx).Table("knowledge_heads h").
		Select("h.id, h.revision, v.json AS document_json").
		Joins("LEFT JOIN document_versions v ON v.doc_id = h.id AND v.revision = h.revision").
		Where("h.active = ?", true).Order("h.id").Find(&rows).Error
	if err != nil {
		return EvidencePage{}, err
	}
	docs := make([]Document, 0, len(rows))
	for _, row := range rows {
		var doc Document
		if err := json.Unmarshal([]byte(row.DocumentJSON), &doc); err != nil {
			return EvidencePage{}, fmt.Errorf("read saved document %s revision %s: %w", row.ID, row.Revision, err)
		}
		if doc.ID != row.ID || doc.Revision != row.Revision {
			return EvidencePage{}, fmt.Errorf("saved document identity differs from its head: %s revision %s", row.ID, row.Revision)
		}
		docs = append(docs, doc)
	}
	var page EvidencePage
	var chunks []Chunk
	if explicitOffset >= 0 {
		page, chunks, err = searchSavedDocumentsWindow(query, docs, explicitOffset, explicitLimit)
	} else {
		page, chunks, err = searchSavedDocuments(query, docs)
	}
	if err != nil {
		return page, err
	}
	// Persist only returned passages so their citation IDs survive a restart.
	// The generation namespace is separate from the vector indexer's manifests.
	if len(chunks) > 0 {
		err = s.DB.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).CreateInBatches(&chunks, 100).Error
	}
	return page, err
}

func searchSavedDocuments(query EvidenceQuery, documents []Document) (EvidencePage, []Chunk, error) {
	return searchSavedDocumentsWindow(query, documents, -1, 0)
}

func searchSavedDocumentsWindow(query EvidenceQuery, documents []Document, explicitOffset, explicitLimit int) (EvidencePage, []Chunk, error) {
	out := EvidencePage{Entries: []EvidenceSnapshot{}}
	if strings.TrimSpace(query.Query) == "" {
		return out, nil, fmt.Errorf("query is required; provide the fact or topic to look up")
	}
	if query.PageSize < 0 {
		return out, nil, fmt.Errorf("page_size must be positive or omitted")
	}
	if query.PageSize == 0 {
		query.PageSize = 20
	}
	docs := make([]Document, 0, len(documents))
	for _, doc := range documents {
		// This namespace stores executable capability metadata, not captured facts.
		if doc.Source == "information-capability" || !query.Scope.accepts(Head{ID: doc.ID, Source: doc.Source, URL: doc.URL}) {
			continue
		}
		docs = append(docs, doc)
	}
	sort.Slice(docs, func(i, j int) bool { return docs[i].ID < docs[j].ID })
	identities := make([][2]string, 0, len(docs))
	chunks := []Chunk{}
	byID := map[string]Chunk{}
	docByID := map[string]Document{}
	for _, doc := range docs {
		identities = append(identities, [2]string{doc.ID, doc.Revision})
		docByID[doc.ID] = doc
		generation := agentdata.Hash([]string{"saved-passages-v1", doc.ID, doc.Revision})
		for _, chunk := range SplitDocument(doc, 600, 80, generation) {
			chunks = append(chunks, chunk)
			byID[chunk.ID] = chunk
		}
	}
	out.CorpusRevision = savedCorpusRevision(query.Query, identities)
	startOffset := 0
	pageSize := query.PageSize
	if explicitOffset >= 0 {
		startOffset = explicitOffset
		if explicitLimit > 0 {
			pageSize = explicitLimit
		}
	} else {
		position := evidenceCursor{Revision: out.CorpusRevision}
		if query.Cursor != "" {
			raw, err := base64.RawURLEncoding.DecodeString(query.Cursor)
			if err != nil || json.Unmarshal(raw, &position) != nil || position.Offset < 0 {
				return out, nil, fmt.Errorf("invalid cursor; use next_cursor from the previous result page")
			}
			if position.Revision != out.CorpusRevision {
				return out, nil, ErrEvidenceCursorChanged
			}
		}
		startOffset = position.Offset
	}
	hits := buildLexicalIndex(chunks).search(query.Query, len(chunks))
	out.Total = len(hits)
	if startOffset > len(hits) {
		return out, nil, fmt.Errorf("cursor is outside the result set; use a returned next_cursor")
	}
	end := startOffset + min(pageSize, len(hits)-startOffset)
	selected := make([]Chunk, 0, end-startOffset)
	for _, hit := range hits[startOffset:end] {
		chunk := byID[hit.ID]
		selected = append(selected, chunk)
		out.Entries = append(out.Entries, snapshotEvidence(docByID[chunk.DocID], chunk, true, true))
	}
	if end < len(hits) {
		raw, _ := json.Marshal(evidenceCursor{Revision: out.CorpusRevision, Offset: end})
		out.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	return out, selected, nil
}

func snapshotEvidence(doc Document, chunk Chunk, current, active bool) EvidenceSnapshot {
	e := Evidence{Kind: "snapshot", ID: "snapshot_" + chunk.ID, DocID: doc.ID, Revision: doc.Revision, Generation: chunk.Generation, ChunkID: chunk.ID, Start: chunk.Start, End: chunk.End, Title: doc.Title, URL: doc.URL, Text: chunk.Text, SourceID: doc.Source, DataVersion: doc.DataVersion, PublishedAt: doc.PublishedAt, TaskID: doc.TaskID, RecordKey: doc.RecordKey, RetrievalMethods: []string{"bm25"}}
	if !doc.FetchedAt.IsZero() {
		e.FetchedAt = &doc.FetchedAt
	}
	origin := "saved_document"
	if strings.HasPrefix(doc.SourceKey, "captured-response:") {
		origin = "source_response"
	} else if doc.RecordKey != "" && doc.TaskID != 0 {
		origin = "extracted_record"
	}
	return EvidenceSnapshot{Evidence: e, Current: current, SourceActive: active, Origin: origin}
}

// Historical citation reads retain their exact revision; they do not change
// the current-only checks used when accepting evidence for a new answer.
func (s *Store) ReadEvidenceSnapshot(ctx context.Context, id string) (EvidenceSnapshot, error) {
	if !strings.HasPrefix(id, "snapshot_") {
		return EvidenceSnapshot{}, fmt.Errorf("%w: copy an evidence ID returned by evidence search", ErrInvalidReference)
	}
	var chunk Chunk
	if err := s.DB.WithContext(ctx).First(&chunk, "id = ?", strings.TrimPrefix(id, "snapshot_")).Error; err != nil {
		return EvidenceSnapshot{}, err
	}
	doc, err := s.Document(ctx, chunk.DocID, chunk.Revision)
	if err != nil {
		return EvidenceSnapshot{}, err
	}
	body := []rune(doc.Body)
	if chunk.Start < 0 || chunk.End < chunk.Start || chunk.End > len(body) || string(body[chunk.Start:chunk.End]) != chunk.Text {
		return EvidenceSnapshot{}, fmt.Errorf("%w: saved passage differs from its original document revision", ErrInvalidReference)
	}
	head, err := s.Head(ctx, doc.ID)
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return EvidenceSnapshot{}, err
	}
	return snapshotEvidence(doc, chunk, head.Active && head.Revision == doc.Revision, head.Active), nil
}

// SearchEvidenceDocumentWindow searches lexical passages within the exact saved
// document revision starting at a 0-based hit offset, including historical
// revisions whose head is still active.
func (s *Store) SearchEvidenceDocumentWindow(ctx context.Context, id, revision, query string, offset, limit int) (EvidencePage, Document, error) {
	if id == "" || revision == "" {
		return EvidencePage{}, Document{}, fmt.Errorf("document_id and revision are both required; copy them from evidence_documents")
	}
	if strings.TrimSpace(query) == "" {
		return EvidencePage{}, Document{}, fmt.Errorf("query is required; provide the fact or topic to look up")
	}
	if offset < 0 {
		return EvidencePage{}, Document{}, fmt.Errorf("offset must be nonnegative")
	}
	if limit <= 0 {
		limit = 20
	}
	doc, err := s.ResolveDocument(ctx, id, revision, RetrievalScope{})
	if err != nil {
		return EvidencePage{}, Document{}, err
	}
	head, err := s.Head(ctx, id)
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return EvidencePage{}, Document{}, err
	}
	page, chunks, err := searchSavedDocumentsWindow(EvidenceQuery{Query: query}, []Document{doc}, offset, limit)
	if err != nil {
		return page, doc, err
	}
	current := head.Active && head.Revision == doc.Revision
	for i := range page.Entries {
		page.Entries[i].Current = current
		page.Entries[i].SourceActive = head.Active
	}
	if len(chunks) > 0 {
		if err := s.DB.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).CreateInBatches(&chunks, 100).Error; err != nil {
			return EvidencePage{}, Document{}, err
		}
	}
	return page, doc, nil
}

// ReadEvidenceDocument pages the exact saved revision named by a data record.
// It also works for a historical capture that is no longer a searchable head.
func (s *Store) ReadEvidenceDocument(ctx context.Context, id, revision, cursor string, pageSize int) (EvidencePage, error) {
	page, _, err := s.readEvidenceDocumentInternal(ctx, id, revision, cursor, -1, pageSize)
	return page, err
}

// ReadEvidenceDocumentWindow returns a chunk window starting at a 0-based chunk
// offset alongside the resolved immutable Document so callers can enforce
// character/byte budgets without losing skipped entries.
func (s *Store) ReadEvidenceDocumentWindow(ctx context.Context, id, revision string, offset, limit int) (EvidencePage, Document, error) {
	if offset < 0 {
		return EvidencePage{}, Document{}, fmt.Errorf("offset must be nonnegative")
	}
	if limit <= 0 {
		limit = 20
	}
	return s.readEvidenceDocumentInternal(ctx, id, revision, "", offset, limit)
}

func (s *Store) readEvidenceDocumentInternal(ctx context.Context, id, revision, cursor string, explicitOffset, pageSize int) (EvidencePage, Document, error) {
	out := EvidencePage{Entries: []EvidenceSnapshot{}}
	if id == "" || revision == "" {
		return out, Document{}, fmt.Errorf("document_id and revision are both required; copy them from evidence_documents")
	}
	if pageSize < 0 {
		return out, Document{}, fmt.Errorf("page_size must be positive or omitted")
	}
	if pageSize == 0 {
		pageSize = 20
	}
	doc, err := s.ResolveDocument(ctx, id, revision, RetrievalScope{})
	if err != nil {
		return out, Document{}, err
	}
	head, err := s.Head(ctx, id)
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return out, Document{}, err
	}
	generation := agentdata.Hash([]string{"saved-passages-v1", doc.ID, doc.Revision})
	chunks := SplitDocument(doc, 600, 80, generation)
	out.Total = len(chunks)
	out.CorpusRevision = agentdata.Hash([]string{doc.ID, doc.Revision})
	startOffset := 0
	if explicitOffset >= 0 {
		if explicitOffset > len(chunks) {
			return out, doc, fmt.Errorf("chunk offset %d exceeds total chunks %d", explicitOffset, len(chunks))
		}
		startOffset = explicitOffset
	} else {
		position := evidenceCursor{Revision: out.CorpusRevision}
		if cursor != "" {
			raw, err := base64.RawURLEncoding.DecodeString(cursor)
			if err != nil || json.Unmarshal(raw, &position) != nil || position.Offset < 0 || position.Offset > len(chunks) {
				return out, Document{}, fmt.Errorf("invalid document cursor; copy next_cursor from this document revision")
			}
			if position.Revision != out.CorpusRevision {
				return out, Document{}, fmt.Errorf("cursor belongs to a different document revision")
			}
		}
		startOffset = position.Offset
	}
	end := startOffset + min(pageSize, len(chunks)-startOffset)
	selected := chunks[startOffset:end]
	if len(selected) > 0 {
		if err := s.DB.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).CreateInBatches(&selected, 100).Error; err != nil {
			return out, Document{}, err
		}
	}
	for _, chunk := range selected {
		evidence := snapshotEvidence(doc, chunk, head.Active && head.Revision == doc.Revision, head.Active)
		evidence.RetrievalMethods = []string{"saved_document"}
		out.Entries = append(out.Entries, evidence)
	}
	if explicitOffset < 0 && end < len(chunks) {
		raw, _ := json.Marshal(evidenceCursor{Revision: out.CorpusRevision, Offset: end})
		out.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	return out, doc, nil
}
