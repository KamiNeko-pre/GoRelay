package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const maxQueryScanRows = 5000

var (
	reHTTPURL      = regexp.MustCompile(`https?://[^\s<>"'` + "`" + `()\[\]]+`)
	reMarkdownLink = regexp.MustCompile(`\[([^\]\n]{1,60})\]\((https?://[^\s<>"'` + "`" + `()\[\]]+)\)`)
)

// FilterCondition defines a single deterministic predicate over a cleaned record field.
type FilterCondition struct {
	Field string `json:"field"`
	Op    string `json:"op,omitempty"` // eq | ne | gt | gte | lt | lte | contains | icontains | prefix | suffix | regex
	Value any    `json:"value"`
}

// URLExtractSpec extracts matching URLs from a text field (e.g. release notes body)
// on matched records without transferring bulky full-text bodies to the host LLM.
type URLExtractSpec struct {
	Field   string `json:"field"`
	Pattern string `json:"pattern,omitempty"` // case-insensitive substring filter on URL
	Regex   string `json:"regex,omitempty"`   // regex filter on URL
}

// RecordQuery runs deterministic pure-Go projection, filtering, sorting, grouping,
// and URL extraction over cleaned records without mutating stored MySQL rows.
type RecordQuery struct {
	Fields        []string          `json:"fields,omitempty"`
	ExcludeFields []string          `json:"exclude_fields,omitempty"`
	MaxFieldChars int               `json:"max_field_chars,omitempty"`
	Where         []FilterCondition `json:"where,omitempty"`
	OrderBy       string            `json:"order_by,omitempty"`
	Limit         int               `json:"limit,omitempty"`
	GroupBy       string            `json:"group_by,omitempty"`
	GroupPrefix   int               `json:"group_prefix,omitempty"`
	ExtractURLs   *URLExtractSpec   `json:"extract_urls,omitempty"`
}

func (q *RecordQuery) IsEmpty() bool {
	if q == nil {
		return true
	}
	return len(q.Fields) == 0 &&
		len(q.ExcludeFields) == 0 &&
		q.MaxFieldChars <= 0 &&
		len(q.Where) == 0 &&
		strings.TrimSpace(q.OrderBy) == "" &&
		q.Limit <= 0 &&
		strings.TrimSpace(q.GroupBy) == "" &&
		(q.ExtractURLs == nil || strings.TrimSpace(q.ExtractURLs.Field) == "")
}

// WantsRows reports whether the caller expects row records back.
// Pure group_by / extract_urls queries follow SQL aggregate semantics and omit row records
// unless the caller explicitly requests row fields, exclude_fields, or sort order.
func (q *RecordQuery) WantsRows() bool {
	if q == nil {
		return true
	}
	isAggOrExtract := strings.TrimSpace(q.GroupBy) != "" || (q.ExtractURLs != nil && strings.TrimSpace(q.ExtractURLs.Field) != "")
	if !isAggOrExtract {
		return true
	}
	return len(q.Fields) > 0 || len(q.ExcludeFields) > 0 || strings.TrimSpace(q.OrderBy) != ""
}

type snapRegistry struct {
	refs []string
	idx  map[string]int
}

func newSnapRegistry() *snapRegistry {
	return &snapRegistry{idx: make(map[string]int)}
}

func (r *snapRegistry) add(ref string) int {
	if ref == "" {
		return -1
	}
	if i, ok := r.idx[ref]; ok {
		return i
	}
	i := len(r.refs)
	r.idx[ref] = i
	r.refs = append(r.refs, ref)
	return i
}

// ParseRecordQuery converts a generic JSON object map into a validated RecordQuery.
func ParseRecordQuery(raw map[string]any) (*RecordQuery, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid query object: %w", err)
	}
	var q RecordQuery
	dec := json.NewDecoder(strings.NewReader(string(encoded)))
	dec.UseNumber()
	if err := dec.Decode(&q); err != nil {
		return nil, fmt.Errorf("invalid query format: %w", err)
	}
	if err := q.Validate(); err != nil {
		return nil, err
	}
	if q.IsEmpty() {
		return nil, nil
	}
	return &q, nil
}

