package pipeline

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

const maxExportRows = 5000

// ExportRecords renders stored cleaned records as JSON, CSV, or Markdown directly
// from MySQL without passing bulk raw data through any LLM context window.
func (s *Store) ExportRecords(ctx context.Context, sourceID string, jobID uint64, format string) (string, string, error) {
	if strings.TrimSpace(sourceID) == "" && jobID == 0 {
		return "", "", fmt.Errorf("source_id or job_id is required for export")
	}
	var rows []SourceRecord
	q := s.currentSourceQuery(ctx, sourceID, jobID)
	if err := q.Order("id ASC").Limit(maxExportRows).Find(&rows).Error; err != nil {
		return "", "", err
	}

	decoded := make([]map[string]any, 0, len(rows))
	fieldSet := make(map[string]bool)
	for _, r := range rows {
		obj := decodePayloadMap(r.PayloadJSON)
		for k := range obj {
			fieldSet[k] = true
		}
		if r.SnapshotRef != "" {
			obj["_snapshot_ref"] = r.SnapshotRef
		}
		decoded = append(decoded, obj)
	}
	columns := make([]string, 0, len(fieldSet)+1)
	for k := range fieldSet {
		columns = append(columns, k)
	}
	sort.Strings(columns)
	if len(decoded) > 0 {
		columns = append(columns, "_snapshot_ref")
	}

	switch strings.ToLower(strings.TrimSpace(format)) {
	case "", "json":
		raw, err := json.MarshalIndent(map[string]any{
			"source_id": sourceID,
			"job_id":    jobID,
			"total":     len(decoded),
			"records":   decoded,
		}, "", "  ")
		if err != nil {
			return "", "", err
		}
		return "application/json; charset=utf-8", string(raw), nil
	case "csv":
		var buf bytes.Buffer
		w := csv.NewWriter(&buf)
		if err := w.Write(columns); err != nil {
			return "", "", err
		}
		for _, obj := range decoded {
			line := make([]string, len(columns))
			for i, col := range columns {
				line[i] = stringifyValue(obj[col])
			}
			if err := w.Write(line); err != nil {
				return "", "", err
			}
		}
		w.Flush()
		if err := w.Error(); err != nil {
			return "", "", err
		}
		return "text/csv; charset=utf-8", buf.String(), nil
	case "md", "markdown":
		var b strings.Builder
		if len(decoded) == 0 {
			b.WriteString("_(no records)_")
			return "text/markdown; charset=utf-8", b.String(), nil
		}
		b.WriteString("| " + strings.Join(columns, " | ") + " |\n")
		seps := make([]string, len(columns))
		for i := range seps {
			seps[i] = "---"
		}
		b.WriteString("| " + strings.Join(seps, " | ") + " |\n")
		for _, obj := range decoded {
			cells := make([]string, len(columns))
			for i, col := range columns {
				cell := strings.ReplaceAll(stringifyValue(obj[col]), "\n", " ")
				cell = strings.ReplaceAll(cell, "|", "\\|")
				if len(cell) > 160 {
					cell = cell[:160] + "..."
				}
				cells[i] = cell
			}
			b.WriteString("| " + strings.Join(cells, " | ") + " |\n")
		}
		return "text/markdown; charset=utf-8", b.String(), nil
	default:
		return "", "", fmt.Errorf("unsupported export format %q (use json, csv, or md)", format)
	}
}

func buildExportURLs(baseURL, sourceID string, jobID uint64) map[string]string {
	prefix := strings.TrimRight(baseURL, "/")
	var basePath string
	if strings.TrimSpace(sourceID) != "" {
		basePath = prefix + "/api/v1/sources/" + url.PathEscape(sourceID) + "/export"
	} else if jobID > 0 {
		basePath = prefix + "/api/v1/jobs/" + strconv.FormatUint(jobID, 10) + "/export"
	} else {
		return nil
	}
	return map[string]string{
		"json": basePath + "?format=json",
		"csv":  basePath + "?format=csv",
		"md":   basePath + "?format=md",
	}
}
