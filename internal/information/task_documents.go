package information

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"unicode"

	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/knowledge"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type DocumentQuery struct {
	Offset      int `json:"offset"`
	Limit       int `json:"limit"`
	documentIDs []string
	sourceIDs   []string
}
type TaskDocumentResult struct {
	Reference   StoredReference `json:"reference"`
	ReceiptID   uint64          `json:"receipt_id"`
	ResponseKey string          `json:"response_key"`
	URL         string          `json:"url,omitempty"`
	Status      string          `json:"status,omitempty"`
	HTTPStatus  int             `json:"http_status,omitempty"`
	BodyValid   bool            `json:"body_valid"`
	RecordCount int             `json:"record_count"`
}
type TaskDocumentPage struct {
	Documents         []TaskDocumentResult `json:"documents"`
	Total             int64                `json:"total"`
	NextOffset        *int                 `json:"next_offset,omitempty"`
	CollectionVersion string               `json:"collection_version"`
}

func (s *Service) QueryTaskDocuments(ctx context.Context, id string, q DocumentQuery) (TaskDocumentPage, error) {
	page := TaskDocumentPage{Documents: []TaskDocumentResult{}}
	if q.Offset < 0 || q.Limit <= 0 {
		return page, businessError("INVALID_DOCUMENT_PAGE", "facts", "offset 需非负且 limit 需为正整数", "从 offset=0 开始并指定 limit，按 next_offset 翻页")
	}
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var r Request
		if err := tx.First(&r, "id = ?", id).Error; err != nil {
			return err
		}
		scoped := *s
		scoped.DB = tx
		rows, err := scoped.taskDocuments(ctx, id, q)
		if err != nil {
			return err
		}
		page.Total, page.CollectionVersion = int64(len(rows)), agentdata.Hash(rows)
		start := min(q.Offset, len(rows))
		end := start + min(q.Limit, len(rows)-start)
		for _, row := range rows[start:end] {
			page.Documents = append(page.Documents, TaskDocumentResult{
				Reference:   StoredReference{DocID: row.DocID, Revision: row.Revision},
				ReceiptID:   row.ReceiptID,
				ResponseKey: row.ResponseKey,
				URL:         row.SourceURL,
				Status:      row.Status,
				HTTPStatus:  row.HTTPStatus,
				BodyValid:   row.BodyValid,
				RecordCount: row.RecordCount,
			})
		}
		next := end
		if int64(next) < page.Total {
			page.NextOffset = &next
		}
		return nil
	})
	return page, err
}

func (s *Service) taskDocuments(ctx context.Context, id string, filter DocumentQuery) ([]taskDocument, error) {
	rows := []taskDocument{}
	query := s.DB.WithContext(ctx).Where("request_id = ?", id)
	if len(filter.documentIDs) > 0 {
		query = query.Where("doc_id IN ?", filter.documentIDs)
	}
	if err := query.Order("receipt_id, response_key").Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(filter.sourceIDs) == 0 {
		return rows, nil
	}
	wanted := map[string]bool{}
	for _, sourceID := range filter.sourceIDs {
		wanted[sourceID] = true
	}
	matches := rows[:0]
	for _, row := range rows {
		if u, err := sourceIdentity(row.SourceURL); err == nil && wanted[agentdata.Hash(u)] {
			matches = append(matches, row)
		}
	}
	return matches, nil
}

type excerptMatchEntry struct {
	Window         deliveryEvidence `json:"-"`
	EvidenceID     string           `json:"evidence_id"`
	Numbers        []string         `json:"numbers"`
	DocID          string           `json:"doc_id"`
	Revision       string           `json:"revision"`
	URL            string           `json:"url"`
	MatchedQueries []string         `json:"matched_queries"`
	Excerpt        string           `json:"excerpt"`
}

func collectExcerptQueries(single string, batch []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(raw string) {
		q := strings.Join(strings.Fields(strings.TrimSpace(raw)), " ")
		if q == "" {
			return
		}
		lk := strings.ToLower(q)
		if seen[lk] {
			return
		}
		seen[lk] = true
		out = append(out, q)
	}
	add(single)
	for _, item := range batch {
		add(item)
	}
	return out
}

const excerptBoundarySearchRunes = 30

