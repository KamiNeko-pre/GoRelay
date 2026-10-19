package agentbench

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/information"
)

type informationRequestView struct {
	information.Request
	Experiment *information.ExperimentControls `json:"experiment"`
}
type informationView struct {
	Request    informationRequestView         `json:"request"`
	Result     aiapp.TaskResult               `json:"result"`
	Accounting *information.RequestAccounting `json:"accounting"`
}

func applyInformationAccounting(row *Trial, accounting *information.RequestAccounting) {
	row.InformationAccounting = accounting
	row.Accounting = nil
	row.Operations = nil
	if accounting != nil {
		row.RunID = accounting.ChildRunID
		row.Operations = accounting.ChildOperations
		row.Accounting = accounting.RunAccounting
	}
	row.InterventionAssessment = aiapp.AssessInterventions(row.Accounting)
	if row.Accounting != nil {
		row.HumanInterventions = len(row.Accounting.Interventions)
	}
}

func informationTerminal(stage string) bool {
	return stage == "completed" || stage == "failed" || stage == "cancelled" || stage == "expired"
}

// Only the application's normal request API drives work. The evaluator never
// supplies selectors, code, routes, action sequences or private expected facts.
func executeInformation(ctx context.Context, cfg ClientConfig, m Manifest, gold map[string]GoldCase, emit func(Trial) error, client, submitClient *http.Client) ([]Trial, error) {
	if m.ExpectedInformationPolicy == nil {
		return nil, fmt.Errorf("freeze expected_information_policy before an information experiment")
	}
	if m.ExpectedActionMode != "" && m.ExpectedActionMode != m.ExpectedInformationPolicy.ActionMode {
		return nil, fmt.Errorf("conflicting action mode expectations")
	}
	var status struct {
		Policy information.ExperimentPolicy `json:"experiment_policy"`
	}
	if _, err := requestJSON(ctx, client, cfg, "GET", "/api/v1/information/status", nil, &status); err != nil {
		return nil, err
	}
	if status.Policy != *m.ExpectedInformationPolicy {
		return nil, fmt.Errorf("server information policy differs from frozen manifest; no request submitted")
	}
	rows := []Trial{}
	for _, c := range m.Cases {
		for rep := 1; rep <= cfg.Repetitions; rep++ {
			key := fmt.Sprintf("bench:%s:%s:%s:%d", cfg.Batch, cfg.Variant, c.ID, rep)
			if len(key) > 128 {
				return rows, fmt.Errorf("benchmark request key exceeds 128 bytes")
			}
			row := Trial{CaseID: c.ID, Repetition: rep, Mode: m.Mode, EnvironmentID: m.EnvironmentID, Variant: cfg.Variant, Split: c.Split, Solvable: c.Solvable, RepetitionPolicy: m.RepetitionPolicy, GoldVerification: gold[c.ID].Verification, Reasons: []string{}}
			in := information.Input{RequestKey: key, Goal: c.Goal, SourceScope: c.SourceScope, EntryURLs: c.EntryURLs, TimeoutSeconds: c.TimeoutSeconds}
			start := time.Now()
			var created struct {
				Request informationRequestView `json:"request"`
			}
			_, err := requestJSON(ctx, submitClient, cfg, "POST", "/api/v1/information/requests", in, &created)
			if err != nil {
				row.Status = "submission_failed"
				row.Reasons = append(row.Reasons, err.Error())
			} else if created.Request.ID == "" {
				err = fmt.Errorf("missing information request identity")
				row.Status = "submission_failed"
				row.Reasons = append(row.Reasons, err.Error())
			}
			row.RequestID = created.Request.ID
			if err == nil && (created.Request.Experiment == nil || created.Request.Experiment.ExperimentPolicy != *m.ExpectedInformationPolicy) {
				err = fmt.Errorf("persisted information policy differs from frozen manifest")
				row.Status = "invalid_configuration"
				row.Reasons = append(row.Reasons, err.Error())
			}
			if created.Request.Experiment != nil {
				row.ApplicationActionMode = created.Request.Experiment.ActionMode
			}
			var view informationView
			var raw json.RawMessage
			if err == nil {
				attemptCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
				for {
					raw, err = requestJSON(attemptCtx, client, cfg, "GET", "/api/v1/information/requests/"+url.PathEscape(row.RequestID), nil, &view)
					if err != nil {
						row.Status = "observation_failed"
						row.Reasons = append(row.Reasons, err.Error())
						break
					}
					if view.Request.ID != row.RequestID {
						err = fmt.Errorf("information request identity mismatch")
						row.Status = "observation_failed"
						row.Reasons = append(row.Reasons, err.Error())
						break
					}
					if view.Accounting == nil || view.Accounting.RequestID != row.RequestID || view.Accounting.Experiment == nil || view.Accounting.Experiment.ExperimentPolicy != *m.ExpectedInformationPolicy {
						err = fmt.Errorf("missing or mismatched information accounting/configuration receipt")
						row.Status = "invalid_configuration"
						row.Reasons = append(row.Reasons, err.Error())
						break
					}
					if a := view.Accounting; a.ChildRunID != "" && (a.RunAccounting == nil || a.ChildActionMode != m.ExpectedInformationPolicy.ActionMode || a.RunAccounting.Experiment.DisableCapabilityReuse != m.ExpectedInformationPolicy.DisableCapabilityReuse || a.RunAccounting.Experiment.DisableKnowledgeReuse != m.ExpectedInformationPolicy.DisableKnowledgeReuse) {
						err = fmt.Errorf("child crawler policy differs from frozen information policy")
						row.Status = "invalid_configuration"
						row.Reasons = append(row.Reasons, err.Error())
						break
					}
					if informationTerminal(view.Request.Stage) {
						row.Status = view.Request.Stage
						break
					}
					timer := time.NewTimer(cfg.PollInterval)
					select {
					case <-attemptCtx.Done():
						timer.Stop()
						err = attemptCtx.Err()
					case <-timer.C:
					}
					if err != nil {
						row.Status = "observation_timeout"
						row.Reasons = append(row.Reasons, err.Error())
						break
					}
				}
				cancel()
			}
			row.DurationMS = time.Since(start).Milliseconds()
			row.PollIntervalMS = cfg.PollInterval.Milliseconds()
			row.Snapshot = raw
			applyInformationAccounting(&row, view.Accounting)
			if informationTerminal(view.Request.Stage) && err == nil {
				if !view.Request.CreatedAt.IsZero() && !view.Request.UpdatedAt.Before(view.Request.CreatedAt) {
					duration := view.Request.UpdatedAt.Sub(view.Request.CreatedAt).Milliseconds()
					row.RunDurationMS = &duration
				}
				j := Judge(gold[c.ID], row.Status, view.Result, time.Now())
				row.Correct, row.FalseCompletion, row.Reasons = j.Correct, j.FalseCompletion, append(row.Reasons, j.Reasons...)
				row.MatchedFields, row.RequiredFields, row.DeliveredFields, row.FieldScored = j.MatchedFields, j.RequiredFields, j.DeliveredFields, j.FieldScored
				row.FieldCoverage, row.FieldPrecision = j.FieldCoverage, j.FieldPrecision
			}
			rows = append(rows, row)
			if emit != nil {
				if save := emit(row); save != nil {
					return rows, save
				}
			}
			if err != nil {
				if row.Status == "invalid_configuration" && row.RequestID != "" && !informationTerminal(view.Request.Stage) {
					var cancelled json.RawMessage
					if _, cause := requestJSON(ctx, client, cfg, "POST", "/api/v1/information/requests/"+url.PathEscape(row.RequestID)+"/cancel", nil, &cancelled); cause != nil {
						return rows, fmt.Errorf("%w; cancel invalid experiment: %v", err, cause)
					}
				}
				return rows, err
			}
		}
	}
	return rows, nil
}
