package pipeline

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gorelay/internal/acquisition"
	"gorelay/internal/knowledge"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var reTitleTag = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)

type Store struct {
	DB          *gorm.DB
	SnapshotDir string
}

func NewStore(db *gorm.DB, snapshotDir string) *Store {
	if strings.TrimSpace(snapshotDir) == "" {
		snapshotDir = filepath.Join("data", "snapshots")
	}
	return &Store{DB: db, SnapshotDir: snapshotDir}
}

func Migrate(db *gorm.DB) error {
	return db.AutoMigrate(&SourceRecord{}, &SnapshotBlob{})
}

// ArchiveSnapshot compresses raw HTTP bytes with gzip under content-addressed storage
// and stores bounded metadata + excerpt in snapshot_blobs.
func ArchiveSnapshot(ctx context.Context, tx *gorm.DB, snapshotDir, sourceID string, jobID uint64, resp acquisition.Response) (string, error) {
	if resp.Body == "" {
		return "", nil
	}
	if strings.TrimSpace(snapshotDir) == "" {
		snapshotDir = filepath.Join("data", "snapshots")
	}
	sum := sha256.Sum256([]byte(resp.Body))
	ref := hex.EncodeToString(sum[:])

	shardDir := filepath.Join(snapshotDir, ref[:2])
	if err := os.MkdirAll(shardDir, 0o755); err != nil {
		return "", fmt.Errorf("create snapshot dir: %w", err)
	}
	gzPath := filepath.Join(shardDir, ref+".gz")
	if _, err := os.Stat(gzPath); errors.Is(err, os.ErrNotExist) {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		if _, err := zw.Write([]byte(resp.Body)); err != nil {
			_ = zw.Close()
			return "", err
		}
		if err := zw.Close(); err != nil {
			return "", err
		}
		tmp := gzPath + ".tmp"
		if err := os.WriteFile(tmp, buf.Bytes(), 0o600); err != nil {
			return "", err
		}
		if err := os.Rename(tmp, gzPath); err != nil {
			_ = os.Remove(tmp)
			return "", err
		}
	}

	if tx == nil {
		return ref, nil
	}
	title := extractTitle(resp.Body)
	excerpt := SanitizeText(resp.Body)
	if len(excerpt) > DefaultSnapshotExcerptSize {
		excerpt = excerpt[:DefaultSnapshotExcerptSize]
	}
	fetched := resp.Receipt.FetchedAt
	if fetched.IsZero() {
		fetched = time.Now().UTC()
	}
	blob := SnapshotBlob{
		SnapshotRef:  ref,
		SourceID:     sourceID,
		JobID:        jobID,
		SourceURL:    resp.Receipt.URL,
		Title:        title,
		ContentType:  resp.Receipt.Headers["content-type"],
		GzipPath:     filepath.ToSlash(gzPath),
		PlainExcerpt: excerpt,
		ByteSize:     int64(len(resp.Body)),
		FetchedAt:    fetched,
	}
	if err := tx.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&blob).Error; err != nil {
		return "", err
	}
	return ref, nil
}

