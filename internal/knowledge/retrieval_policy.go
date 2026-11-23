package knowledge

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"go-lua-crawler/internal/agentdata"
)

// Scope restricts the candidate universe before ranking. The SQL head remains
// authoritative even when a vector of a withdrawn generation still exists.
type RetrievalScope struct {
	Sources     []string `json:"sources,omitempty"`
	URLHosts    []string `json:"url_hosts,omitempty"`
	DocumentIDs []string `json:"document_ids,omitempty"`
}

type RetrievalPolicy struct {
	Mode            string         `json:"mode"`
	CandidateBudget int            `json:"candidate_budget"`
	RRFK            int            `json:"rrf_k"`
	Scope           RetrievalScope `json:"scope"`
}

func NormalizeRetrievalPolicy(p RetrievalPolicy) (RetrievalPolicy, error) {
	if p.Mode == "" {
		p.Mode = "dense"
	}
	if p.CandidateBudget == 0 {
		p.CandidateBudget = 60
	}
	if p.RRFK == 0 {
		p.RRFK = 60
	}
	if p.Mode != "dense" && p.Mode != "bm25" && p.Mode != "hybrid" {
		return p, fmt.Errorf("unknown retrieval mode %q", p.Mode)
	}
	if p.CandidateBudget < 1 || p.RRFK < 1 {
		return p, fmt.Errorf("candidate budget and RRF rank constant must be positive")
	}
	return p, nil
}

func includes(values []string, value string) bool {
	if len(values) == 0 {
		return true
	}
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}

func (s RetrievalScope) accepts(h Head) bool {
	if !includes(s.Sources, h.Source) || !includes(s.DocumentIDs, h.ID) {
		return false
	}
	if len(s.URLHosts) == 0 {
		return true
	}
	u, err := url.Parse(h.URL)
	if err != nil || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	for _, allowed := range s.URLHosts {
		allowed = strings.ToLower(strings.TrimSuffix(allowed, "."))
		if host == allowed || strings.HasSuffix(host, "."+allowed) {
			return true
		}
	}
	return false
}

type GenerationSearcher interface {
	SearchGenerations(context.Context, []float32, int, []string) ([]VectorHit, error)
}