func (q *RecordQuery) Validate() error {
	if q == nil {
		return nil
	}
	if q.Limit < 0 {
		return fmt.Errorf("query.limit must be >= 0")
	}
	if q.MaxFieldChars < 0 {
		return fmt.Errorf("query.max_field_chars must be >= 0")
	}
	if q.GroupPrefix < 0 {
		return fmt.Errorf("query.group_prefix must be >= 0")
	}
	if q.ExtractURLs != nil {
		q.ExtractURLs.Field = strings.TrimSpace(q.ExtractURLs.Field)
		if q.ExtractURLs.Regex != "" {
			if _, err := regexp.Compile(q.ExtractURLs.Regex); err != nil {
				return fmt.Errorf("query.extract_urls invalid regex: %w", err)
			}
		}
	}
	for i := range q.Where {
		c := &q.Where[i]
		c.Field = strings.TrimSpace(c.Field)
		if c.Field == "" {
			return fmt.Errorf("query.where[%d].field is required", i)
		}
		op := strings.ToLower(strings.TrimSpace(c.Op))
		if op == "" {
			op = "eq"
		}
		switch op {
		case "eq", "ne", "gt", "gte", "lt", "lte", "contains", "icontains", "prefix", "suffix":
			c.Op = op
		case "regex":
			c.Op = op
			if _, err := regexp.Compile(stringifyValue(c.Value)); err != nil {
				return fmt.Errorf("query.where[%d] invalid regex: %w", i, err)
			}
		default:
			return fmt.Errorf("query.where[%d] unsupported op %q", i, c.Op)
		}
	}
	return nil
}

type decodedRecord struct {
	row SourceRecord
	obj map[string]any
}

// DeliverRecordsPageWithQuery runs deterministic server-side query pushdown
// capped to DefaultMCPQueryMaxBytes (3 KiB) to avoid bloating LLM context.
func (s *Store) DeliverRecordsPageWithQuery(
	ctx context.Context,
	sourceID string,
	jobID uint64,
	cursor string,
	maxBytes, pageSize int,
	baseURL string,
	query *RecordQuery,
) (DeliveredPage, error) {
	if query == nil || query.IsEmpty() {
		return s.DeliverRecordsPage(ctx, sourceID, jobID, cursor, maxBytes, pageSize, baseURL)
	}
	if err := query.Validate(); err != nil {
		return DeliveredPage{}, err
	}

	dbq := s.currentSourceQuery(ctx, sourceID, jobID)
	var total int64
	if err := dbq.Count(&total).Error; err != nil {
		return DeliveredPage{}, err
	}
	var rows []SourceRecord
	if err := dbq.Order("id ASC").Limit(maxQueryScanRows).Find(&rows).Error; err != nil {
		return DeliveredPage{}, err
	}
	return ApplyRecordQuery(rows, total, sourceID, jobID, cursor, maxBytes, pageSize, baseURL, query)
}

// DeliverRecordsMultiQuery runs up to MaxMCPMultiQueries named RecordQuery views in a single
// MySQL read, bounding the combined payload to DefaultMCPMultiQueryMaxBytes (4 KiB).
func (s *Store) DeliverRecordsMultiQuery(
	ctx context.Context,
	sourceID string,
	jobID uint64,
	maxBytes, pageSize int,
	baseURL string,
	queries map[string]*RecordQuery,
) (DeliveredPage, error) {
	if len(queries) == 0 {
		return s.DeliverReceipt(ctx, sourceID, jobID, baseURL)
	}
	if len(queries) > MaxMCPMultiQueries {
		return DeliveredPage{}, fmt.Errorf("queries supports at most %d named views per call", MaxMCPMultiQueries)
	}
	for name, q := range queries {
		if strings.TrimSpace(name) == "" {
			return DeliveredPage{}, fmt.Errorf("queries keys must be non-empty")
		}
		if err := q.Validate(); err != nil {
			return DeliveredPage{}, fmt.Errorf("queries[%q]: %w", name, err)
		}
	}

	dbq := s.currentSourceQuery(ctx, sourceID, jobID)
	var total int64
	if err := dbq.Count(&total).Error; err != nil {
		return DeliveredPage{}, err
	}
	var rows []SourceRecord
	if err := dbq.Order("id ASC").Limit(maxQueryScanRows).Find(&rows).Error; err != nil {
		return DeliveredPage{}, err
	}
	return ApplyRecordQueries(rows, total, sourceID, jobID, maxBytes, pageSize, baseURL, queries)
}

