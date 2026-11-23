package agentbench

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/aiapp"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type ClientConfig struct {
	Endpoint, Token, Batch, Variant string
	Repetitions                     int
	PollInterval, Timeout           time.Duration
}
type runView struct {
	Run        agentdata.Run         `json:"run"`
	Result     aiapp.TaskResult      `json:"result"`
	Operations []agentdata.Operation `json:"operations"`
	Loop       aiapp.LoopReport      `json:"loop"`
	Accounting *aiapp.RunAccounting  `json:"accounting"`
}

// Read-only reconciliation after asynchronous indexing settles. Gold judgment,
// original latency and failure status remain frozen; no paid retry is dispatched.
func RefreshAccounting(ctx context.Context, cfg ClientConfig, rows []Trial) ([]Trial, error) {
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") || u.RawQuery != "" || u.Fragment != "" || cfg.Token == "" {
		return nil, fmt.Errorf("authenticated HTTP(S) endpoint required")
	}
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return fmt.Errorf("redirect disabled") }}
	out := make([]Trial, 0, len(rows))
	for _, row := range rows {
		if row.RequestID != "" {
			var view informationView
			if _, err := requestJSON(ctx, client, cfg, "GET", "/api/v1/information/requests/"+url.PathEscape(row.RequestID), nil, &view); err != nil {
				return out, err
			}
			if view.Request.ID != row.RequestID {
				return out, fmt.Errorf("information request identity mismatch")
			}
			applyInformationAccounting(&row, view.Accounting)
			out = append(out, row)
			continue
		}
		var view runView
		if _, err := requestJSON(ctx, client, cfg, "GET", "/api/v1/agent/runs/"+url.PathEscape(row.RunID), nil, &view); err != nil {
			return out, err
		}
		if view.Run.ID != row.RunID {
			return out, fmt.Errorf("run identity mismatch")
		}
		row.Accounting = view.Accounting
		row.InterventionAssessment = aiapp.AssessInterventions(view.Accounting)
		row.Operations = view.Operations
		if view.Accounting != nil {
			row.HumanInterventions = len(view.Accounting.Interventions)
		}
		out = append(out, row)
	}
	return out, nil
}

