package aiapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/knowledge"
	"go-lua-crawler/internal/recorddiff"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"strings"
	"time"
)

// The original Run/results retain historical observations. This row is the
// latest validated view used for comparisons; runtime IDs never identify entities.
type MonitorSnapshot struct {
	ID        uint64          `json:"id" gorm:"primaryKey;autoIncrement"`
	MonitorID uint64          `json:"monitor_id" gorm:"uniqueIndex:monitor_entity"`
	KeyHash   string          `json:"key_hash" gorm:"size:64;uniqueIndex:monitor_entity"`
	Entity    string          `json:"entity" gorm:"type:text"`
	RunID     string          `json:"run_id" gorm:"size:32"`
	Fields    json.RawMessage `json:"fields" gorm:"type:json"`
	Sources   json.RawMessage `json:"sources" gorm:"type:json"`
	Matched   bool            `json:"matched"`
	Episode   uint64          `json:"episode"`
	ValueHash string          `json:"value_hash" gorm:"size:64"`
	CheckedAt time.Time       `json:"checked_at"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

func (MonitorSnapshot) TableName() string { return "agent_monitor_snapshots" }

type MonitorInbox struct {
	ID         uint64          `json:"id" gorm:"primaryKey;autoIncrement"`
	MonitorID  uint64          `json:"monitor_id" gorm:"index:monitor_inbox"`
	EventKey   string          `json:"event_key" gorm:"size:64;uniqueIndex"`
	KeyHash    string          `json:"key_hash" gorm:"size:64;index"`
	RunID      string          `json:"run_id" gorm:"size:32"`
	Fields     json.RawMessage `json:"fields" gorm:"type:json"`
	Sources    json.RawMessage `json:"sources" gorm:"type:json"`
	Changes    json.RawMessage `json:"changes" gorm:"type:json"`
	Status     string          `json:"status" gorm:"size:16"`
	Active     bool            `json:"active"`
	Verified   bool            `json:"verified"`
	ValidUntil *time.Time      `json:"valid_until,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
	UpdatedAt  time.Time       `json:"updated_at"`
}

func (MonitorInbox) TableName() string { return "agent_monitor_inbox" }

func (i MonitorInbox) Current(now time.Time) bool {
	return i.Verified && (i.ValidUntil == nil || i.ValidUntil.After(now))
}

func (s *Service) MarkMonitorInbox(ctx context.Context, id uint64, status, observedRunID string) (MonitorInbox, error) {
	var out MonitorInbox
	if (status != "read" && status != "ignored") || observedRunID == "" {
		return out, fmt.Errorf("status and observed_run_id required")
	}
	if err := s.Data.DB.WithContext(ctx).First(&out, id).Error; err != nil {
		return out, err
	}
	err := s.Data.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var m Monitor
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&m, out.MonitorID).Error; err != nil {
			return err
		}
		if err := tx.First(&out, id).Error; err != nil {
			return err
		}
		if out.RunID != observedRunID {
			return agentdata.ErrConflict
		}
		return tx.Model(&out).Update("status", status).Error
	})
	out.Status = status
	out.Verified = out.Current(time.Now().UTC())
	return out, err
}

type monitorObservation struct {
	Key    string
	Entity string
	Row    MergedRecord
}