// ApplyRecordQueries evaluates multiple named RecordQuery views deterministically over rows.
// Pure aggregate/extract views run first so their minimal footprint never starves row-returning views.
func ApplyRecordQueries(
	rows []SourceRecord,
	total int64,
	sourceID string,
	jobID uint64,
	maxBytes, pageSize int,
	baseURL string,
	queries map[string]*RecordQuery,
) (DeliveredPage, error) {
	if len(queries) == 0 {
		return DeliveredPage{
			SourceID: sourceID,
			JobID:    jobID,
			Mode:     "multi_query",
			Total:    total,
			Views:    map[string]*QueryView{},
		}, nil
	}
	if len(queries) > MaxMCPMultiQueries {
		return DeliveredPage{}, fmt.Errorf("queries supports at most %d named views per call", MaxMCPMultiQueries)
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMCPSafetyMaxBytes
	}

	var aggKeys, rowKeys []string
	for k, q := range queries {
		if q != nil && !q.WantsRows() {
			aggKeys = append(aggKeys, k)
		} else {
			rowKeys = append(rowKeys, k)
		}
	}
	sort.Strings(aggKeys)
	sort.Strings(rowKeys)

	reg := newSnapRegistry()
	out := DeliveredPage{
		SourceID: sourceID,
		JobID:    jobID,
		Mode:     "multi_query",
		Total:    total,
		Views:    make(map[string]*QueryView, len(queries)),
	}
	remaining := maxBytes
	totalReturned := 0
	type deferredView struct {
		view     *QueryView
		snapIdxs []int
	}
	var allViews []deferredView

	// Pass 1: pure aggregate / URL-extract views (typically 50–350 B each).
	for _, k := range aggKeys {
		q := queries[k]
		sub, snapIdxs, err := evalRecordQuery(rows, total, sourceID, jobID, "", remaining, pageSize, q, reg)
		if err != nil {
			return DeliveredPage{}, fmt.Errorf("queries[%q]: %w", k, err)
		}
		v := &QueryView{
			Matched:       sub.Matched,
			Groups:        sub.Groups,
			ExtractedURLs: sub.ExtractedURLs,
			URLLabels:     sub.URLLabels,
		}
		out.Views[k] = v
		allViews = append(allViews, deferredView{view: v, snapIdxs: snapIdxs})
		if raw, mErr := json.Marshal(v); mErr == nil {
			remaining -= len(raw)
		}
	}

	// Pass 2: row-returning views share the actual remaining safety budget dynamically.
	for idx, k := range rowKeys {
		q := queries[k]
		if q == nil {
			q = &RecordQuery{}
		}
		viewsLeft := len(rowKeys) - idx
		viewBudget := remaining
		if viewsLeft > 1 {
			viewBudget = remaining / viewsLeft
		}
		if viewBudget < 512 {
			viewBudget = 512
		}
		sub, snapIdxs, err := evalRecordQuery(rows, total, sourceID, jobID, "", viewBudget, pageSize, q, reg)
		if err != nil {
			return DeliveredPage{}, fmt.Errorf("queries[%q]: %w", k, err)
		}
		v := &QueryView{
			Matched:           sub.Matched,
			Returned:          sub.Returned,
			Groups:            sub.Groups,
			ExtractedURLs:     sub.ExtractedURLs,
			URLLabels:         sub.URLLabels,
			TruncatedToBudget: sub.TruncatedToBudget,
			NextCursor:        sub.NextCursor,
			Records:           sub.Records,
		}
		out.Views[k] = v
		allViews = append(allViews, deferredView{view: v, snapIdxs: snapIdxs})
		if raw, mErr := json.Marshal(v); mErr == nil {
			remaining -= len(raw)
		}
		totalReturned += sub.Returned
		if sub.TruncatedToBudget {
			out.TruncatedToBudget = true
		}
	}

	// Attach compact _snapshot_idx per row only when multiple distinct snapshots are present.
	if len(reg.refs) > 1 {
		for _, dv := range allViews {
			for i, rec := range dv.view.Records {
				if i < len(dv.snapIdxs) && dv.snapIdxs[i] >= 0 {
					rec["_snapshot_idx"] = dv.snapIdxs[i]
				}
			}
		}
	}
	out.SnapshotRefs = reg.refs
	out.Returned = totalReturned
	if out.TruncatedToBudget {
		out.ExportURLs = buildExportURLs(baseURL, sourceID, jobID)
	}
	return out, nil
}

