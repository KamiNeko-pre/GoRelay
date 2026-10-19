package knowledge

import (
	"context"
	"math"
	"sort"
	"strings"
	"sync"
	"unicode"
)

const LexicalTokenizerVersion = "unicode-identifier-components-v1"

type lexicalDocument struct {
	id     string
	terms  map[string]int
	length int
}
type lexicalIndex struct {
	documents []lexicalDocument
	df        map[string]int
	average   float64
}

// One immutable current-scope snapshot per retriever. A manifest change rebuilds
// the index; no stale cache entries or cross-scope results are returned.
type LexicalCache struct {
	mu       sync.Mutex
	manifest string
	index    *lexicalIndex
}

func LexicalTerms(text string) []string {
	var out []string
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && !strings.ContainsRune("._-/@", r)
	})
	for _, word := range words {
		word = strings.Trim(word, "._-/@")
		if word == "" {
			continue
		}
		out = append(out, word)
		parts := strings.FieldsFunc(word, func(r rune) bool { return strings.ContainsRune("._-/@", r) })
		if len(parts) > 1 {
			out = append(out, parts...)
		}
		var han []rune
		for _, r := range word {
			if unicode.Is(unicode.Han, r) {
				han = append(han, r)
			} else {
				han = nil
			}
			if len(han) >= 2 {
				out = append(out, string(han[len(han)-2:]))
			}
		}
	}
	return out
}

func (r Retriever) lexicalIndex(ctx context.Context, generations []string, manifest string) (*lexicalIndex, error) {
	if r.Lexical != nil {
		r.Lexical.mu.Lock()
		defer r.Lexical.mu.Unlock()
		if r.Lexical.manifest == manifest && r.Lexical.index != nil {
			return r.Lexical.index, nil
		}
	}
	var chunks []Chunk
	err := r.Store.DB.WithContext(ctx).Table("chunks c").Select("c.id,c.doc_id,c.revision,c.generation,c.number,c.`start`,c.`end`,c.text").Joins("JOIN knowledge_heads h ON h.id=c.doc_id AND h.generation=c.generation AND h.revision=c.revision").Where("h.active = ? AND c.generation IN ?", true, generations).Order("c.id").Find(&chunks).Error
	if err != nil {
		return nil, err
	}
	index := &lexicalIndex{df: map[string]int{}}
	for _, chunk := range chunks {
		terms := LexicalTerms(chunk.Text)
		doc := lexicalDocument{id: chunk.ID, terms: map[string]int{}, length: len(terms)}
		for _, term := range terms {
			doc.terms[term]++
		}
		for term := range doc.terms {
			index.df[term]++
		}
		index.average += float64(doc.length)
		index.documents = append(index.documents, doc)
	}
	if len(index.documents) > 0 {
		index.average /= float64(len(index.documents))
	}
	if r.Lexical != nil {
		r.Lexical.index, r.Lexical.manifest = index, manifest
	}
	return index, nil
}

func (index *lexicalIndex) search(query string, limit int) []VectorHit {
	if index.average == 0 {
		return nil
	}
	queryTerms := map[string]bool{}
	for _, term := range LexicalTerms(query) {
		queryTerms[term] = true
	}
	hits := []VectorHit{}
	const k1, b = 1.2, 0.75
	for _, doc := range index.documents {
		score := 0.0
		for term := range queryTerms {
			tf := float64(doc.terms[term])
			if tf == 0 {
				continue
			}
			df, n := float64(index.df[term]), float64(len(index.documents))
			idf := math.Log(1 + (n-df+0.5)/(df+0.5))
			score += idf * tf * (k1 + 1) / (tf + k1*(1-b+b*float64(doc.length)/index.average))
		}
		if score > 0 {
			hits = append(hits, VectorHit{ID: doc.id, Score: score})
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score == hits[j].Score {
			return hits[i].ID < hits[j].ID
		}
		return hits[i].Score > hits[j].Score
	})
	return hits[:min(len(hits), limit)]
}
