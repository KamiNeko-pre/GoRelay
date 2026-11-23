package repository

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"gorelay/internal/analysis"
	taskmodel "gorelay/internal/task"
)

const maxAnalysisSnapshotLimit = 200

type analysisSnapshotRow struct {
	TaskID      uint64
	RuleID      string
	Status      string
	FinishedAt  *time.Time
	FailureCode sql.NullString
	LastError   sql.NullString
	Payload     sql.NullString
}

// ListEndedTaskSnapshots reads the task metadata and optional result payload
// needed by the frontend. Running tasks are excluded because they are not
// stable observations yet; failed tasks remain visible for health analysis.
func ListEndedTaskSnapshots(
	ctx context.Context,
	ruleID string,
	from *time.Time,
	to *time.Time,
	limit int,
) ([]analysis.SnapshotRecord, error) {
	if DB == nil {
		return nil, fmt.Errorf("analysis snapshots: database is not initialized")
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > maxAnalysisSnapshotLimit {
		limit = maxAnalysisSnapshotLimit
	}

	query := DB.WithContext(ctx).
		Table("crawl_tasks AS tasks").
		Select(strings.Join([]string{
			"tasks.id AS task_id",
			"tasks.target AS rule_id",
			"tasks.status AS status",
			"tasks.finished_at AS finished_at",
			"tasks.failure_code AS failure_code",
			"tasks.last_error AS last_error",
			"results.payload AS payload",
		}, ", ")).
		Joins("LEFT JOIN crawl_results AS results ON results.task_id = tasks.id").
		Where("tasks.status IN ?", []taskmodel.Status{taskmodel.StatusSucceeded, taskmodel.StatusFailed}).
		Where("tasks.finished_at IS NOT NULL").
		Order("tasks.finished_at DESC").
		Order("tasks.id DESC").
		Limit(limit)

	if strings.TrimSpace(ruleID) != "" {
		query = query.Where("tasks.target = ?", ruleID)
	}
	if from != nil {
		query = query.Where("tasks.finished_at >= ?", *from)
	}
	if to != nil {
		query = query.Where("tasks.finished_at <= ?", *to)
	}

	var rows []analysisSnapshotRow
	if err := query.Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("list ended task snapshots: %w", err)
	}

	records := make([]analysis.SnapshotRecord, 0, len(rows))
	for _, row := range rows {
		records = append(records, analysis.SnapshotRecord{
			TaskID:      row.TaskID,
			RuleID:      row.RuleID,
			Status:      row.Status,
			FinishedAt:  row.FinishedAt,
			FailureCode: nullableString(row.FailureCode),
			LastError:   nullableString(row.LastError),
			Payload:     nullableString(row.Payload),
		})
	}
	return records, nil
}

func nullableString(value sql.NullString) string {
	if !value.Valid {
		return ""
	}
	return value.String
}