func (r Retriever) RetrieveWithPolicy(ctx context.Context, query string, limit int, policy RetrievalPolicy) (out EvidenceSet, cause error) {
	out.Entries = []Evidence{}
	started := time.Now()
	defer func() { out.Metrics.TotalMS = time.Since(started).Milliseconds() }()
	if limit < 1 || limit > 10 {
		return out, fmt.Errorf("retrieval limit must be between 1 and 10")
	}
	p, err := NormalizeRetrievalPolicy(policy)
	if err != nil {
		return out, err
	}
	if p.CandidateBudget < limit || (p.Mode == "hybrid" && p.CandidateBudget < 2) {
		return out, fmt.Errorf("candidate budget cannot be smaller than the final result limit")
	}
	out.Metrics.Mode, out.Metrics.CandidateBudget, out.Metrics.RRFK = p.Mode, p.CandidateBudget, p.RRFK
	var vector []float32
	if p.Mode != "bm25" {
		if r.Embed == nil || r.Vectors == nil {
			return out, fmt.Errorf("dense retrieval requires embedding and vector providers")
		}
		out.Metrics.EmbeddingCalls = 1
		stage := time.Now()
		var rows [][]float32
		if measured, ok := r.Embed.(MeasuredEmbedder); ok {
			var response EmbeddingMeasurement
			response, err = measured.EmbedMeasured(ctx, []string{query})
			rows, out.Metrics.Usage, out.Metrics.Model = response.Vectors, response.Usage, response.Model
		} else {
			rows, err = r.Embed.EmbedTexts(ctx, []string{query})
		}
		out.Metrics.EmbeddingMS = time.Since(stage).Milliseconds()
		if err != nil {
			return out, err
		}
		if len(rows) != 1 || len(rows[0]) == 0 {
			return out, fmt.Errorf("query embedding count/dimension mismatch")
		}
		vector = rows[0]
	}
	if r.Store == nil || r.Store.DB == nil {
		return out, fmt.Errorf("retrieval requires a current SQL knowledge store")
	}
	var current []Head
	if err := r.Store.DB.WithContext(ctx).Where("active = ? AND generation <> ?", true, "").Order("id").Find(&current).Error; err != nil {
		return out, err
	}
	heads := map[string]Head{}
	var generations []string
	var identity []any
	for _, h := range current {
		if p.Scope.accepts(h) {
			heads[h.ID] = h
			generations = append(generations, h.Generation)
			identity = append(identity, []string{h.ID, h.Revision, h.Generation, h.Source, h.URL})
		}
	}
	out.Metrics.EligibleHeads = len(heads)
	out.Metrics.ScopeManifest = agentdata.Hash(identity)
	if len(heads) == 0 {
		out.RequestID = agentdata.Token()
		return out, nil
	}
	denseLimit, lexicalLimit := p.CandidateBudget, p.CandidateBudget
	if p.Mode == "hybrid" {
		denseLimit = p.CandidateBudget / 2
		lexicalLimit = p.CandidateBudget - denseLimit
	}
	var dense, lexical []VectorHit
	if p.Mode != "bm25" {
		stage := time.Now()
		if filtered, ok := r.Vectors.(GenerationSearcher); ok {
			dense, err = filtered.SearchGenerations(ctx, vector, denseLimit, generations)
			out.Metrics.ScopeFilter = "generation_before_vector_ranking"
		} else {
			// Legacy adapters remain bounded and SQL-validated. Their inability to
			// prefilter is exposed, never silently claimed as a filtered query.
			dense, err = r.Vectors.Search(ctx, vector, denseLimit)
			out.Metrics.ScopeFilter = "legacy_bounded_then_sql"
		}
		out.Metrics.VectorMS = time.Since(stage).Milliseconds()
		if err != nil {
			return out, err
		}
		if len(dense) > denseLimit {
			return out, fmt.Errorf("vector provider exceeded requested candidate budget")
		}
	}
	if p.Mode != "dense" {
		stage := time.Now()
		index, e := r.lexicalIndex(ctx, generations, out.Metrics.ScopeManifest)
		if e != nil {
			return out, e
		}
		lexical = index.search(query, lexicalLimit)
		out.Metrics.LexicalMS = time.Since(stage).Milliseconds()
		if p.Mode == "bm25" {
			out.Metrics.ScopeFilter = "current_sql_before_lexical_ranking"
		}
	}
	out.Metrics.DenseCandidates, out.Metrics.LexicalCandidates = len(dense), len(lexical)
	scores, methods := map[string]float64{}, map[string][]string{}
	for _, lane := range []struct {
		name string
		hits []VectorHit
	}{{"dense", dense}, {"bm25", lexical}} {
		seen := map[string]bool{}
		for rank, hit := range lane.hits {
			if seen[hit.ID] {
				continue
			}
			seen[hit.ID] = true
			methods[hit.ID] = append(methods[hit.ID], lane.name)
			if p.Mode == "hybrid" {
				scores[hit.ID] += 1 / float64(p.RRFK+rank+1)
			} else {
				scores[hit.ID] = hit.Score
			}
		}
	}
	out.Metrics.UniqueCandidates = len(scores)
	ids := make([]string, 0, len(scores))
	for id := range scores {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if scores[ids[i]] == scores[ids[j]] {
			return ids[i] < ids[j]
		}
		return scores[ids[i]] > scores[ids[j]]
	})
	chunks, err := r.Store.CurrentChunks(ctx, ids)
	if err != nil {
		return out, err
	}
	byID := map[string]Chunk{}
	for _, c := range chunks {
		if h, ok := heads[c.DocID]; ok && h.Generation == c.Generation && h.Revision == c.Revision {
			byID[c.ID] = c
		}
	}
	var ordered []Chunk
	for _, id := range ids {
		if c, ok := byID[id]; ok {
			ordered = append(ordered, c)
			if len(ordered) == limit {
				break
			}
		}
	}
	set := NewEvidenceSet(ordered, heads)
	for i := range set.Entries {
		set.Entries[i].RetrievalMethods = methods[set.Entries[i].ChunkID]
	}
	out.RequestID, out.Entries = set.RequestID, set.Entries
	return out, nil
}
