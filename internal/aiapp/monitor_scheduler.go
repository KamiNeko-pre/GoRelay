package aiapp

import (
	"context"
	"errors"
	"go-lua-crawler/internal/agentdata"
	"strconv"
	"time"
)

// The sweep is durable recovery/acceptance only; existing Run/Worker execution
// owns the actual work. Row locks and persisted due slots arbitrate multiple servers.
func (s *Service) SweepMonitors(ctx context.Context) error {
	checks := []MonitorCheck{}
	err := s.Data.DB.WithContext(ctx).Model(&MonitorCheck{}).
		Joins("JOIN agent_runs ON agent_runs.id = agent_monitor_checks.run_id").
		Where("agent_monitor_checks.processed_at IS NULL AND agent_runs.status IN ?", []string{"completed", "partial", "failed", "cancelled", "expired"}).
		Select("agent_monitor_checks.*").Order("agent_monitor_checks.id ASC").Limit(8).Find(&checks).Error
	if err != nil {
		return err
	}
	for _, c := range checks {
		if err = s.ProcessMonitorCheck(ctx, c.RunID); err != nil && !errors.Is(err, agentdata.ErrConflict) {
			return err
		}
	}
	due := []Monitor{}
	if err = s.Data.DB.WithContext(ctx).Where("enabled = ? AND next_run_at <= ? AND active_run_id = ?", true, time.Now().UTC(), "").Order("next_run_at ASC, id ASC").Limit(8).Find(&due).Error; err != nil {
		return err
	}
	for _, m := range due {
		key := "schedule-" + strconv.FormatUint(m.ID, 10) + "-" + m.NextRunAt.UTC().Format("20060102T150405.999999999Z")
		if _, err = s.refreshMonitor(ctx, m.ID, key, m.NextRunAt); err != nil && !errors.Is(err, agentdata.ErrConflict) {
			// Admission belongs to this monitor. Persist the refusal and wait one
			// configured interval; never retry it each second or terminate other Runs.
			config, configErr := m.Config()
			if configErr != nil {
				return configErr
			}
			next := time.Now().UTC().Add(time.Duration(config.IntervalSeconds) * time.Second)
			if saveErr := s.Data.DB.WithContext(ctx).Model(&Monitor{}).Where("id = ? AND enabled = ? AND next_run_at = ? AND active_run_id = ?", m.ID, true, m.NextRunAt, "").Updates(map[string]any{"last_error": err.Error(), "next_run_at": next}).Error; saveErr != nil {
				return saveErr
			}
		}
	}
	return nil
}
