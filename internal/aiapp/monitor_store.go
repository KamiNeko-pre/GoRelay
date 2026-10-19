package aiapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/knowledge"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"time"
)

// Monitor stores a recurring task configuration, not a domain-specific entity.
type Monitor struct {
	ID          uint64     `json:"id" gorm:"primaryKey;autoIncrement"`
	RequestKey  string     `json:"request_key" gorm:"size:128;uniqueIndex"`
	Goal        string     `json:"goal" gorm:"type:text"`
	ConfigJSON  string     `json:"-" gorm:"type:longtext"`
	Enabled     bool       `json:"enabled" gorm:"index:monitor_due,priority:1"`
	NextRunAt   *time.Time `json:"next_run_at,omitempty" gorm:"index:monitor_due,priority:2"`
	ActiveRunID string     `json:"active_run_id,omitempty" gorm:"size:32"`
	LastRunID   string     `json:"last_run_id,omitempty" gorm:"size:32"`
	PlanJSON    string     `json:"-" gorm:"type:longtext"`
	Version     uint64     `json:"version"`
	LastError   string     `json:"last_error,omitempty" gorm:"type:text"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

func (Monitor) TableName() string { return "agent_monitors" }

type MonitorCheck struct {
	ID             uint64     `json:"id" gorm:"primaryKey;autoIncrement"`
	MonitorID      uint64     `json:"monitor_id" gorm:"index:monitor_checks,priority:1"`
	RequestKey     string     `json:"request_key" gorm:"size:128;uniqueIndex"`
	RunID          string     `json:"run_id" gorm:"size:32;uniqueIndex"`
	ConfigVersion  uint64     `json:"config_version"`
	Mode           string     `json:"mode" gorm:"size:16"`
	PlanJSON       string     `json:"-" gorm:"type:longtext"`
	FallbackReason string     `json:"fallback_reason,omitempty" gorm:"type:text"`
	ProcessedAt    *time.Time `json:"processed_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at" gorm:"index:monitor_checks,priority:2"`
}

func (MonitorCheck) TableName() string { return "agent_monitor_checks" }

type UpdateMonitorRequest struct {
	Config  *CreateMonitorRequest `json:"config,omitempty"`
	Enabled *bool                 `json:"enabled,omitempty"`
}

type MonitorView struct {
	Monitor
	ExecutionPlanReady bool                 `json:"execution_plan_ready"`
	Config             CreateMonitorRequest `json:"config"`
	Checks             []MonitorCheck       `json:"checks"`
}

func (m Monitor) Config() (CreateMonitorRequest, error) {
	var out CreateMonitorRequest
	err := json.Unmarshal([]byte(m.ConfigJSON), &out)
	return out, err
}

func (s *Service) Monitor(ctx context.Context, id uint64) (Monitor, error) {
	var out Monitor
	err := s.Data.DB.WithContext(ctx).First(&out, id).Error
	return out, err
}

func (s *Service) MonitorView(ctx context.Context, id uint64) (MonitorView, error) {
	m, err := s.Monitor(ctx, id)
	if err != nil {
		return MonitorView{}, err
	}
	config, err := m.Config()
	if err != nil {
		return MonitorView{}, err
	}
	out := MonitorView{Monitor: m, ExecutionPlanReady: m.PlanJSON != "", Config: config, Checks: []MonitorCheck{}}
	err = s.Data.DB.WithContext(ctx).Where("monitor_id = ?", id).Order("id DESC").Limit(20).Find(&out.Checks).Error
	return out, err
}

func (s *Service) CreateMonitor(ctx context.Context, in CreateMonitorRequest) (Monitor, error) {
	if err := in.Normalize(); err != nil {
		return Monitor{}, err
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return Monitor{}, err
	}
	out := Monitor{RequestKey: in.RequestKey, Goal: in.Goal, ConfigJSON: string(raw), Version: 1}
	err = s.Data.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&out)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 0 {
			return nil
		}
		if err := tx.Where("request_key = ?", in.RequestKey).First(&out).Error; err != nil {
			return err
		}
		if out.ConfigJSON != string(raw) {
			return agentdata.ErrConflict
		}
		return nil
	})
	return out, err
}

func (s *Service) UpdateMonitor(ctx context.Context, id uint64, in UpdateMonitorRequest) (Monitor, error) {
	var out Monitor
	if in.Config == nil && in.Enabled == nil {
		return out, fmt.Errorf("monitor update requires config or enabled")
	}
	err := s.Data.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&out, id).Error; err != nil {
			return err
		}
		if in.Config != nil {
			if out.ActiveRunID != "" {
				return agentdata.ErrConflict
			}
			config := *in.Config
			previous, err := out.Config()
			if err != nil {
				return err
			}
			if config.RequestKey == "" {
				config.RequestKey = out.RequestKey
			}
			if config.RequestKey != out.RequestKey {
				return fmt.Errorf("monitor request_key is immutable")
			}
			if err := config.Normalize(); err != nil {
				return err
			}
			raw, err := json.Marshal(config)
			if err != nil {
				return err
			}
			if out.ConfigJSON != string(raw) {
				out.ConfigJSON, out.Goal = string(raw), config.Goal
				out.Version++
				// Conditions and cadence consume facts after acquisition; only
				// the inputs passed to Create may invalidate its saved recipe.
				priorInput := CreateRunRequest{Goal: previous.Goal, Spec: previous.Spec, SourceScope: previous.SourceScope}
				nextInput := CreateRunRequest{Goal: config.Goal, Spec: config.Spec, SourceScope: config.SourceScope}
				if agentdata.Hash(priorInput) == agentdata.Hash(nextInput) && out.PlanJSON != "" {
					var plan MonitorPlan
					if err = json.Unmarshal([]byte(out.PlanJSON), &plan); err != nil {
						return err
					}
					plan.ConfigVersion = out.Version
					encoded, e := json.Marshal(plan)
					if e != nil {
						return e
					}
					out.PlanJSON = string(encoded)
				} else {
					out.PlanJSON = ""
				}
			}
		}
		config, err := out.Config()
		if err != nil {
			return err
		}
		if in.Enabled != nil {
			out.Enabled = *in.Enabled
		}
		out.NextRunAt = nil
		if out.Enabled {
			next := time.Now().UTC().Add(time.Duration(config.IntervalSeconds) * time.Second)
			out.NextRunAt = &next
		}
		return tx.Save(&out).Error
	})
	return out, err
}

// The monitor lock owns run creation/linking. Creating a Run never calls a
// provider; the existing Agent runtime advances the accepted work afterward.
func (s *Service) RefreshMonitor(ctx context.Context, id uint64, key string) (agentdata.Run, error) {
	return s.refreshMonitor(ctx, id, key, nil)
}

func (s *Service) refreshMonitor(ctx context.Context, id uint64, key string, due *time.Time) (agentdata.Run, error) {
	if key == "" || len(key) > 128 {
		return agentdata.Run{}, fmt.Errorf("refresh request_key required (1..128 characters)")
	}
	var run agentdata.Run
	err := s.Data.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var m Monitor
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&m, id).Error; err != nil {
			return err
		}
		if due != nil && (!m.Enabled || m.NextRunAt == nil || !m.NextRunAt.Equal(*due) || m.NextRunAt.After(time.Now())) {
			return agentdata.ErrConflict
		}
		var prior MonitorCheck
		err := tx.Where("request_key = ?", key).First(&prior).Error
		if err == nil {
			if prior.MonitorID != id {
				return agentdata.ErrConflict
			}
			return tx.First(&run, "id = ?", prior.RunID).Error
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if m.ActiveRunID != "" {
			return agentdata.ErrConflict
		}
		config, err := m.Config()
		if err != nil {
			return err
		}
		owned := *s
		owned.Data = agentdata.NewStore(tx)
		owned.Knowledge = knowledge.NewStore(tx)
		run, err = owned.Create(ctx, CreateRunRequest{RequestKey: "monitor-" + agentdata.Hash([]any{id, key})[:48], Goal: config.Goal, Spec: config.Spec, SourceScope: config.SourceScope})
		if err != nil {
			return err
		}
		check := MonitorCheck{MonitorID: id, RequestKey: key, RunID: run.ID, ConfigVersion: m.Version, Mode: "agent"}
		if m.PlanJSON != "" {
			check.Mode = "plan"
			check.PlanJSON = m.PlanJSON
		}
		if err := tx.Create(&check).Error; err != nil {
			return err
		}
		m.ActiveRunID = run.ID
		m.LastError = ""
		if m.Enabled {
			next := time.Now().UTC().Add(time.Duration(config.IntervalSeconds) * time.Second)
			m.NextRunAt = &next
		}
		return tx.Save(&m).Error
	})
	return run, err
}