// Whole-result coverage is insufficient: a partial result can contain valid
// entities alongside missing ones. Validate each identity and its field evidence.
func monitorValidatedRows(spec TaskSpec, result TaskResult, now time.Time) ([]monitorObservation, []string, error) {
	keys := spec.JoinKeys
	if len(keys) == 0 && spec.EntityField != "" {
		keys = []string{spec.EntityField}
	}
	if len(keys) == 0 {
		return nil, nil, fmt.Errorf("monitor requires explicit business identity fields")
	}
	counts := map[string]int{}
	conflict := map[string]bool{}
	for _, c := range result.Conflicts {
		conflict[c.JoinKey] = true
	}
	for _, row := range result.Records {
		key, e := recorddiff.BusinessKey(row.Fields, keys)
		if e == nil {
			counts[key]++
		}
	}
	out := []monitorObservation{}
	issues := []string{}
	for _, row := range result.Records {
		key, err := recorddiff.BusinessKey(row.Fields, keys)
		if err != nil {
			issues = append(issues, err.Error())
			continue
		}
		if counts[key] != 1 || conflict[key] {
			issues = append(issues, "ambiguous business identity: "+key)
			continue
		}
		entity, _ := row.Fields[spec.EntityField].(string)
		if len(spec.Entities) > 0 {
			found := false
			for _, v := range spec.Entities {
				found = found || v == entity
			}
			if !found {
				issues = append(issues, "unexpected entity: "+entity)
				continue
			}
		}
		one := spec
		one.Entities = nil
		facts, err := EvaluateMergedRecords(one, MergeResult{Records: []MergedRecord{row}}, now)
		if err != nil {
			return nil, nil, err
		}
		if facts.ValidRecords != 1 {
			issues = append(issues, "incomplete or stale fields: "+key)
			continue
		}
		// Contract fields drive changes. Extra debug/context fields do not.
		fields := map[string]any{}
		for _, f := range spec.RequiredFields {
			fields[f.Name] = row.Fields[f.Name]
		}
		row.Fields = fields
		out = append(out, monitorObservation{Key: key, Entity: entity, Row: row})
	}
	return out, issues, nil
}

func monitorValidUntil(spec TaskSpec, row MergedRecord, now time.Time) *time.Time {
	if spec.MaxAgeSeconds == 0 {
		return nil
	}
	var earliest time.Time
	for _, f := range spec.RequiredFields {
		var latest time.Time
		fieldSpec := spec
		fieldSpec.Entities = nil
		fieldSpec.EntityField = ""
		fieldSpec.JoinKeys = nil
		fieldSpec.RequiredFields = []FieldRequirement{f}
		for _, p := range row.FieldSources[f.Name] {
			facts, err := EvaluateRecords(fieldSpec, []Record{{Key: row.Keys[0], SchemaVersion: row.SchemaVersion, Fields: row.Fields, Provenance: p}}, now)
			if err == nil && facts.ValidRecords == 1 && p.FetchedAt.After(latest) {
				latest = p.FetchedAt
			}
		}
		until := latest.Add(time.Duration(spec.MaxAgeSeconds) * time.Second)
		if earliest.IsZero() || until.Before(earliest) {
			earliest = until
		}
	}
	return &earliest
}

// Run completion and monitor processing are separate durable steps. A crash
// after terminal commit is recovered by the next sweep, using the same check.
func (s *Service) ProcessMonitorCheck(ctx context.Context, runID string) error {
	var link MonitorCheck
	if err := s.Data.DB.WithContext(ctx).Where("run_id = ?", runID).First(&link).Error; err != nil {
		return err
	}
	return s.Data.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var m Monitor
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&m, link.MonitorID).Error; err != nil {
			return err
		}
		if err := tx.First(&link, link.ID).Error; err != nil {
			return err
		}
		if link.ProcessedAt != nil {
			return nil
		}
		if m.ActiveRunID != runID || m.Version != link.ConfigVersion {
			return agentdata.ErrConflict
		}
		owned := *s
		owned.Data = agentdata.NewStore(tx)
		owned.Knowledge = knowledge.NewStore(tx)
		r, err := owned.Data.Run(ctx, runID)
		if err != nil {
			return err
		}
		if !r.IsTerminal() {
			return agentdata.ErrConflict
		}
		config, err := m.Config()
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		issues := []string{}
		// Old opportunities remain historical, but are not confirmed by a failure.
		if err = tx.Model(&MonitorInbox{}).Where("monitor_id = ? AND active = ?", m.ID, true).Update("verified", false).Error; err != nil {
			return err
		}
		if r.Status == "completed" || r.Status == "partial" {
			spec, e := specOf(r)
			if e != nil {
				issues = append(issues, e.Error())
			} else {
				result, e := owned.Evaluate(ctx, r)
				if e != nil {
					issues = append(issues, e.Error())
				} else {
					observations, problems, e := monitorValidatedRows(spec, result, now)
					if e != nil {
						issues = append(issues, e.Error())
					} else {
						issues = append(issues, problems...)
						for _, o := range observations {
							if e = saveMonitorObservation(tx, m, link, config, spec, o, now); e != nil {
								return e
							}
						}
					}
					if !result.Coverage.Satisfied {
						issues = append(issues, "run has incomplete coverage")
					}
					if r.Status == "completed" && taskDeliverySatisfied(&result) {
						plan, e := owned.captureMonitorPlan(ctx, m, r)
						if e != nil {
							return e
						}
						m.PlanJSON = plan
						if config.Spec == nil && plan != "" {
							config.Spec = &spec
							if e = config.Normalize(); e != nil {
								return e
							}
							raw, e := json.Marshal(config)
							if e != nil {
								return e
							}
							m.ConfigJSON = string(raw)
						}
					}
				}
			}
		} else {
			issues = append(issues, "run "+r.Status+": "+r.LastError)
		}
		link.ProcessedAt = &now
		m.ActiveRunID = ""
		m.LastRunID = runID
		m.LastError = strings.Join(issues, "; ")
		if err = tx.Save(&link).Error; err != nil {
			return err
		}
		return tx.Save(&m).Error
	})
}