// IngestBatchTx validates, sanitizes, deduplicates, and version-updates records atomically.
func (s *Store) IngestBatchTx(ctx context.Context, tx *gorm.DB, in IngestInput) (IngestResult, error) {
	out := IngestResult{InputRows: len(in.Records)}
	if drift := CheckSilentDrift(in.Records, in.OutputSchema, in.Evidence, in.AllowEmptyBatch && !in.RequireNonEmpty); drift != nil {
		out.Drift = drift
		return out, fmt.Errorf("%w: %s", ErrSilentDrift, drift.Reason)
	}
	if tx == nil {
		tx = s.DB
	}

	dir := in.SnapshotDir
	if dir == "" {
		dir = s.SnapshotDir
	}

	urlToRef := make(map[string]string, len(in.Evidence))
	var primaryRef string
	for _, ev := range in.Evidence {
		ref, err := ArchiveSnapshot(ctx, tx, dir, in.SourceID, in.JobID, ev)
		if err != nil {
			return out, err
		}
		if ref != "" {
			if primaryRef == "" {
				primaryRef = ref
			}
			if ev.Receipt.URL != "" {
				urlToRef[ev.Receipt.URL] = ref
			}
			out.SnapshotRefs = append(out.SnapshotRefs, ref)
		}
	}

	now := time.Now().UTC()
	currentTrue := true
	seenKeys := make(map[string]string, len(in.Records))

	for _, rawRow := range in.Records {
		proj := SanitizeAndProject(rawRow, in.OutputSchema)
		if prevHash, dup := seenKeys[proj.EntityKey]; dup && prevHash == proj.ContentHash {
			out.SkippedDuplicates++
			continue
		}
		seenKeys[proj.EntityKey] = proj.ContentHash

		ref := primaryRef
		if proj.SourceURL != "" {
			if matched, ok := urlToRef[proj.SourceURL]; ok {
				ref = matched
			}
		}

		var existing SourceRecord
		findErr := tx.WithContext(ctx).
			Where("source_id = ? AND entity_key = ? AND is_current = ?", in.SourceID, proj.EntityKey, true).
			First(&existing).Error
		if findErr == nil {
			if existing.ContentHash == proj.ContentHash {
				out.SkippedDuplicates++
				continue
			}
			if err := tx.WithContext(ctx).Model(&SourceRecord{}).Where("id = ?", existing.ID).Update("is_current", nil).Error; err != nil {
				return out, err
			}
		} else if !errors.Is(findErr, gorm.ErrRecordNotFound) {
			return out, findErr
		}

		rec := SourceRecord{
			SourceID:         in.SourceID,
			EntityKey:        proj.EntityKey,
			IsCurrent:        &currentTrue,
			ConnectorVersion: in.ConnectorVersion,
			JobID:            in.JobID,
			ContentHash:      proj.ContentHash,
			PayloadJSON:      proj.PayloadJSON,
			SnapshotRef:      ref,
			SourceURL:        proj.SourceURL,
			FetchedAt:        now,
		}
		if err := tx.WithContext(ctx).Create(&rec).Error; err != nil {
			return out, err
		}
		out.SavedRows++
	}
	return out, nil
}

// PackRecordsToBudget packs serialized records under maxBytes/maxRows limits for MCP responses.
func PackRecordsToBudget(rows []SourceRecord, maxBytes, pageSize int, baseURL string) DeliveredPage {
	if maxBytes <= 0 {
		maxBytes = DefaultMCPResponseMaxBytes
	}
	if pageSize <= 0 {
		pageSize = DefaultMCPPageSize
	}
	page := DeliveredPage{Records: []map[string]any{}}
	seenRefs := make(map[string]bool)
	bytesUsed := 256 // reserve header metadata budget

	for i, rec := range rows {
		if i >= pageSize {
			if i > 0 {
				page.NextCursor = strconv.FormatUint(rows[i-1].ID, 10)
			}
			break
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(rec.PayloadJSON), &obj); err != nil {
			obj = map[string]any{"raw": rec.PayloadJSON}
		}
		if rec.SnapshotRef != "" {
			obj["_snapshot_ref"] = rec.SnapshotRef
		}
		rowBytes, _ := json.Marshal(obj)
		if bytesUsed+len(rowBytes) > maxBytes && len(page.Records) > 0 {
			page.TruncatedToBudget = true
			page.NextCursor = strconv.FormatUint(rows[i-1].ID, 10)
			break
		}
		bytesUsed += len(rowBytes) + 2
		page.Records = append(page.Records, obj)
		if rec.SnapshotRef != "" && !seenRefs[rec.SnapshotRef] {
			seenRefs[rec.SnapshotRef] = true
			page.SnapshotURLs = append(page.SnapshotURLs, formatSnapshotURL(baseURL, rec.SnapshotRef))
		}
	}
	page.Returned = len(page.Records)
	return page
}

