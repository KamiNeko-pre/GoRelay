package pipeline

import (
	"fmt"
	"strings"

	"gorelay/internal/acquisition"
	"gorelay/internal/contract"
)

// CheckSilentDrift runs inline quality & schema validation before committing to
// source_records so broken layouts or soft-200 blocks never overwrite valid data.
func CheckSilentDrift(rows []map[string]any, schema contract.OutputSchema, evidence []acquisition.Response, allowEmpty bool) *DriftDiagnosis {
	if len(rows) == 0 {
		if soft := detectSoftBlock(evidence); soft != nil {
			return soft
		}
		if !allowEmpty || schema.MinRows > 0 {
			return &DriftDiagnosis{
				Code:      "SILENT_EMPTY_RESULT",
				Reason:    "connector yielded 0 rows when non-empty output was expected",
				TotalRows: 0,
			}
		}
		return nil
	}

	if schema.MinRows > 0 && len(rows) < schema.MinRows {
		return &DriftDiagnosis{
			Code:      "ROW_COUNT_BELOW_BASELINE",
			Reason:    fmt.Sprintf("yielded %d rows, below expected min_rows %d", len(rows), schema.MinRows),
			TotalRows: len(rows),
			SampleRow: rows[0],
		}
	}

	failed := 0
	var firstBrokenRow map[string]any
	var firstReason string
	for _, row := range rows {
		for _, field := range schema.Fields {
			if !field.Required {
				continue
			}
			val, exists := row[field.Name]
			if !exists || !contract.FieldMatches(val, field.Type) {
				failed++
				if firstBrokenRow == nil {
					firstBrokenRow = row
					firstReason = fmt.Sprintf("required field %q missing or incompatible with type %s", field.Name, field.Type)
				}
				break
			}
		}
	}
	if failed > 0 && float64(failed)/float64(len(rows)) > 0.2 {
		return &DriftDiagnosis{
			Code:       "SCHEMA_DRIFT_MISSING_FIELDS",
			Reason:     fmt.Sprintf("%d/%d rows failed schema contract (%s)", failed, len(rows), firstReason),
			FailedRows: failed,
			TotalRows:  len(rows),
			SampleRow:  firstBrokenRow,
		}
	}
	return nil
}

func detectSoftBlock(evidence []acquisition.Response) *DriftDiagnosis {
	markers := []string{
		"access denied",
		"verify you are human",
		"enable javascript",
		"attention required! | cloudflare",
		"cf-browser-verification",
		"g-recaptcha",
		"rate limit exceeded",
	}
	for _, ev := range evidence {
		if ev.Receipt.Status == 401 || ev.Receipt.Status == 403 || ev.Receipt.Status == 429 {
			return &DriftDiagnosis{
				Code:       "SOFT_HTTP_BLOCK",
				Reason:     fmt.Sprintf("upstream returned HTTP %d with 0 extracted rows", ev.Receipt.Status),
				HTTPStatus: ev.Receipt.Status,
			}
		}
		lower := strings.ToLower(ev.Body)
		if len(lower) > 8192 {
			lower = lower[:8192]
		}
		for _, marker := range markers {
			if strings.Contains(lower, marker) {
				return &DriftDiagnosis{
					Code:       "SOFT_BLOCK_PAGE",
					Reason:     fmt.Sprintf("upstream body matched soft-block marker %q with 0 extracted rows", marker),
					HTTPStatus: ev.Receipt.Status,
				}
			}
		}
	}
	return nil
}