func saveMonitorObservation(tx *gorm.DB, m Monitor, check MonitorCheck, config CreateMonitorRequest, spec TaskSpec, o monitorObservation, now time.Time) error {
	match, err := MatchMonitorEligibility(spec, config, o.Entity, o.Row.Fields)
	if err != nil {
		return err
	}
	keyHash := agentdata.Hash(o.Key)
	valueHash := agentdata.Hash(o.Row.Fields)
	var prior MonitorSnapshot
	err = tx.Where("monitor_id = ? AND key_hash = ?", m.ID, keyHash).First(&prior).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	had := err == nil
	changed := !had || prior.ValueHash != valueHash
	var before []map[string]any
	if had {
		var old map[string]any
		if err = json.Unmarshal(prior.Fields, &old); err != nil {
			return err
		}
		before = append(before, old)
	}
	keys := spec.JoinKeys
	if len(keys) == 0 {
		keys = []string{spec.EntityField}
	}
	changes, err := recorddiff.Diff(before, []map[string]any{o.Row.Fields}, keys)
	if err != nil {
		return err
	}
	fields, _ := json.Marshal(o.Row.Fields)
	sources, _ := json.Marshal(o.Row.Sources)
	delta, _ := json.Marshal(changes)
	until := monitorValidUntil(spec, o.Row, now)
	var active MonitorInbox
	err = tx.Where("monitor_id = ? AND key_hash = ? AND active = ?", m.ID, keyHash, true).Order("id DESC").First(&active).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	hasItem := err == nil
	if !match {
		if err = tx.Model(&MonitorInbox{}).Where("monitor_id = ? AND key_hash = ? AND active = ?", m.ID, keyHash, true).Updates(map[string]any{"active": false, "verified": true, "valid_until": until, "fields": fields, "sources": sources, "changes": delta, "run_id": check.RunID}).Error; err != nil {
			return err
		}
	} else if hasItem && (!changed || active.Status == "unread" || active.Status == "ignored") {
		updates := map[string]any{"fields": fields, "sources": sources, "run_id": check.RunID, "verified": true, "valid_until": until}
		if changed {
			updates["changes"] = delta
		}
		if err = tx.Model(&active).Updates(updates).Error; err != nil {
			return err
		}
	} else if !had || !prior.Matched || changed {
		if hasItem {
			if err = tx.Model(&active).Update("active", false).Error; err != nil {
				return err
			}
		}
		item := MonitorInbox{MonitorID: m.ID, KeyHash: keyHash, EventKey: agentdata.Hash([]any{m.ID, keyHash, check.ID}), RunID: check.RunID, Fields: fields, Sources: sources, Changes: delta, Status: "unread", Active: true, Verified: true, ValidUntil: until}
		if err = tx.Create(&item).Error; err != nil {
			return err
		}
	}
	if match && (!had || !prior.Matched) {
		prior.Episode++
	}
	prior.MonitorID = m.ID
	prior.KeyHash = keyHash
	prior.Entity = o.Entity
	prior.RunID = check.RunID
	prior.Fields = fields
	prior.Sources = sources
	prior.Matched = match
	prior.ValueHash = valueHash
	prior.CheckedAt = now
	return tx.Save(&prior).Error
}
