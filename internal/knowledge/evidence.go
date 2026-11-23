package knowledge

import (
	"context"
	"errors"
	"fmt"
	"go-lua-crawler/internal/agentdata"
	"time"
	"unicode/utf8"
)

var ErrInvalidReference = errors.New("evidence is unknown, foreign or no longer current")

type Evidence struct {
	Kind             string     `json:"kind,omitempty"`
	ID               string     `json:"id"`
	DocID            string     `json:"doc_id"`
	Revision         string     `json:"revision"`
	Generation       string     `json:"generation"`
	ChunkID          string     `json:"chunk_id"`
	Start            int        `json:"start"`
	End              int        `json:"end"`
	Title            string     `json:"title"`
	URL              string     `json:"url"`
	Text             string     `json:"text"`
	SourceID         string     `json:"source_id"`
	DataVersion      string     `json:"data_version,omitempty"`
	FetchedAt        *time.Time `json:"fetched_at,omitempty"`
	PublishedAt      *time.Time `json:"published_at,omitempty"`
	TaskID           uint64     `json:"task_id,omitempty"`
	RecordKey        string     `json:"record_key,omitempty"`
	RetrievalMethods []string   `json:"retrieval_methods,omitempty"`
}
type EvidenceSet struct {
	RequestID string           `json:"request_id"`
	Entries   []Evidence       `json:"entries"`
	Metrics   RetrievalMetrics `json:"retrieval_metrics"`
}

type RetrievalMetrics struct {
	EmbeddingCalls    int             `json:"embedding_calls"`
	Usage             *EmbeddingUsage `json:"usage"`
	Model             string          `json:"model,omitempty"`
	EmbeddingMS       int64           `json:"embedding_ms"`
	VectorMS          int64           `json:"vector_ms"`
	TotalMS           int64           `json:"total_ms"`
	Mode              string          `json:"mode,omitempty"`
	CandidateBudget   int             `json:"candidate_budget,omitempty"`
	EligibleHeads     int             `json:"eligible_heads"`
	ScopeManifest     string          `json:"scope_manifest,omitempty"`
	ScopeFilter       string          `json:"scope_filter,omitempty"`
	DenseCandidates   int             `json:"dense_candidates"`
	LexicalCandidates int             `json:"lexical_candidates"`
	UniqueCandidates  int             `json:"unique_candidates"`
	LexicalMS         int64           `json:"lexical_ms"`
	RRFK              int             `json:"rrf_k,omitempty"`
}
type EvidenceReader interface {
	ReadEvidence(context.Context, string) (Evidence, error)
}

func NewEvidenceSet(chunks []Chunk, heads map[string]Head) EvidenceSet {
	set := EvidenceSet{RequestID: agentdata.Token(), Entries: []Evidence{}}
	for _, c := range chunks {
		h, ok := heads[c.DocID]
		if !ok || !h.Active || h.Generation != c.Generation || h.Revision != c.Revision {
			continue
		}
		set.Entries = append(set.Entries, Evidence{ID: "ev_" + agentdata.Token(), DocID: c.DocID, Revision: c.Revision, Generation: c.Generation, ChunkID: c.ID, Start: c.Start, End: c.End, Title: h.Title, URL: h.URL, Text: c.Text, SourceID: h.Source, DataVersion: h.DataVersion, FetchedAt: h.FetchedAt, PublishedAt: h.PublishedAt, TaskID: h.TaskID, RecordKey: h.RecordKey})
	}
	return set
}

type SetReader struct {
	Store *Store
	Set   EvidenceSet
}

func matchesCurrentHead(e Evidence, head Head) bool {
	if e.Kind == "document" {
		return head.Active && head.Revision == e.Revision && e.DocID == head.ID && e.Generation == "document:"+head.Revision
	}
	return head.Active && head.Generation == e.Generation && head.Revision == e.Revision
}

