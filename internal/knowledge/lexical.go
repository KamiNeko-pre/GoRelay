package knowledge

import (
	"math"
	"sort"
	"strings"
	"unicode"
)

const LexicalTokenizerVersion = "unicode-identifier-components-v1"

// Evidence represents an attributed factual excerpt backed by an archived snapshot.
type Evidence struct {
	ID          string `json:"id"`
	DocID       string `json:"doc_id"`
	Title       string `json:"title"`
	URL         string `json:"url"`
	Text        string `json:"text"`
	SourceID    string `json:"source_id"`
	SnapshotRef string `json:"snapshot_ref,omitempty"`
}

type EvidenceSnapshot struct {
	Evidence
	Current      bool `json:"current"`
	SourceActive bool `json:"source_active"`
}

type LexicalDocument struct {
	ID   string
	Text string
}

type LexicalHit struct {
	ID    string
	Score float64
}

type indexedDoc struct {
	id     string
	terms  map[string]int
	length int
}

// LexicalIndex is an immutable in-memory BM25 index over archived text passages.
type LexicalIndex struct {
	docs    []indexedDoc
	df      map[string]int
	average float64
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

func BuildLexicalIndex(docs []LexicalDocument) *LexicalIndex {
	index := &LexicalIndex{df: map[string]int{}}
	for _, input := range docs {
		terms := LexicalTerms(input.Text)
		doc := indexedDoc{id: input.ID, terms: map[string]int{}, length: len(terms)}
		for _, term := range terms {
			doc.terms[term]++
		}
		for term := range doc.terms {
			index.df[term]++
		}
		index.average += float64(doc.length)
		index.docs = append(index.docs, doc)
	}
	if len(index.docs) > 0 {
		index.average /= float64(len(index.docs))
	}
	return index
}

func (index *LexicalIndex) Search(query string, limit int) []LexicalHit {
	if index == nil || index.average == 0 || limit <= 0 {
		return nil
	}
	queryTerms := map[string]bool{}
	for _, term := range LexicalTerms(query) {
		queryTerms[term] = true
	}
	var hits []LexicalHit
	const k1, b = 1.2, 0.75
	for _, doc := range index.docs {
		score := 0.0
		for term := range queryTerms {
			tf := float64(doc.terms[term])
			if tf == 0 {
				continue
			}
			df, n := float64(index.df[term]), float64(len(index.docs))
			idf := math.Log(1 + (n-df+0.5)/(df+0.5))
			score += idf * tf * (k1 + 1) / (tf + k1*(1-b+b*float64(doc.length)/index.average))
		}
		if score > 0 {
			hits = append(hits, LexicalHit{ID: doc.id, Score: score})
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score == hits[j].Score {
			return hits[i].ID < hits[j].ID
		}
		return hits[i].Score > hits[j].Score
	})
	if len(hits) > limit {
		return hits[:limit]
	}
	return hits
}