func (s *Store) DeliverRecordsPage(ctx context.Context, sourceID string, jobID uint64, cursor string, maxBytes, pageSize int, baseURL string) (DeliveredPage, error) {
	if pageSize <= 0 || pageSize > DefaultMCPPageSize {
		pageSize = DefaultMCPPageSize
	}
	q := s.DB.WithContext(ctx).Model(&SourceRecord{}).Where("is_current = ?", true)
	if sourceID != "" {
		q = q.Where("source_id = ?", sourceID)
	}
	if jobID > 0 {
		q = q.Where("job_id = ?", jobID)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return DeliveredPage{}, err
	}
	if cursor != "" {
		afterID, err := strconv.ParseUint(cursor, 10, 64)
		if err != nil {
			return DeliveredPage{}, fmt.Errorf("invalid cursor %q", cursor)
		}
		q = q.Where("id > ?", afterID)
	}
	var rows []SourceRecord
	if err := q.Order("id ASC").Limit(pageSize + 1).Find(&rows).Error; err != nil {
		return DeliveredPage{}, err
	}
	page := PackRecordsToBudget(rows, maxBytes, pageSize, baseURL)
	page.SourceID = sourceID
	page.JobID = jobID
	page.Total = total
	if page.NextCursor == "" && len(rows) > page.Returned && page.Returned > 0 {
		page.NextCursor = strconv.FormatUint(rows[page.Returned-1].ID, 10)
	}
	if total > int64(page.Returned) {
		page.Summary = fmt.Sprintf("Returned %d of %d records (truncated to MCP budget); pass next_cursor for more.", page.Returned, total)
	}
	return page, nil
}

// SearchEvidence runs scoped BM25 retrieval over snapshot_blobs or pages an archived snapshot by ref.
func (s *Store) SearchEvidence(ctx context.Context, sourceIDs []string, query, snapshotRef, cursor string, pageSize int, baseURL string) (EvidencePage, error) {
	if pageSize <= 0 || pageSize > 20 {
		pageSize = 10
	}
	if snapshotRef != "" {
		var blob SnapshotBlob
		if err := s.DB.WithContext(ctx).First(&blob, "snapshot_ref = ?", snapshotRef).Error; err != nil {
			return EvidencePage{}, err
		}
		full, err := readSnapshotText(blob)
		if err != nil {
			return EvidencePage{}, err
		}
		return sliceSnapshotText(blob, full, cursor, 4000, baseURL), nil
	}
	if len(sourceIDs) == 0 {
		return EvidencePage{}, fmt.Errorf("source_ids or snapshot_ref is required for deterministic evidence search")
	}
	var blobs []SnapshotBlob
	if err := s.DB.WithContext(ctx).Where("source_id IN ?", sourceIDs).Order("fetched_at DESC").Limit(500).Find(&blobs).Error; err != nil {
		return EvidencePage{}, err
	}
	return RankEvidenceBlobs(blobs, query, cursor, pageSize, baseURL), nil
}

