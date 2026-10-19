package aiapp

import (
	"context"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/testdb"
	"testing"
	"time"
)

func TestMonitorSchedulerRecoversTerminalCheckAndSubmitsOnce(t *testing.T) {
	db := testdb.Open(t)
	s := NewService(db)
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	spec := monitorSpec()
	m, err := s.CreateMonitor(ctx, CreateMonitorRequest{RequestKey: "due", Goal: spec.Goal, Spec: &spec})
	if err != nil {
		t.Fatal(err)
	}
	enabled := true
	m, err = s.UpdateMonitor(ctx, m.ID, UpdateMonitorRequest{Enabled: &enabled})
	if err != nil {
		t.Fatal(err)
	}
	due := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	if err = db.Model(&m).Update("next_run_at", due).Error; err != nil {
		t.Fatal(err)
	}
	paused, err := s.CreateMonitor(ctx, CreateMonitorRequest{RequestKey: "paused", Goal: spec.Goal, Spec: &spec})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Model(&paused).Update("next_run_at", due).Error; err != nil {
		t.Fatal(err)
	}
	// A stale due scan must not override a subsequent pause.
	off := false
	_, err = s.UpdateMonitor(ctx, m.ID, UpdateMonitorRequest{Enabled: &off})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.refreshMonitor(ctx, m.ID, "stale-schedule", &due); err != agentdata.ErrConflict {
		t.Fatal("stale scan ignored pause", err)
	}
	_, err = s.UpdateMonitor(ctx, m.ID, UpdateMonitorRequest{Enabled: &enabled})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Model(&m).Update("next_run_at", due).Error; err != nil {
		t.Fatal(err)
	}
	if err = s.SweepMonitors(ctx); err != nil {
		t.Fatal(err)
	}
	current, err := s.Monitor(ctx, m.ID)
	if err != nil || current.ActiveRunID == "" || current.NextRunAt == nil || !current.NextRunAt.After(time.Now()) {
		t.Fatal(current, err)
	}
	// Recovery happens after a terminal Run commit, including when the service restarts.
	run, err := s.Data.Run(ctx, current.ActiveRunID)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Model(&run).Updates(map[string]any{"status": "failed", "last_error": "source unavailable"}).Error; err != nil {
		t.Fatal(err)
	}
	restarted := NewService(db)
	if err = restarted.SweepMonitors(ctx); err != nil {
		t.Fatal(err)
	}
	current, err = s.Monitor(ctx, m.ID)
	if err != nil || current.ActiveRunID != "" || current.LastRunID != run.ID || current.LastError == "" {
		t.Fatal("terminal check not recovered", current, err)
	}
	if err = s.SweepMonitors(ctx); err != nil {
		t.Fatal(err)
	}
	var count int64
	db.Model(&MonitorCheck{}).Count(&count)
	if count != 1 {
		t.Fatal("recovery or paused row created duplicate", count)
	}
}

func TestMonitorScheduleAdmissionFailureDoesNotStopOtherTasks(t *testing.T) {
	t.Setenv("GORELAY_DEMO_DAILY_RUNS", "1")
	db := testdb.Open(t)
	s := NewService(db)
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	spec := monitorSpec()
	on := true
	due := time.Now().UTC().Add(-time.Minute)
	for _, key := range []string{"one", "two"} {
		m, e := s.CreateMonitor(ctx, CreateMonitorRequest{RequestKey: key, Goal: spec.Goal, Spec: &spec})
		if e != nil {
			t.Fatal(e)
		}
		if _, e = s.UpdateMonitor(ctx, m.ID, UpdateMonitorRequest{Enabled: &on}); e != nil {
			t.Fatal(e)
		}
		if e = db.Model(&m).Update("next_run_at", due).Error; e != nil {
			t.Fatal(e)
		}
	}
	if err := s.SweepMonitors(ctx); err != nil {
		t.Fatal("one quota refusal stopped the Agent runtime", err)
	}
	var rows []Monitor
	db.Order("id ASC").Find(&rows)
	if len(rows) != 2 || rows[0].ActiveRunID == "" || rows[1].ActiveRunID != "" || rows[1].LastError == "" || rows[1].NextRunAt == nil || !rows[1].NextRunAt.After(time.Now()) {
		t.Fatal("admission refusal not durable/backed off", rows)
	}
}