// A synchronously saved acquisition document can be cited before background
// indexing. Its identity follows source revision, independently of the index.
func DocumentEvidence(doc Document, head Head) (Evidence, error) {
	if !head.Active || doc.ID != head.ID || doc.Revision != head.Revision || doc.Body == "" {
		return Evidence{}, fmt.Errorf("%w: acquisition document is absent, revoked or superseded", ErrInvalidReference)
	}
	id := agentdata.Hash([]string{"document", doc.ID, doc.Revision, doc.Body})
	return Evidence{Kind: "document", ID: "ev_" + id, DocID: doc.ID, Revision: doc.Revision, Generation: "document:" + doc.Revision, ChunkID: id, Start: 0, End: utf8.RuneCountInString(doc.Body), Title: doc.Title, URL: doc.URL, Text: doc.Body, SourceID: doc.Source, DataVersion: doc.DataVersion, FetchedAt: head.FetchedAt, PublishedAt: doc.PublishedAt, TaskID: doc.TaskID, RecordKey: doc.RecordKey, RetrievalMethods: []string{"current_task_document"}}, nil
}

// CurrentSet projects historical, run-scoped references onto current SQL heads.
// It never creates references to new versions or rewrites the stored receipts.
func (r SetReader) CurrentSet(ctx context.Context) (EvidenceSet, error) {
	out := r.Set
	out.Entries = []Evidence{}
	ids := []string{}
	seen := map[string]bool{}
	for _, e := range r.Set.Entries {
		if !seen[e.DocID] {
			ids = append(ids, e.DocID)
			seen[e.DocID] = true
		}
	}
	if len(ids) == 0 {
		return out, ctx.Err()
	}
	var rows []Head
	if err := r.Store.DB.WithContext(ctx).Where("id IN ?", ids).Find(&rows).Error; err != nil {
		return EvidenceSet{}, err
	}
	heads := map[string]Head{}
	for _, h := range rows {
		heads[h.ID] = h
	}
	for _, e := range r.Set.Entries {
		if h, ok := heads[e.DocID]; ok && matchesCurrentHead(e, h) {
			out.Entries = append(out.Entries, e)
		}
	}
	return out, nil
}

func (r SetReader) ReadEvidence(ctx context.Context, id string) (Evidence, error) {
	if err := ctx.Err(); err != nil {
		return Evidence{}, err
	}
	for _, e := range r.Set.Entries {
		if e.ID != id {
			continue
		}
		head, err := r.Store.Head(ctx, e.DocID)
		if err != nil {
			return Evidence{}, err
		}
		if !matchesCurrentHead(e, head) {
			return Evidence{}, fmt.Errorf("%w: citation %q references revoked/superseded document %s revision %s; current revision=%s active=%t", ErrInvalidReference, id, e.DocID, e.Revision, head.Revision, head.Active)
		}
		if e.Kind == "document" {
			doc, err := r.Store.Document(ctx, e.DocID, e.Revision)
			if err != nil {
				return Evidence{}, err
			}
			expected, err := DocumentEvidence(doc, head)
			if err != nil {
				return Evidence{}, err
			}
			if e.ID != expected.ID || e.Text != expected.Text || e.URL != expected.URL {
				return Evidence{}, fmt.Errorf("%w: citation %q differs from its saved source document", ErrInvalidReference, id)
			}
		}
		return e, nil
	}
	return Evidence{}, fmt.Errorf("%w: citation %q is not in this task's loaded evidence; copy an actual evidence ID", ErrInvalidReference, id)
}
func CheckReferences(ctx context.Context, reader SetReader, ids []string) error {
	for i, id := range ids {
		if _, err := reader.ReadEvidence(ctx, id); err != nil {
			return fmt.Errorf("citations[%d]: %w", i, err)
		}
	}
	return nil
}

type Retriever struct {
	Store   *Store
	Embed   Embedder
	Vectors VectorStore
	Policy  RetrievalPolicy
	Lexical *LexicalCache
}

func (r Retriever) Retrieve(ctx context.Context, query string, limit int) (out EvidenceSet, cause error) {
	return r.RetrieveWithPolicy(ctx, query, limit, r.Policy)
}