func RankEvidenceBlobs(blobs []SnapshotBlob, query, cursor string, pageSize int, baseURL string) EvidencePage {
	if len(blobs) == 0 {
		return EvidencePage{Entries: []EvidenceHit{}}
	}
	docs := make([]knowledge.LexicalDocument, 0, len(blobs))
	byRef := make(map[string]SnapshotBlob, len(blobs))
	for _, b := range blobs {
		byRef[b.SnapshotRef] = b
		docs = append(docs, knowledge.LexicalDocument{ID: b.SnapshotRef, Text: b.Title + "\n" + b.PlainExcerpt})
	}
	var ordered []EvidenceHit
	if strings.TrimSpace(query) == "" {
		for _, b := range blobs {
			ordered = append(ordered, EvidenceHit{
				SnapshotRef: b.SnapshotRef, SourceID: b.SourceID, SourceURL: b.SourceURL, Title: b.Title,
				Snippet: centeredSnippet(b.PlainExcerpt, "", 320), SnapshotURL: formatSnapshotURL(baseURL, b.SnapshotRef), FetchedAt: b.FetchedAt,
			})
		}
	} else {
		hits := knowledge.BuildLexicalIndex(docs).Search(query, len(docs))
		for _, h := range hits {
			b := byRef[h.ID]
			ordered = append(ordered, EvidenceHit{
				SnapshotRef: b.SnapshotRef, SourceID: b.SourceID, SourceURL: b.SourceURL, Title: b.Title,
				Snippet: centeredSnippet(b.PlainExcerpt, query, 320), SnapshotURL: formatSnapshotURL(baseURL, b.SnapshotRef),
				FetchedAt: b.FetchedAt, Score: h.Score,
			})
		}
	}
	offset := 0
	if cursor != "" {
		if parsed, err := strconv.Atoi(cursor); err == nil && parsed >= 0 {
			offset = parsed
		}
	}
	if offset >= len(ordered) {
		return EvidencePage{Entries: []EvidenceHit{}, Total: len(ordered)}
	}
	end := min(len(ordered), offset+pageSize)
	page := EvidencePage{Entries: ordered[offset:end], Total: len(ordered)}
	if end < len(ordered) {
		page.NextCursor = strconv.Itoa(end)
	}
	return page
}

func readSnapshotText(blob SnapshotBlob) (string, error) {
	if blob.GzipPath != "" {
		f, err := os.Open(filepath.FromSlash(blob.GzipPath))
		if err == nil {
			defer f.Close()
			zr, gzErr := gzip.NewReader(f)
			if gzErr == nil {
				defer zr.Close()
				raw, rErr := io.ReadAll(io.LimitReader(zr, 6*1024*1024))
				if rErr == nil {
					return SanitizeText(string(raw)), nil
				}
			}
		}
	}
	return blob.PlainExcerpt, nil
}

func sliceSnapshotText(blob SnapshotBlob, fullText, cursor string, chunkRunes int, baseURL string) EvidencePage {
	runes := []rune(fullText)
	offset := 0
	if cursor != "" {
		if parsed, err := strconv.Atoi(cursor); err == nil && parsed >= 0 {
			offset = parsed
		}
	}
	if offset >= len(runes) {
		return EvidencePage{Entries: []EvidenceHit{}, Total: 0}
	}
	end := min(len(runes), offset+chunkRunes)
	page := EvidencePage{
		Total: 1,
		Entries: []EvidenceHit{{
			SnapshotRef: blob.SnapshotRef,
			SourceID:    blob.SourceID,
			SourceURL:   blob.SourceURL,
			Title:       blob.Title,
			Snippet:     string(runes[offset:end]),
			SnapshotURL: formatSnapshotURL(baseURL, blob.SnapshotRef),
			FetchedAt:   blob.FetchedAt,
		}},
	}
	if end < len(runes) {
		page.NextCursor = strconv.Itoa(end)
	}
	return page
}

func centeredSnippet(text, query string, maxRunes int) string {
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return text
	}
	terms := knowledge.LexicalTerms(query)
	lower := strings.ToLower(text)
	byteIdx := 0
	for _, t := range terms {
		if pos := strings.Index(lower, t); pos > 0 {
			byteIdx = pos
			break
		}
	}
	runePos := len([]rune(text[:byteIdx]))
	start := max(0, runePos-maxRunes/3)
	end := min(len(runes), start+maxRunes)
	return string(runes[start:end])
}

func formatSnapshotURL(baseURL, ref string) string {
	if baseURL == "" {
		return "/evidence?ref=" + ref
	}
	return strings.TrimRight(baseURL, "/") + "/evidence?ref=" + ref
}

func extractTitle(raw string) string {
	head := raw
	if len(head) > 16384 {
		head = head[:16384]
	}
	m := reTitleTag.FindStringSubmatch(head)
	if len(m) < 2 {
		return ""
	}
	title := SanitizeText(m[1])
	if len(title) > 250 {
		return title[:250]
	}
	return title
}