// ApplyRecordQuery evaluates RecordQuery deterministically in memory over SourceRecord rows.
func ApplyRecordQuery(
	rows []SourceRecord,
	total int64,
	sourceID string,
	jobID uint64,
	cursor string,
	maxBytes, pageSize int,
	baseURL string,
	query *RecordQuery,
) (DeliveredPage, error) {
	reg := newSnapRegistry()
	page, snapIdxs, err := evalRecordQuery(rows, total, sourceID, jobID, cursor, maxBytes, pageSize, query, reg)
	if err != nil {
		return DeliveredPage{}, err
	}
	if len(reg.refs) > 1 {
		for i, rec := range page.Records {
			if i < len(snapIdxs) && snapIdxs[i] >= 0 {
				rec["_snapshot_idx"] = snapIdxs[i]
			}
		}
	}
	page.SnapshotRefs = reg.refs
	if page.TruncatedToBudget {
		page.ExportURLs = buildExportURLs(baseURL, sourceID, jobID)
	}
	return page, nil
}

func evalRecordQuery(
	rows []SourceRecord,
	total int64,
	sourceID string,
	jobID uint64,
	cursor string,
	maxBytes, pageSize int,
	query *RecordQuery,
	reg *snapRegistry,
) (DeliveredPage, []int, error) {
	if err := query.Validate(); err != nil {
		return DeliveredPage{}, nil, err
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMCPSafetyMaxBytes
	}
	if pageSize <= 0 || pageSize > DefaultMCPPageSize {
		pageSize = DefaultMCPPageSize
	}

	compiledWhere := make(map[int]*regexp.Regexp)
	for i, cond := range query.Where {
		if cond.Op == "regex" {
			re, err := regexp.Compile(stringifyValue(cond.Value))
			if err != nil {
				return DeliveredPage{}, nil, err
			}
			compiledWhere[i] = re
		}
	}

	matched := make([]decodedRecord, 0, len(rows))
	for _, r := range rows {
		obj := decodePayloadMap(r.PayloadJSON)
		if matchAllConditions(obj, query.Where, compiledWhere) {
			matched = append(matched, decodedRecord{row: r, obj: obj})
		}
	}

	var groups map[string]int
	if gb := strings.TrimSpace(query.GroupBy); gb != "" {
		groups = make(map[string]int)
		for _, item := range matched {
			key := stringifyValue(item.obj[gb])
			if key == "" {
				key = "(empty)"
			}
			if query.GroupPrefix > 0 {
				runes := []rune(key)
				if len(runes) > query.GroupPrefix {
					key = string(runes[:query.GroupPrefix])
				}
			}
			groups[key]++
		}
	}

	var urlRegex *regexp.Regexp
	if query.ExtractURLs != nil && query.ExtractURLs.Regex != "" {
		urlRegex, _ = regexp.Compile(query.ExtractURLs.Regex)
	}

	page := DeliveredPage{
		SourceID: sourceID,
		JobID:    jobID,
		Mode:     "query",
		Total:    total,
		Matched:  len(matched),
		Groups:   groups,
	}
	seenURLs := make(map[string]bool)
	urlLabels := make(map[string]string)
	if query.ExtractURLs != nil && query.ExtractURLs.Field != "" {
		for _, item := range matched {
			extractMatchingURLs(item.obj, query.ExtractURLs, urlRegex, seenURLs, &page.ExtractedURLs, urlLabels)
			if len(page.ExtractedURLs) >= 20 {
				break
			}
		}
	}
	if len(urlLabels) > 0 {
		page.URLLabels = urlLabels
	}

	// Pure aggregate / extract query: preserve provenance in snapshot_refs and omit row records.
	if !query.WantsRows() {
		for _, item := range matched {
			reg.add(item.row.SnapshotRef)
		}
		return page, nil, nil
	}

	if orderField, desc := parseOrderBy(query.OrderBy); orderField != "" {
		sort.SliceStable(matched, func(i, j int) bool {
			vi, oki := matched[i].obj[orderField]
			vj, okj := matched[j].obj[orderField]
			if !oki && !okj {
				return false
			}
			if !oki {
				return false
			}
			if !okj {
				return true
			}
			cmp := compareValues(vi, vj)
			if desc {
				return cmp > 0
			}
			return cmp < 0
		})
	}

	offset := 0
	if cursor != "" {
		parsed, err := strconv.Atoi(cursor)
		if err != nil || parsed < 0 {
			return DeliveredPage{}, nil, fmt.Errorf("invalid query cursor %q", cursor)
		}
		offset = parsed
	}
	if offset > len(matched) {
		offset = len(matched)
	}
	window := matched[offset:]
	limit := pageSize
	if query.Limit > 0 && query.Limit < limit {
		limit = query.Limit
	}

	maxFieldChars := query.MaxFieldChars
	if maxFieldChars <= 0 {
		maxFieldChars = DefaultQueryFieldChars
	} else if maxFieldChars > 1024 {
		maxFieldChars = 1024
	}

	// Account for actual non-row bytes already in this view instead of a magic offset.
	bytesUsed := 0
	if len(page.Groups) > 0 {
		if b, err := json.Marshal(page.Groups); err == nil {
			bytesUsed += len(b)
		}
	}
	if len(page.ExtractedURLs) > 0 {
		if b, err := json.Marshal(page.ExtractedURLs); err == nil {
			bytesUsed += len(b)
		}
	}
	if len(page.URLLabels) > 0 {
		if b, err := json.Marshal(page.URLLabels); err == nil {
			bytesUsed += len(b)
		}
	}

	snapIdxs := make([]int, 0, min(len(window), limit))
	for i, item := range window {
		if i >= limit {
			if offset+i < len(matched) && (query.Limit == 0 || offset+i < query.Limit) {
				page.NextCursor = strconv.Itoa(offset + i)
			}
			break
		}
		proj := projectRecord(item.obj, query, maxFieldChars)
		rowBytes, _ := json.Marshal(proj)
		// Reserve 20 bytes for optional "_snapshot_idx":N field + JSON comma.
		if bytesUsed+len(rowBytes)+20 > maxBytes && len(page.Records) > 0 {
			page.TruncatedToBudget = true
			page.NextCursor = strconv.Itoa(offset + i)
			break
		}
		bytesUsed += len(rowBytes) + 20
		page.Records = append(page.Records, proj)
		snapIdxs = append(snapIdxs, reg.add(item.row.SnapshotRef))
	}
	page.Returned = len(page.Records)
	if len(matched) > page.Returned && page.NextCursor != "" && page.TruncatedToBudget {
		page.Summary = fmt.Sprintf("Query returned %d of %d matched (%d total); pass cursor=%s or export_urls.", page.Returned, len(matched), total, page.NextCursor)
	}
	return page, snapIdxs, nil
}

