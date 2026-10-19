package aiapp

import (
	"context"
	"fmt"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/knowledge"
	"gorm.io/gorm/clause"
	"strings"
	"time"
)

type HumanIntervention struct {
	ID        uint64    `json:"id" gorm:"primaryKey"`
	RunID     string    `json:"run_id" gorm:"size:32;uniqueIndex:human_event"`
	Key       string    `json:"key" gorm:"size:128;uniqueIndex:human_event"`
	Kind      string    `json:"kind" gorm:"size:32"`
	Reason    string    `json:"reason" gorm:"type:text"`
	CreatedAt time.Time `json:"created_at"`
}

type InterventionAssessment struct {
	Level         string         `json:"level"`
	EventsByLevel map[string]int `json:"events_by_level"`
}

// Classify receipts, not model claims. Missing receipts cannot establish L0.
// Approvals record consent only; this does not authorize execution or relax checks.
func AssessInterventions(accounting *RunAccounting) InterventionAssessment {
	out := InterventionAssessment{Level: "unknown", EventsByLevel: map[string]int{"L1": 0, "L2": 0, "L3": 0, "unknown": 0}}
	if accounting == nil || accounting.Interventions == nil {
		return out
	}
	out.Level = "L0"
	highest := 0
	for _, event := range accounting.Interventions {
		level, rank := "unknown", 0
		switch event.Kind {
		case "approval":
			level, rank = "L1", 1
		case "source_hint", "scope_feedback":
			level, rank = "L2", 2
		case "manual_code", "manual_registration", "expanded_scope", "manual_recovery":
			level, rank = "L3", 3
		}
		out.EventsByLevel[level]++
		if rank > highest {
			highest = rank
			out.Level = level
		}
	}
	if out.EventsByLevel["unknown"] > 0 {
		out.Level = "unknown"
	}
	return out
}

type RunAccounting struct {
	AsOf                   time.Time                `json:"as_of"`
	IndexAttempts          []knowledge.IndexAttempt `json:"index_attempts"`
	IndexWork              []knowledge.IndexWork    `json:"index_work"`
	Interventions          []HumanIntervention      `json:"interventions"`
	Replacements           []RecordReplacement      `json:"record_replacements"`
	Experiment             ExperimentControls       `json:"experiment"`
	IndexSettled           bool                     `json:"index_settled"`
	InterventionAssessment InterventionAssessment   `json:"intervention_assessment"`
}

func (s *Service) Accounting(ctx context.Context, r agentdata.Run) (RunAccounting, error) {
	out := RunAccounting{AsOf: time.Now(), IndexWork: []knowledge.IndexWork{}, Interventions: []HumanIntervention{}, Experiment: experimentControls(r), IndexSettled: true}
	var err error
	out.IndexAttempts, err = s.Knowledge.IndexAttemptsForRun(ctx, r.ID)
	if err != nil {
		return out, err
	}
	if err = s.Data.DB.WithContext(ctx).Where("origin_run_id = ?", r.ID).Order("id").Find(&out.IndexWork).Error; err != nil {
		return out, err
	}
	for _, work := range out.IndexWork {
		if work.Status != "ready" && work.Status != "superseded" {
			out.IndexSettled = false
		}
	}
	if err = s.Data.DB.WithContext(ctx).Where("run_id = ?", r.ID).Order("id").Find(&out.Interventions).Error; err != nil {
		return out, err
	}
	out.Replacements, err = s.replacements(ctx, r.ID)
	out.InterventionAssessment = AssessInterventions(&out)
	return out, err
}
func (s *Service) RecordIntervention(ctx context.Context, r agentdata.Run, event HumanIntervention) (HumanIntervention, error) {
	switch event.Kind {
	case "approval", "source_hint", "scope_feedback", "manual_code", "manual_registration", "expanded_scope", "manual_recovery":
	default:
		return event, fmt.Errorf("unknown material intervention kind")
	}
	if event.Key == "" || len(event.Key) > 128 || strings.TrimSpace(event.Reason) == "" || len(event.Reason) > 2000 {
		return event, fmt.Errorf("intervention key and reason required")
	}
	event.ID = 0
	event.RunID = r.ID
	event.CreatedAt = time.Now()
	q := s.Data.DB.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&event)
	if q.Error != nil {
		return event, q.Error
	}
	if q.RowsAffected == 0 {
		var prior HumanIntervention
		if err := s.Data.DB.WithContext(ctx).Where("run_id = ? AND `key` = ?", r.ID, event.Key).First(&prior).Error; err != nil {
			return prior, err
		}
		if prior.Kind != event.Kind || prior.Reason != event.Reason {
			return prior, agentdata.ErrConflict
		}
		event = prior
	}
	return event, nil
}