func snapRuneWindow(textRunes []rune, startIdx, endIdx, contextRadius int) [2]int {
	start := startIdx - contextRadius
	if start < 0 {
		start = 0
	} else {
		for k := start; k > max(0, start-excerptBoundarySearchRunes); k-- {
			if textRunes[k-1] == '\n' || textRunes[k-1] == ' ' {
				start = k
				break
			}
		}
	}
	end := endIdx + contextRadius
	if end > len(textRunes) {
		end = len(textRunes)
	} else {
		for k := end; k < min(len(textRunes), end+excerptBoundarySearchRunes); k++ {
			if textRunes[k] == '\n' || textRunes[k] == ' ' {
				end = k
				break
			}
		}
	}
	return [2]int{start, end}
}

func matchQueryRuneWindows(textRunes, lowerText []rune, query string, contextRadius int, maxHits int) [][2]int {
	queryRunes := []rune(query)
	if len(queryRunes) == 0 || len(textRunes) < len(queryRunes) || maxHits <= 0 {
		return nil
	}
	lowerQuery := make([]rune, len(queryRunes))
	for i, r := range queryRunes {
		lowerQuery[i] = unicode.ToLower(r)
	}
	var windows [][2]int
	n := len(lowerQuery)
	limit := len(lowerText) - n
	for i := 0; i <= limit; {
		matched := true
		for j := 0; j < n; j++ {
			if lowerText[i+j] != lowerQuery[j] {
				matched = false
				break
			}
		}
		if !matched {
			i++
			continue
		}
		windows = append(windows, snapRuneWindow(textRunes, i, i+n, contextRadius))
		if len(windows) >= maxHits {
			break
		}
		i += n + max(1, contextRadius/2)
	}
	return windows
}

func isASCIIExcerptText(text string) bool {
	for i := 0; i < len(text); i++ {
		if text[i] >= 0x80 {
			return false
		}
	}
	return true
}

func matchASCIIExcerptWindows(text, lowerText, query string, contextRadius, maxHits int) [][2]int {
	query = strings.ToLower(query)
	if query == "" {
		return nil
	}
	var windows [][2]int
	for from := 0; from < len(text) && len(windows) < maxHits; {
		offset := strings.Index(lowerText[from:], query)
		if offset < 0 {
			break
		}
		start := from + offset
		end := start + len(query)
		// Only decode the portion that can affect the snapped boundaries.
		left := max(0, start-contextRadius-excerptBoundarySearchRunes)
		right := min(len(text), end+contextRadius+excerptBoundarySearchRunes)
		window := snapRuneWindow([]rune(text[left:right]), start-left, end-left, contextRadius)
		windows = append(windows, [2]int{left + window[0], left + window[1]})
		from = end + max(1, contextRadius/2)
	}
	return windows
}

// ASCII byte positions already are rune positions. Use substring search and
// retain Unicode folding and rune offsets for all other documents.
func findCaseInsensitiveRuneWindowsBatch(text string, queries []string, contextRadius int, maxHits int) ([][2]int, []string) {
	if text == "" || len(queries) == 0 || maxHits <= 0 {
		return nil, nil
	}
	ascii := isASCIIExcerptText(text)
	var lowerASCII string
	var textRunes, lowerRunes []rune
	if ascii {
		lowerASCII = strings.ToLower(text)
	} else {
		textRunes = []rune(text)
		lowerRunes = make([]rune, len(textRunes))
		for i, r := range textRunes {
			lowerRunes[i] = unicode.ToLower(r)
		}
	}
	var windows [][2]int
	var hitQueries []string
	for _, qry := range queries {
		var ws [][2]int
		if ascii {
			ws = matchASCIIExcerptWindows(text, lowerASCII, qry, contextRadius, maxHits)
		} else {
			ws = matchQueryRuneWindows(textRunes, lowerRunes, qry, contextRadius, maxHits)
		}
		if len(ws) > 0 {
			hitQueries = append(hitQueries, qry)
			windows = append(windows, ws...)
		}
	}
	return mergeRuneWindows(windows), hitQueries
}

