package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"gorelay/internal/contract"
	"gorelay/internal/recorddiff"
)

var (
	reDangerousBlocks = regexp.MustCompile(`(?is)<(script|style|iframe|object|embed|noscript)[^>]*>.*?</\s*(script|style|iframe|object|embed|noscript)\s*>`)
	reHTMLTags        = regexp.MustCompile(`<[^>]+>`)
)

// SanitizeText removes executable/script markup and control characters as basic
// hygiene against indirect prompt injection before storing or returning text.
func SanitizeText(s string) string {
	if s == "" {
		return ""
	}
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "")
	}
	cleaned := reDangerousBlocks.ReplaceAllString(s, " ")
	cleaned = reHTMLTags.ReplaceAllString(cleaned, " ")
	var b strings.Builder
	b.Grow(len(cleaned))
	for _, r := range cleaned {
		if isHiddenInjectionRune(r) {
			continue
		}
		if r == '\n' || r == '\t' || (r >= 0x20 && r != 0x7f) {
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func isHiddenInjectionRune(r rune) bool {
	return (r >= 0x200B && r <= 0x200F) ||
		(r >= 0x202A && r <= 0x202E) ||
		(r >= 0x2060 && r <= 0x206F) ||
		r == 0xFEFF
}

type ProjectedRow struct {
	EntityKey   string
	ContentHash string
	SourceURL   string
	Payload     map[string]any
	PayloadJSON string
}

// SanitizeAndProject filters a raw row down to declared output_schema fields,
// sanitizes strings, bounds field lengths, and computes deterministic entity/content hashes.
func SanitizeAndProject(row map[string]any, schema contract.OutputSchema) ProjectedRow {
	maxChars := schema.MaxTextLen
	if maxChars <= 0 {
		maxChars = DefaultMaxFieldChars
	}
	projected := make(map[string]any, len(schema.Fields)+1)
	for _, f := range schema.Fields {
		val, ok := row[f.Name]
		if !ok {
			continue
		}
		projected[f.Name] = sanitizeValue(val, maxChars)
	}

	var srcURL string
	if raw, ok := row["source_url"].(string); ok && strings.TrimSpace(raw) != "" {
		srcURL = strings.TrimSpace(raw)
		projected["source_url"] = srcURL
	} else if raw, ok := row["_source_url"].(string); ok && strings.TrimSpace(raw) != "" {
		srcURL = strings.TrimSpace(raw)
		projected["source_url"] = srcURL
	}

	entityKey := computeEntityKey(projected, row, schema.EntityKeys, srcURL)
	contentHash := contract.HashJSON(projected)
	rawJSON, _ := json.Marshal(projected)

	return ProjectedRow{
		EntityKey:   entityKey,
		ContentHash: contentHash,
		SourceURL:   srcURL,
		Payload:     projected,
		PayloadJSON: string(rawJSON),
	}
}

func sanitizeValue(val any, maxChars int) any {
	switch v := val.(type) {
	case string:
		cleaned := SanitizeText(v)
		runes := []rune(cleaned)
		if len(runes) > maxChars {
			return string(runes[:maxChars]) + "...(truncated)"
		}
		return cleaned
	case []string:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if c := sanitizeValue(item, maxChars).(string); c != "" {
				out = append(out, c)
			}
		}
		return out
	case []any:
		out := make([]any, 0, len(v))
		for _, item := range v {
			out = append(out, sanitizeValue(item, maxChars))
		}
		return out
	default:
		return v
	}
}

func computeEntityKey(projected, raw map[string]any, keyFields []string, srcURL string) string {
	if len(keyFields) > 0 {
		if key, err := recorddiff.BusinessKey(projected, keyFields); err == nil {
			if len(key) <= 180 {
				return key
			}
			sum := sha256.Sum256([]byte(key))
			return "bk:" + hex.EncodeToString(sum[:16])
		}
		if key, err := recorddiff.BusinessKey(raw, keyFields); err == nil {
			if len(key) <= 180 {
				return key
			}
			sum := sha256.Sum256([]byte(key))
			return "bk:" + hex.EncodeToString(sum[:16])
		}
	}
	if srcURL != "" {
		sum := sha256.Sum256([]byte(srcURL + "|" + contract.HashJSON(projected)))
		return "url:" + hex.EncodeToString(sum[:20])
	}
	sum := sha256.Sum256([]byte(contract.HashJSON(projected)))
	return "row:" + hex.EncodeToString(sum[:20])
}

// JobURLSet provides TTL and size-bounded URL cycle prevention per multi-page job.
type JobURLSet struct {
	mu      sync.Mutex
	ttl     time.Duration
	maxSize int
	seen    map[string]time.Time
}

func NewJobURLSet(ttl time.Duration, maxSize int) *JobURLSet {
	if ttl <= 0 {
		ttl = time.Hour
	}
	if maxSize <= 0 {
		maxSize = 10000
	}
	return &JobURLSet{ttl: ttl, maxSize: maxSize, seen: make(map[string]time.Time)}
}

// MarkSeen returns true if url was NOT yet seen (and records it), false if cyclic.
func (s *JobURLSet) MarkSeen(rawURL string) bool {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if exp, ok := s.seen[rawURL]; ok && now.Before(exp) {
		return false
	}
	if len(s.seen) >= s.maxSize {
		for k, exp := range s.seen {
			if !now.Before(exp) {
				delete(s.seen, k)
			}
		}
		if len(s.seen) >= s.maxSize {
			clear(s.seen)
		}
	}
	s.seen[rawURL] = now.Add(s.ttl)
	return true
}