func requestJSON(ctx context.Context, client *http.Client, cfg ClientConfig, method, path string, in any, out any) (json.RawMessage, error) {
	var payload []byte
	if in != nil {
		var err error
		payload, err = json.Marshal(in)
		if err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(cfg.Endpoint, "/")+path, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	response, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("benchmark API %s returned %d", path, response.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(raw, out)
	return raw, err
}

// Execute uses the real application's API, not an evaluator-owned action loop.
// Reusing a batch/case/repetition gives the same request key after interruption.
func Execute(ctx context.Context, cfg ClientConfig, m Manifest, gold map[string]GoldCase, emit func(Trial) error) ([]Trial, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("invalid benchmark endpoint")
	}
	if cfg.Token == "" || cfg.Batch == "" || cfg.Variant == "" || cfg.Repetitions < 1 || cfg.Repetitions > 20 {
		return nil, fmt.Errorf("token/batch/variant and 1..20 repetitions required")
	}
	if cfg.Repetitions > 1 && m.RepetitionPolicy != "warm_reuse" {
		return nil, fmt.Errorf("multiple repetitions on one endpoint require explicit warm_reuse; independent cold states need separate deployments/manifests")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 16 * time.Minute
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = time.Second
	}
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return fmt.Errorf("benchmark redirects disabled") }}
	// Cold admission may run the configured inference readiness probe (CLI up
	// to 90s plus login). Polling keeps its short timeout; no automatic retry.
	submitClient := *client
	submitClient.Timeout = 2 * time.Minute
	if m.EntryPoint == "information" {
		return executeInformation(ctx, cfg, m, gold, emit, client, &submitClient)
	}
	if m.ExpectedActionMode != "" {
		var status struct {
			ActionMode string `json:"action_mode"`
		}
		if _, err := requestJSON(ctx, client, cfg, "GET", "/api/v1/agent/status", nil, &status); err != nil {
			return nil, err
		}
		if status.ActionMode != m.ExpectedActionMode {
			return nil, fmt.Errorf("application action mode differs from frozen manifest; no task submitted")
		}
	}
	rows := []Trial{}
	for _, c := range m.Cases {
		for rep := 1; rep <= cfg.Repetitions; rep++ {
			key := fmt.Sprintf("bench:%s:%s:%s:%d", cfg.Batch, cfg.Variant, c.ID, rep)
			if len(key) > 128 {
				return rows, fmt.Errorf("benchmark request key exceeds 128 bytes")
			}
			trial := Trial{CaseID: c.ID, Repetition: rep, Mode: m.Mode, EnvironmentID: m.EnvironmentID, Variant: cfg.Variant, Split: c.Split, Solvable: c.Solvable, RepetitionPolicy: m.RepetitionPolicy, GoldVerification: gold[c.ID].Verification, Reasons: []string{}}
			var created agentdata.Run
			submissionStart := time.Now()
			_, err := requestJSON(ctx, &submitClient, cfg, "POST", "/api/v1/agent/runs", PublicRequest(c, key), &created)
			if err != nil {
				trial.Status = "submission_failed"
				trial.DurationMS = time.Since(submissionStart).Milliseconds()
				trial.Reasons = append(trial.Reasons, err.Error())
				trial.InterventionAssessment = aiapp.AssessInterventions(nil)
				rows = append(rows, trial)
				if emit != nil {
					if emitErr := emit(trial); emitErr != nil {
						return rows, emitErr
					}
				}
				return rows, err
			}
			trial.RunID = created.ID
			var createdInput aiapp.CreateRunRequest
			if err := json.Unmarshal([]byte(created.InputJSON), &createdInput); err == nil {
				trial.ApplicationActionMode = createdInput.ActionMode
			}
			if m.ExpectedActionMode != "" && trial.ApplicationActionMode != m.ExpectedActionMode {
				return rows, fmt.Errorf("application action mode differs from frozen manifest; stop before treating labels as A/B")
			}
			attemptCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
			start := time.Now()
			var view runView
			var raw json.RawMessage
			for {
				raw, err = requestJSON(attemptCtx, client, cfg, "GET", "/api/v1/agent/runs/"+url.PathEscape(created.ID), nil, &view)
				if err != nil {
					trial.Status = "observation_failed"
					trial.Reasons = append(trial.Reasons, err.Error())
					break
				}
				if view.Run.IsTerminal() || strings.HasPrefix(view.Run.Status, "blocked_") || view.Run.Status == "needs_input" {
					trial.Status = view.Run.Status
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
					trial.Status = "observation_timeout"
					trial.Reasons = append(trial.Reasons, err.Error())
					break
				}
			}
			cancel()
			trial.DurationMS = time.Since(start).Milliseconds()
			trial.PollIntervalMS = cfg.PollInterval.Milliseconds()
			if view.Run.IsTerminal() && !view.Run.CreatedAt.IsZero() && !view.Run.UpdatedAt.Before(view.Run.CreatedAt) {
				duration := view.Run.UpdatedAt.Sub(view.Run.CreatedAt).Milliseconds()
				trial.RunDurationMS = &duration
			}
			trial.Operations = view.Operations
			trial.Accounting = view.Accounting
			trial.InterventionAssessment = aiapp.AssessInterventions(view.Accounting)
			if view.Accounting != nil {
				trial.HumanInterventions = len(view.Accounting.Interventions)
			}
			trial.Loop = view.Loop
			trial.Snapshot = raw
			if view.Run.IsTerminal() {
				judgment := Judge(gold[c.ID], trial.Status, view.Result, time.Now())
				trial.Correct = judgment.Correct
				trial.FalseCompletion = judgment.FalseCompletion
				trial.MatchedFields = judgment.MatchedFields
				trial.RequiredFields = judgment.RequiredFields
				trial.DeliveredFields = judgment.DeliveredFields
				trial.FieldScored = judgment.FieldScored
				trial.FieldCoverage = judgment.FieldCoverage
				trial.FieldPrecision = judgment.FieldPrecision
				trial.Reasons = append(trial.Reasons, judgment.Reasons...)
			}
			rows = append(rows, trial)
			if emit != nil {
				if err := emit(trial); err != nil {
					return rows, err
				}
			}
			if strings.HasPrefix(trial.Status, "blocked_") || trial.Status == "observation_failed" || trial.Status == "observation_timeout" {
				return rows, nil
			}
		}
	}
	return rows, nil
}