func mergeRuneWindows(windows [][2]int) [][2]int {
	if len(windows) <= 1 {
		return windows
	}
	sort.Slice(windows, func(i, j int) bool {
		if windows[i][0] != windows[j][0] {
			return windows[i][0] < windows[j][0]
		}
		return windows[i][1] < windows[j][1]
	})
	merged := [][2]int{windows[0]}
	for _, w := range windows[1:] {
		last := &merged[len(merged)-1]
		if w[0] <= last[1]+20 {
			if w[1] > last[1] {
				last[1] = w[1]
			}
		} else {
			merged = append(merged, w)
		}
	}
	return merged
}

// QueryTaskDocumentExcerpts scans valid captured task snapshots locally for exact
// verbatim excerpts matching the caller's queries, eliminating multi-turn text grep scripts.
func (s *Service) QueryTaskDocumentExcerpts(ctx context.Context, id string, scope []string, q MaterialReadQuery) (map[string]any, error) {
	queries := collectExcerptQueries(q.Query, q.Queries)
	if len(queries) == 0 {
		return nil, businessError("EXCERPT_QUERY_REQUIRED", "facts", "kind='excerpts' 需要至少一个非空查询词 (query 或 queries)", "传入要检索的关键词数组，例如 queries=['Free tier','GET','Class B']")
	}
	var rows []taskDocument
	var input Input
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var r Request
		if err := tx.First(&r, "id = ?", id).Error; err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(r.InputJSON), &input); err != nil {
			return err
		}
		scoped := *s
		scoped.DB = tx
		var qErr error
		rows, qErr = scoped.taskDocuments(ctx, id, q.documentQuery())
		return qErr
	})
	if err != nil {
		return nil, err
	}
	type docVer struct{ docID, revision string }
	seenDocs := map[docVer]bool{}
	allowedVersions := make(map[docVer]bool, len(q.exactReferences))
	for _, ref := range q.exactReferences {
		allowedVersions[docVer{ref.DocID, ref.Revision}] = true
	}
	seenSource := map[[2]string]bool{}
	validDocs := make([]taskDocument, 0, len(rows))
	for i := len(rows) - 1; i >= 0; i-- {
		row := rows[i]
		if !isValidDocumentForCompleted(row) {
			continue
		}
		key := docVer{row.DocID, row.Revision}
		if len(allowedVersions) > 0 && !allowedVersions[key] {
			continue
		}
		source := [2]string{row.SourceURL, row.RequestProfile}
		latestOnly := len(q.DocumentIDs) == 0 && row.SourceURL != ""
		if seenDocs[key] || latestOnly && seenSource[source] {
			continue
		}
		seenDocs[key] = true
		seenSource[source] = true
		validDocs = append(validDocs, row)
	}

	const maxExcerptBytes = 8192
	const contextRadiusRunes = 140
	store := knowledge.NewStore(s.DB)
	allMatchedQueries := map[string]bool{}
	var byDoc [][]excerptMatchEntry
	for _, row := range validDocs {
		doc, resErr := store.ResolveDocument(ctx, row.DocID, row.Revision, knowledge.RetrievalScope{URLHosts: scope})
		if resErr != nil {
			continue
		}
		var docCandidates []excerptMatchEntry
		bodyWindows, bodyHits := findCaseInsensitiveRuneWindowsBatch(doc.Body, queries, contextRadiusRunes, 2)
		bodyHitSet := map[string]bool{}
		for _, h := range bodyHits {
			bodyHitSet[h] = true
			allMatchedQueries[h] = true
		}
		emitWindows := func(sourceText string, windows [][2]int, hits []string, raw bool) {
			if len(windows) == 0 {
				return
			}
			var runes []rune
			if !isASCIIExcerptText(sourceText) {
				runes = []rune(sourceText)
			}
			for _, w := range windows {
				var excerpt string
				if runes == nil {
					excerpt = sourceText[w[0]:w[1]]
				} else {
					excerpt = string(runes[w[0]:w[1]])
				}
				window := newDeliveryEvidence(id, doc, raw, w[0], w[1])
				var matched []string
				for _, hit := range hits {
					if strings.Contains(strings.ToLower(excerpt), strings.ToLower(hit)) {
						matched = append(matched, hit)
					}
				}
				docCandidates = append(docCandidates, excerptMatchEntry{Window: window, EvidenceID: window.ID, Numbers: extractDeliveryNumbers(excerpt), DocID: doc.ID, Revision: doc.Revision, URL: doc.URL, MatchedQueries: matched, Excerpt: excerpt})
			}
		}
		emitWindows(doc.Body, bodyWindows, bodyHits, false)
		if doc.RawBody != "" && doc.RawBody != doc.Body {
			var remaining []string
			for _, qry := range queries {
				if !bodyHitSet[qry] {
					remaining = append(remaining, qry)
				}
			}
			rawWindows, rawHits := findCaseInsensitiveRuneWindowsBatch(doc.RawBody, remaining, contextRadiusRunes, 2)
			for _, h := range rawHits {
				allMatchedQueries[h] = true
			}
			emitWindows(doc.RawBody, rawWindows, rawHits, true)
		}
		if len(docCandidates) > 0 {
			byDoc = append(byDoc, docCandidates)
		}
	}

	// Round-robin across matched documents so one large document does not starve others.
	var candidates []excerptMatchEntry
	maxDocLen := 0
	for _, list := range byDoc {
		if len(list) > maxDocLen {
			maxDocLen = len(list)
		}
	}
	for round := 0; round < maxDocLen; round++ {
		for _, list := range byDoc {
			if round < len(list) {
				candidates = append(candidates, list[round])
			}
		}
	}

	packed, returnedQueries := packExcerptMatches(candidates, maxExcerptBytes)
	seenRefs := map[docVer]bool{}
	refs := []StoredReference{}
	for _, cand := range packed {
		k := docVer{cand.DocID, cand.Revision}
		if !seenRefs[k] {
			seenRefs[k] = true
			refs = append(refs, StoredReference{DocID: cand.DocID, Revision: cand.Revision})
		}
	}
	if len(packed) > 0 {
		windows := make([]deliveryEvidence, 0, len(packed))
		for _, entry := range packed {
			windows = append(windows, entry.Window)
		}
		if err := s.DB.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&windows).Error; err != nil {
			return nil, err
		}
	}
	var matchedList, missingList, deferredList []string
	for _, qry := range queries {
		if returnedQueries[qry] {
			matchedList = append(matchedList, qry)
		} else if allMatchedQueries[qry] {
			deferredList = append(deferredList, qry)
		} else {
			missingList = append(missingList, qry)
		}
	}
	out := map[string]any{
		"mode":               "excerpts",
		"searched_documents": len(validDocs),
		"matches":            packed,
		"references":         refs,
	}
	if quantities := parsedPromptQuantities(input.Goal); len(quantities) > 0 {
		out["prompt_quantities"] = quantities
	}
	if len(matchedList) > 0 {
		out["matched_queries"] = matchedList
	}
	if len(missingList) > 0 {
		out["missing_queries"] = missingList
	}
	if len(deferredList) > 0 {
		out["deferred_queries"] = deferredList
	}
	if len(packed) < len(candidates) {
		out["truncated"] = true
		out["hint"] = "Only matches[] supplies evidence handles. Requery deferred_queries for terms found but not returned. Use missing_queries to choose different terms or sources."
	}
	return out, nil
}

// First cover missing queries. Input order breaks ties across documents.
func packExcerptMatches(candidates []excerptMatchEntry, byteBudget int) ([]excerptMatchEntry, map[string]bool) {
	packed := make([]excerptMatchEntry, 0, len(candidates))
	covered := map[string]bool{}
	sizes := make([]int, len(candidates))
	for i, candidate := range candidates {
		raw, err := json.Marshal(candidate)
		if err == nil {
			sizes[i] = len(raw) + 1
		}
	}
	remaining := byteBudget - 2
	for range candidates {
		best, gain := -1, -1
		for i, candidate := range candidates {
			if sizes[i] == 0 || (len(packed) > 0 && sizes[i] > remaining) {
				continue
			}
			newQueries := 0
			for _, query := range candidate.MatchedQueries {
				if !covered[query] {
					newQueries++
				}
			}
			if newQueries > gain || (newQueries == gain && newQueries > 0 && sizes[i] < sizes[best]) {
				best, gain = i, newQueries
			}
		}
		if best < 0 {
			break
		}
		remaining -= sizes[best]
		sizes[best] = 0
		packed = append(packed, candidates[best])
		for _, query := range candidates[best].MatchedQueries {
			covered[query] = true
		}
	}
	return packed, covered
}
