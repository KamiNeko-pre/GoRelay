package information

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"go-lua-crawler/internal/agentdata"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type MonitorConfig struct {
	RequestKey      string   `json:"request_key"`
	Goal            string   `json:"goal"`
	DeliveryID      string   `json:"delivery_id,omitempty"`
	SourceScope     []string `json:"source_scope,omitempty"`
	Focus           string   `json:"focus,omitempty"`
	IntervalSeconds int64    `json:"interval_seconds"`
}

type Monitor struct {
	ID                 uint64     `json:"id" gorm:"primaryKey"`
	RequestKey         string     `json:"request_key" gorm:"size:128;uniqueIndex"`
	Goal               string     `json:"goal" gorm:"type:text"`
	ConfigJSON         string     `json:"-" gorm:"type:longtext"`
	Enabled            bool       `json:"enabled" gorm:"index"`
	NextRunAt          *time.Time `json:"next_run_at,omitempty" gorm:"index"`
	ActiveRequestID    string     `json:"active_request_id,omitempty" gorm:"size:32"`
	LastRequestID      string     `json:"last_request_id,omitempty" gorm:"size:32"`
	BaselineDeliveryID string     `json:"baseline_delivery_id,omitempty" gorm:"size:32"`
	LastDeliveryID     string     `json:"last_delivery_id,omitempty" gorm:"size:32"`
	LastError          string     `json:"last_error,omitempty" gorm:"type:text"`
	Version            uint64     `json:"version"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

func (Monitor) TableName() string { return "information_monitors" }

type MonitorCheck struct {
	ID                 uint64     `json:"id" gorm:"primaryKey"`
	MonitorID          uint64     `json:"monitor_id" gorm:"index"`
	RequestKey         string     `json:"request_key" gorm:"size:128;uniqueIndex"`
	RequestID          string     `json:"request_id" gorm:"size:32;uniqueIndex;default:null"`
	ConfigVersion      uint64     `json:"config_version"`
	BaselineDeliveryID string     `json:"baseline_delivery_id,omitempty" gorm:"size:32"`
	DeliveryID         string     `json:"delivery_id,omitempty" gorm:"size:32"`
	ProcessedAt        *time.Time `json:"processed_at,omitempty" gorm:"index"`
	Outcome            string     `json:"outcome,omitempty" gorm:"size:32"`
	LastError          string     `json:"last_error,omitempty" gorm:"type:text"`
	CreatedAt          time.Time  `json:"created_at"`
}

func (MonitorCheck) TableName() string { return "information_monitor_checks" }

type MonitorInbox struct {
	ID                 uint64    `json:"id" gorm:"primaryKey"`
	CheckID            uint64    `json:"check_id" gorm:"uniqueIndex"`
	MonitorID          uint64    `json:"monitor_id" gorm:"index"`
	RequestID          string    `json:"request_id" gorm:"size:32"`
	DeliveryID         string    `json:"delivery_id,omitempty" gorm:"size:32"`
	BaselineDeliveryID string    `json:"baseline_delivery_id,omitempty" gorm:"size:32"`
	Outcome            string    `json:"outcome" gorm:"size:32"`
	Status             string    `json:"status" gorm:"size:16"`
	Summary            string    `json:"summary" gorm:"type:text"`
	ComparisonJSON     string    `json:"-" gorm:"type:longtext"`
	CreatedAt          time.Time `json:"created_at"`
}

func (MonitorInbox) TableName() string { return "information_monitor_inbox" }

func (m Monitor) Config() (MonitorConfig, error) {
	var config MonitorConfig
	err := json.Unmarshal([]byte(m.ConfigJSON), &config)
	return config, err
}

func normalizeMonitor(config *MonitorConfig) error {
	config.Goal, config.Focus = strings.TrimSpace(config.Goal), strings.TrimSpace(config.Focus)
	scope, err := normalizeSourceScope(config.SourceScope)
	if err != nil {
		return businessError("INVALID_SOURCE_SCOPE", "monitor", err.Error(), "在 source_scope 中填写纯域名，或留空允许自主寻找来源")
	}
	config.SourceScope = scope
	if config.RequestKey == "" || len(config.RequestKey) > 128 {
		return businessError("MONITOR_REQUEST_KEY_REQUIRED", "monitor", "需要 request_key（最多 128 字节）", "为这次创建使用稳定的请求标识；重复提交沿用同一标识")
	}
	if config.Goal == "" {
		return businessError("MONITOR_GOAL_REQUIRED", "monitor", "缺少持续关注目标", "填写希望持续检查的目标，或选择已保存交付物作为基线")
	}
	if config.IntervalSeconds < 1 || config.IntervalSeconds > int64(^uint64(0)>>1)/int64(time.Second) {
		return businessError("INVALID_MONITOR_INTERVAL", "monitor", "检查间隔必须是可表示的正整数秒", "填写自己需要的检查频率；不需要周期执行时先保持暂停，手动检查仍可用")
	}
	return nil
}

func (s *Service) CreateMonitor(ctx context.Context, config MonitorConfig) (Monitor, error) {
	var result Monitor
	if config.DeliveryID != "" {
		delivery, _, err := s.SavedDelivery(ctx, config.DeliveryID)
		if err != nil {
			return result, err
		}
		request, err := s.Request(ctx, delivery.RequestID)
		if err != nil {
			return result, err
		}
		var input Input
		if err := json.Unmarshal([]byte(request.InputJSON), &input); err != nil {
			return result, err
		}
		if config.Goal == "" {
			config.Goal = input.Goal
		}
		if config.SourceScope == nil {
			config.SourceScope = input.SourceScope
		}
	}
	if err := normalizeMonitor(&config); err != nil {
		return result, err
	}
	raw := string(mustMarshal(config))
	result = Monitor{RequestKey: config.RequestKey, Goal: config.Goal, ConfigJSON: raw, BaselineDeliveryID: config.DeliveryID, Version: 1}
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		created := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&result)
		if created.Error != nil {
			return created.Error
		}
		if created.RowsAffected != 0 {
			return nil
		}
		if err := tx.First(&result, "request_key = ?", config.RequestKey).Error; err != nil {
			return err
		}
		if result.ConfigJSON != raw {
			return agentdata.ErrConflict
		}
		return nil
	})
	return result, err
}

type MonitorUpdate struct {
	Config  *MonitorConfig `json:"config,omitempty"`
	Enabled *bool          `json:"enabled,omitempty"`
}

func (s *Service) UpdateMonitor(ctx context.Context, id uint64, input MonitorUpdate) (Monitor, error) {
	var result Monitor
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&result, id).Error; err != nil {
			return err
		}
		config, err := result.Config()
		if err != nil {
			return err
		}
		if input.Config != nil {
			config = *input.Config
			config.RequestKey = result.RequestKey
			if err := normalizeMonitor(&config); err != nil {
				return err
			}
			previous, err := result.Config()
			if err != nil {
				return err
			}
			if config.DeliveryID != previous.DeliveryID {
				if config.DeliveryID != "" {
					if _, _, err := s.SavedDelivery(ctx, config.DeliveryID); err != nil {
						return err
					}
				}
				result.BaselineDeliveryID = config.DeliveryID
			}
			result.Goal, result.ConfigJSON = config.Goal, string(mustMarshal(config))
			if result.ConfigJSON != string(mustMarshal(previous)) {
				result.Version++
			}
		}
		if input.Enabled != nil {
			result.Enabled = *input.Enabled
		}
		result.NextRunAt = nil
		if result.Enabled {
			next := time.Now().UTC().Add(time.Duration(config.IntervalSeconds) * time.Second)
			result.NextRunAt = &next
		}
		return tx.Save(&result).Error
	})
	return result, err
}

func (s *Service) RefreshMonitor(ctx context.Context, id uint64, key string) (Request, error) {
	return s.refreshMonitor(ctx, id, key, nil)
}

// A schedule slot creates one normal task. Eino owns all planning, acquisition,
// correction and delivery; monitoring only records the accepted result.
func (s *Service) refreshMonitor(ctx context.Context, id uint64, key string, due *time.Time) (Request, error) {
	var request Request
	if key == "" || len(key) > 128 {
		return request, businessError("MONITOR_REQUEST_KEY_REQUIRED", "monitor", "本次检查缺少稳定 request_key", "为本次检查生成一个标识；网络重试时沿用它，不重复创建任务")
	}
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var monitor Monitor
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&monitor, id).Error; err != nil {
			return err
		}
		var previous MonitorCheck
		err := tx.First(&previous, "request_key = ?", key).Error
		if err == nil {
			if previous.MonitorID != id {
				return agentdata.ErrConflict
			}
			return tx.First(&request, "id = ?", previous.RequestID).Error
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if due != nil && (!monitor.Enabled || monitor.NextRunAt == nil || !monitor.NextRunAt.Equal(*due)) {
			return agentdata.ErrConflict
		}
		if monitor.ActiveRequestID != "" {
			return businessError("MONITOR_CHECK_RUNNING", "monitor", "上一轮检查仍在进行", "查看 active_request_id 的真实进度，等待其交付或显式停止；不要重复提交相同检查")
		}
		config, err := monitor.Config()
		if err != nil {
			return err
		}
		local := *s
		local.DB = tx
		baseline := monitor.BaselineDeliveryID
		if baseline == "" {
			baseline = monitor.LastDeliveryID
		}
		check := MonitorCheck{MonitorID: id, RequestKey: key, BaselineDeliveryID: baseline, ConfigVersion: monitor.Version}
		if err := tx.Create(&check).Error; err != nil {
			return err
		}
		requestKey := fmt.Sprintf("monitor:%d:check:%d", id, check.ID)
		guidance := config.Focus
		if baseline != "" {
			guidance = "本次持续关注目标：" + config.Goal + "\n本轮重点：" + config.Focus
			request, err = local.CreateUpdate(ctx, baseline, UpdateInput{RequestKey: requestKey, Guidance: guidance, SourceScope: &config.SourceScope})
		} else {
			request, err = local.Create(ctx, Input{RequestKey: requestKey, Goal: config.Goal, Guidance: guidance, SourceScope: config.SourceScope, Refresh: true})
		}
		if err != nil {
			return err
		}
		if err := tx.Model(&check).Update("request_id", request.ID).Error; err != nil {
			return err
		}
		return tx.Model(&Monitor{}).Where("id = ?", id).Updates(map[string]any{"active_request_id": request.ID, "last_error": ""}).Error
	})
	return request, err
}

func (s *Service) ProcessMonitorCheck(ctx context.Context, requestID string) error {
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var link MonitorCheck
		if err := tx.First(&link, "request_id = ?", requestID).Error; err != nil {
			return err
		}
		var monitor Monitor
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&monitor, link.MonitorID).Error; err != nil {
			return err
		}
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&link, link.ID).Error; err != nil {
			return err
		}
		if link.ProcessedAt != nil {
			return nil
		}
		var request Request
		if err := tx.First(&request, "id = ?", requestID).Error; err != nil {
			return err
		}
		if !terminal(request.Status) {
			return nil
		}
		now := time.Now().UTC()
		link.ProcessedAt, link.Outcome, link.LastError = &now, request.Status, request.LastError
		item := MonitorInbox{CheckID: link.ID, MonitorID: monitor.ID, RequestID: request.ID, BaselineDeliveryID: link.BaselineDeliveryID, Outcome: request.Status, Status: "unread", Summary: request.LastError}
		local := *s
		local.DB = tx
		if request.CurrentDeliveryID != "" {
			_, draft, err := local.SavedDelivery(ctx, request.CurrentDeliveryID)
			if err != nil {
				return err
			}
			link.DeliveryID, link.Outcome = request.CurrentDeliveryID, draft.Outcome
			item.DeliveryID, item.Outcome = link.DeliveryID, draft.Outcome
			item.Summary = "本轮检查已完成，结果和原文已保存。"
			if draft.Outcome == "partial" {
				item.Summary = "本轮部分完成：" + strings.Join(draft.Gaps, "；")
			}
			if link.BaselineDeliveryID != "" {
				comparison, err := local.CompareDeliveries(ctx, link.BaselineDeliveryID, link.DeliveryID)
				if err != nil {
					return err
				}
				item.ComparisonJSON = string(mustMarshal(comparison))
				item.Summary += fmt.Sprintf(" 与基线相比记录了 %d 项变化。", len(comparison.Changes))
			}
			monitor.LastDeliveryID = link.DeliveryID
			if draft.Outcome != "partial" && monitor.Version == link.ConfigVersion {
				monitor.BaselineDeliveryID = link.DeliveryID
			}
		}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&item).Error; err != nil {
			return err
		}
		if err := tx.Save(&link).Error; err != nil {
			return err
		}
		monitor.LastRequestID, monitor.LastError = request.ID, request.LastError
		if monitor.ActiveRequestID == request.ID {
			monitor.ActiveRequestID = ""
		}
		config, err := monitor.Config()
		if err != nil {
			return err
		}
		monitor.NextRunAt = nil
		if monitor.Enabled {
			next := now.Add(time.Duration(config.IntervalSeconds) * time.Second)
			monitor.NextRunAt = &next
		}
		return tx.Save(&monitor).Error
	})
}

func (s *Service) SweepMonitors(ctx context.Context) error {
	var completed []MonitorCheck
	if err := s.DB.WithContext(ctx).Model(&MonitorCheck{}).Joins("JOIN information_requests r ON r.id = information_monitor_checks.request_id").Where("processed_at IS NULL AND r.status IN ?", terminalStages()).Select("information_monitor_checks.*").Order("information_monitor_checks.id").Find(&completed).Error; err != nil {
		return err
	}
	var failures []error
	for _, check := range completed {
		if err := s.ProcessMonitorCheck(ctx, check.RequestID); err != nil {
			failures = append(failures, err)
		}
	}
	var due []Monitor
	if err := s.DB.WithContext(ctx).Where("enabled = ? AND next_run_at <= ? AND active_request_id = ?", true, time.Now().UTC(), "").Order("next_run_at, id").Find(&due).Error; err != nil {
		return errors.Join(append(failures, err)...)
	}
	for _, monitor := range due {
		key := fmt.Sprintf("schedule:%d:%s", monitor.ID, monitor.NextRunAt.UTC().Format(time.RFC3339Nano))
		if _, err := s.refreshMonitor(ctx, monitor.ID, key, monitor.NextRunAt); err != nil && !errors.Is(err, agentdata.ErrConflict) {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