func extractMatchingURLs(obj map[string]any, spec *URLExtractSpec, re *regexp.Regexp, seen map[string]bool, dst *[]string, labels map[string]string) {
	if spec == nil || len(*dst) >= 20 {
		return
	}
	var texts []string
	if spec.Field == "*" {
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if s, ok := obj[k].(string); ok && s != "" {
				texts = append(texts, s)
			}
		}
	} else if s, ok := obj[spec.Field].(string); ok && s != "" {
		texts = append(texts, s)
	}
	patternLower := strings.ToLower(strings.TrimSpace(spec.Pattern))
	for _, txt := range texts {
		mdLabels := make(map[string]string)
		for _, m := range reMarkdownLink.FindAllStringSubmatch(txt, -1) {
			if len(m) == 3 {
				lbl := strings.TrimSpace(m[1])
				u := strings.TrimRight(m[2], `.,;:!?)"]'>`)
				if lbl != "" && u != "" && !strings.HasPrefix(lbl, "http") {
					mdLabels[u] = lbl
				}
			}
		}
		for _, raw := range reHTTPURL.FindAllString(txt, -1) {
			u := strings.TrimRight(raw, `.,;:!?)"]'>`)
			if u == "" || seen[u] {
				continue
			}
			if patternLower != "" && !strings.Contains(strings.ToLower(u), patternLower) {
				continue
			}
			if re != nil && !re.MatchString(u) {
				continue
			}
			seen[u] = true
			*dst = append(*dst, u)
			if labels != nil {
				if lbl, ok := mdLabels[u]; ok {
					labels[u] = lbl
				}
			}
			if len(*dst) >= 20 {
				return
			}
		}
	}
}

