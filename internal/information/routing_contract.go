package information

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"go-lua-crawler/internal/aiapp"
)

const nativeAPIExecutionContract = "One initial public GET endpoint and its observed next_pointer URL chain; no independent-endpoint fan-out or code transformations. Every trial validates the whole frozen output contract. Partial trials are not accumulated. Use the Lua route when these capabilities are required."

func decisionFields(action string) []string {
	switch action {
	case "inspect_api":
		return []string{"url"}
	case "execute_asset":
		return []string{"asset_id", "params", "url"}
	case "trial_api":
		return []string{"api"}
	case "trial_browser":
		return []string{"browser"}
	}
	return nil
}

func decisionHelp(stage string) string {
	switch stage {
	case "known_api", "select_api_candidate":
		return "Choose inspect_api with an absolute candidate URL in the root url field, or no_known_api. This stage selects a candidate; it does not accept an api plan or asset_id."
	case "plan_api":
		return "Choose trial_api with api={url,items_pointer,fields,next_pointer}, or api_unsuitable with the actual capability reason. " + nativeAPIExecutionContract
	case "choose_asset":
		return "Choose execute_asset with the exact eligible catalog id (not source_id) and its declared params, or no_matching_asset. The host uses the single current request entry URL as the execution target; set root url explicitly when selecting among multiple entries. Keep target url separate from params unless the asset schema itself declares it. The catalog version is already bound by the host. Admission inventories are not callable assets."
	case "plan_browser":
		return "Choose trial_browser with an observed browser plan, or browser_unsuitable with the actual reason. Existing host authorization is still required."
	}
	return "Return one call of this stage's decision tool using the offered action and declared arguments."
}

func validateRoutingDecision(r Request, out routeDecision, raw json.RawMessage, allowed map[string]bool) (string, any, any) {
	var in Input
	_ = json.Unmarshal([]byte(r.InputJSON), &in)
	switch out.Action {
	case "inspect_api":
		if !urlInScope(out.URL, in.SourceScope) {
			return "url", out.URL, "a required absolute public URL in the root url field and granted source_scope"
		}
	case "execute_asset":
		if out.AssetID == "" {
			return "asset_id", out.AssetID, "an eligible asset ID from observations"
		}
		if out.URL != "" && !urlInScope(out.URL, in.SourceScope) {
			return "url", out.URL, "an absolute execution target URL within source_scope; keep target url separate from declared params"
		}
	case "trial_api":
		if out.API == nil {
			return "api", nil, "an object containing url, items_pointer and nonempty fields"
		}
		if !urlInScope(out.API.URL, in.SourceScope) {
			return "api.url", out.API.URL, "an absolute URL within source_scope"
		}
		if len(out.API.Fields) == 0 {
			return "api.fields", out.API.Fields, "requested fields mapped to observed JSON pointers"
		}
	case "trial_browser":
		if out.Browser == nil {
			return "browser", nil, "an observed browser plan object"
		}
		if !urlInScope(out.Browser.URL, in.SourceScope) {
			return "browser.url", out.Browser.URL, "an absolute URL within source_scope"
		}
		if len(out.Browser.Fields) == 0 {
			return "browser.fields", nil, "requested fields mapped to observed locators"
		}
	}
	var args map[string]any
	_ = json.Unmarshal(raw, &args)
	keys := make([]string, 0, len(args))
	for key := range args {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if !allowed[key] {
			return key, args[key], "only parameters declared by the current stage"
		}
	}
	if strings.TrimSpace(out.Reason) == "" {
		return "reason", out.Reason, "a nonempty explanation based on observed evidence"
	}
	return "", nil, nil
}

func (s *Service) rejectRoutingArguments(ctx context.Context, r Request, stage string, raw json.RawMessage, field string, actual, expected any) error {
	var args map[string]any
	if json.Unmarshal(raw, &args) != nil || args == nil {
		args = map[string]any{"invalid_json": string(raw)}
	}
	delete(args, "reason")
	step, fresh, err := s.reserveStep(ctx, r, "route_arguments", map[string]any{"tool": stage, "arguments": args})
	if err != nil {
		return err
	}
	if !fresh {
		return duplicateStep(step)
	}
	failure := actionFailure("ROUTING_ARGUMENTS_INVALID", fmt.Errorf("%s parameter %s: actual=%v; expected=%v", stage, field, actual, expected), decisionHelp(stage), "Correct the named parameter using the saved observation and current schema. A corrected business argument is a new request; changing reason or call metadata is not.")
	failure.PriorStepID = step.ID
	failure.Details = map[string]any{"stage": stage, "field": field, "actual": actual, "expected": expected}
	if err := s.finishStep(context.WithoutCancel(ctx), step, map[string]any{"failure": failure}, failure); err != nil {
		return err
	}
	return failure
}

// The model sees eligible assets, not the inventory captured for ablations.
func routingObservation(payload any) any {
	switch p := payload.(type) {
	case *PreflightResult:
		if p == nil {
			return nil
		}
		copy := *p
		copy.Experiment = nil
		return copy
	case PreflightResult:
		p.Experiment = nil
		return p
	case map[string]any:
		copy := make(map[string]any, len(p))
		for k, v := range p {
			copy[k] = routingObservation(v)
		}
		return copy
	}
	return payload
}

func (s *Service) routingFailureFeedback(ctx context.Context, r Request, cause error, restored bool) string {
	feedback := map[string]any{"error": cause.Error(), "status": aiapp.OperationalStatus(cause)}
	var failure *ActionError
	if errors.As(cause, &failure) {
		feedback["diagnostic"] = failure
		if failure.PriorStepID > 0 {
			var prior Step
			if err := s.DB.WithContext(ctx).First(&prior, "id = ? AND request_id = ?", failure.PriorStepID, r.ID).Error; err != nil {
				feedback["prior_step_lookup_error"] = err.Error()
			} else {
				feedback["prior_step"] = map[string]any{"id": prior.ID, "kind": prior.Kind, "stage": prior.Stage, "input": validRaw(prior.InputJSON), "result": validRaw(prior.ResultJSON), "error": prior.Error}
			}
		}
	}
	if restored {
		feedback["restored_without_execution"] = true
	}
	return string(mustMarshal(feedback))
}

// The execution target belongs to the current request, not the tool parameter schema.
func publishedAssetTarget(in Input, asset Asset, selected routeDecision) (string, error) {
	target := selected.URL
	if target == "" && len(in.EntryURLs) == 1 {
		target = in.EntryURLs[0]
	}
	if target == "" && len(in.EntryURLs) == 0 {
		target = asset.EntryURL
	}
	if target == "" || !urlInScope(target, in.SourceScope) {
		failure := actionFailure("ROUTING_ARGUMENTS_INVALID", fmt.Errorf("execute_asset parameter url: an unambiguous absolute target within source_scope is required"), decisionHelp("choose_asset"), "Set root url to the current intended entry from goal_input.entry_urls (or another observed URL within source_scope), keep the exact catalog asset_id, and supply only schema-declared tool params. Do not add url or version to params unless its input_schema declares them.")
		failure.Details = map[string]any{"field": "url", "actual": target, "entry_urls": in.EntryURLs, "expected": "one explicit or unambiguous current target within source_scope"}
		return "", failure
	}
	return target, nil
}
