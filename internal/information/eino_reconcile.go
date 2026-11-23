package information

import (
	"context"
	"encoding/json"
	"fmt"
	"go-lua-crawler/internal/agentstatus"
	"strings"
)

// restoreDomainAction reads the underlying durable receipt after a process died
// between the physical commit and Eino tool-result commit. Both are stored in
// the same admitted receipt; restoration never creates another admission.
func (h *informationHost) restoreDomainAction(ctx context.Context, name, raw string, admitted ...Step) (any, *informationWait, error, bool) {
	// Invalid advertised arguments cannot have reached a domain executor.
	if err := validateInformationToolArgs(name, raw); err != nil {
		return nil, nil, err, true
	}
	if len(admitted) != 1 {
		return nil, nil, nil, false
	}
	step := admitted[0]
	switch name {
	case "update_plan", "revise_requirement_interpretation", "read_materials", "submit_delivery", "ask_user":
		data, wait, err := h.dispatch(ctx, name, raw, step)
		return data, wait, err, true
	case "discover_sources":
		if step.ExecutionKind != "search_sources" {
			data, wait, err := h.dispatch(ctx, name, raw, step)
			return data, wait, err, true
		}
	case "acquire_sources":
		if step.ExecutionKind == "" {
			var args informationAcquireArgs
			if err := decodeInformationArgs(raw, &args); err != nil {
				return nil, nil, err, true
			}
			if args.AssetID == "" && (args.InspectURL != "" || len(args.InspectURLs) > 0) {
				// Each URL has its own durable admission. Completed inspections can
				// be read again without issuing another HTTP request.
				data, wait, err := h.dispatch(ctx, name, raw, step)
				return data, wait, err, true
			}
		}
	}
	if step.ExecutionResultJSON == "" {
		return nil, nil, nil, false
	}
	domain := executionResult(step)
	cause := restoredDomainFailure(domain)
	switch step.ExecutionKind {
	case "execute_asset", "execute_browser":
		var args informationAcquireArgs
		if err := decodeInformationArgs(raw, &args); err != nil {
			return nil, nil, err, true
		}
		var saved map[string]any
		decoder := json.NewDecoder(strings.NewReader(domain.ResultJSON))
		decoder.UseNumber()
		if err := decoder.Decode(&saved); err != nil {
			return nil, nil, err, true
		}
		if saved == nil {
			saved = map[string]any{}
		}
		return map[string]any{"asset_id": args.AssetID, "records": saved["records"]}, nil, cause, true
	case "trial_api", "trial_browser":
		if cause == nil {
			var trial TrialResult
			if err := json.Unmarshal([]byte(domain.ResultJSON), &trial); err != nil {
				return nil, nil, err, true
			}
			return acquisitionTrialSummary(trial), nil, nil, true
		}
	}
	return validRaw(domain.ResultJSON), nil, cause, true
}

func restoredDomainFailure(step Step) error {
	if step.Error == "" && step.Stage != "failed" {
		return nil
	}
	var payload struct {
		Failure *ActionError     `json:"failure"`
		Status  agentstatus.Code `json:"failure_status"`
	}
	decoder := json.NewDecoder(strings.NewReader(step.ResultJSON))
	decoder.UseNumber()
	if decoder.Decode(&payload) == nil && payload.Failure != nil {
		if payload.Status != 0 {
			return agentstatus.Wrap(payload.Status, payload.Failure, payload.Failure.Steps...)
		}
		return payload.Failure
	}
	return actionFailure("SAVED_ACTION_FAILED", fmt.Errorf("%s", step.Error), "Read the saved domain receipt and correct real parameters from its source evidence; unchanged execution is not retried.")
}