func decodePayloadMap(raw string) map[string]any {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil || obj == nil {
		return map[string]any{"raw": raw}
	}
	return obj
}

func matchAllConditions(obj map[string]any, conds []FilterCondition, compiled map[int]*regexp.Regexp) bool {
	for i, c := range conds {
		val, exists := obj[c.Field]
		if !exists {
			if c.Op == "ne" {
				continue
			}
			return false
		}
		switch c.Op {
		case "eq":
			if compareValues(val, c.Value) != 0 {
				return false
			}
		case "ne":
			if compareValues(val, c.Value) == 0 {
				return false
			}
		case "gt":
			if compareValues(val, c.Value) <= 0 {
				return false
			}
		case "gte":
			if compareValues(val, c.Value) < 0 {
				return false
			}
		case "lt":
			if compareValues(val, c.Value) >= 0 {
				return false
			}
		case "lte":
			if compareValues(val, c.Value) > 0 {
				return false
			}
		case "contains":
			if !strings.Contains(stringifyValue(val), stringifyValue(c.Value)) {
				return false
			}
		case "icontains":
			if !strings.Contains(strings.ToLower(stringifyValue(val)), strings.ToLower(stringifyValue(c.Value))) {
				return false
			}
		case "prefix":
			if !strings.HasPrefix(stringifyValue(val), stringifyValue(c.Value)) {
				return false
			}
		case "suffix":
			if !strings.HasSuffix(stringifyValue(val), stringifyValue(c.Value)) {
				return false
			}
		case "regex":
			re := compiled[i]
			if re == nil || !re.MatchString(stringifyValue(val)) {
				return false
			}
		}
	}
	return true
}

func parseOrderBy(orderBy string) (string, bool) {
	s := strings.TrimSpace(orderBy)
	if s == "" {
		return "", false
	}
	if strings.HasPrefix(s, "-") {
		return strings.TrimSpace(s[1:]), true
	}
	parts := strings.Fields(s)
	if len(parts) == 1 {
		return parts[0], false
	}
	return parts[0], strings.EqualFold(parts[1], "desc")
}

func compareValues(a, b any) int {
	fa, oka := toFloat64(a)
	fb, okb := toFloat64(b)
	if oka && okb {
		if math.Abs(fa-fb) < 1e-12 {
			return 0
		}
		if fa < fb {
			return -1
		}
		return 1
	}
	return strings.Compare(stringifyValue(a), stringifyValue(b))
}

func toFloat64(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case uint64:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case string:
		trimmed := strings.TrimSpace(x)
		if trimmed == "" {
			return 0, false
		}
		for _, sym := range []string{"£", "$", "€", "¥"} {
			if strings.HasPrefix(trimmed, sym) {
				trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, sym))
				break
			}
		}
		f, err := strconv.ParseFloat(trimmed, 64)
		return f, err == nil
	default:
		return 0, false
	}
}

func stringifyValue(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case json.Number:
		return x.String()
	case bool:
		if x {
			return "true"
		}
		return "false"
	default:
		return fmt.Sprint(x)
	}
}

func projectRecord(obj map[string]any, q *RecordQuery, maxChars int) map[string]any {
	out := make(map[string]any, len(obj))
	if len(q.Fields) > 0 {
		for _, f := range q.Fields {
			if v, ok := obj[f]; ok {
				out[f] = truncateFieldValue(v, maxChars)
			}
		}
		return out
	}
	exclude := make(map[string]bool, len(q.ExcludeFields))
	for _, f := range q.ExcludeFields {
		exclude[f] = true
	}
	for k, v := range obj {
		if exclude[k] {
			continue
		}
		out[k] = truncateFieldValue(v, maxChars)
	}
	return out
}

func truncateFieldValue(v any, maxChars int) any {
	if maxChars <= 0 {
		return v
	}
	s, ok := v.(string)
	if !ok {
		return v
	}
	runes := []rune(s)
	if len(runes) <= maxChars {
		return s
	}
	return string(runes[:maxChars]) + "...(truncated; archived in snapshot)"
}
