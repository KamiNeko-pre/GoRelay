package information

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/agentstatus"
	"go-lua-crawler/internal/aiapp"
)

type luaDiagnostic struct {
	OperationID uint64               `json:"operation_id"`
	Tool        string               `json:"tool"`
	Failure     *aiapp.FailureInfo   `json:"failure,omitempty"`
	Receipt     *acquisition.Receipt `json:"source_receipt,omitempty"`
	Data        json.RawMessage      `json:"observations,omitempty"`
}

type luaFailureReport struct {
	RunID        string          `json:"run_id"`
	Status       string          `json:"status"`
	Error        string          `json:"error"`
	Result       json.RawMessage `json:"result,omitempty"`
	Diagnostics  []luaDiagnostic `json:"diagnostics"`
	FinalMessage *aiapp.Message  `json:"final_model_message,omitempty"`
}

type browserAssessment struct {
	Action            string `json:"action"`
	CauseCode         string `json:"cause_code"`
	Reason            string `json:"reason"`
	SourceOperationID uint64 `json:"source_operation_id,omitempty"`
}

// Preserve the finished crawler's own report and structured error boundaries.
// A provider's HTTP 403 must never be mistaken for a source's HTTP 403.
func (s *Service) crawlerFailureReport(ctx context.Context, child agentdata.Run) (luaFailureReport, error) {
	report := luaFailureReport{RunID: child.ID, Status: child.Status, Error: child.LastError, Diagnostics: []luaDiagnostic{}}
	if json.Valid([]byte(child.ResultJSON)) {
		report.Result = json.RawMessage(child.ResultJSON)
	}
	ops, err := s.Crawler.Data.Operations(ctx, child.ID)
	if err != nil {
		return report, err
	}
	for _, op := range ops {
		if op.Kind == "model" {
			var response aiapp.ChatResponse
			if json.Unmarshal([]byte(op.ResultJSON), &response) == nil {
				message := response.Message
				report.FinalMessage = &message
				if response.Failure != nil {
					report.Diagnostics = append(report.Diagnostics, luaDiagnostic{OperationID: op.ID, Tool: "model", Failure: response.Failure})
				}
			}
			continue
		}
		var out aiapp.ToolResult
		if json.Unmarshal([]byte(op.ResultJSON), &out) != nil {
			continue
		}
		diagnostic := luaDiagnostic{OperationID: op.ID, Tool: out.Name, Failure: out.Failure, Data: out.Data}
		if out.Failure != nil && out.Failure.Code == "SOURCE_HTTP_ERROR" {
			var details struct {
				Receipt acquisition.Receipt `json:"receipt"`
			}
			raw, _ := json.Marshal(out.Failure.Details)
			if json.Unmarshal(raw, &details) == nil && details.Receipt.Status > 0 {
				diagnostic.Receipt = &details.Receipt
			}
		} else if out.Name == "inspect_source" {
			var observation struct {
				Source acquisition.Response `json:"source"`
			}
			if json.Unmarshal(out.Data, &observation) == nil && observation.Source.Receipt.Status > 0 {
				diagnostic.Receipt = &observation.Source.Receipt
			}
		}
		if diagnostic.Failure != nil || diagnostic.Receipt != nil {
			report.Diagnostics = append(report.Diagnostics, diagnostic)
		}
	}
	return report, nil
}

func browserTarget(decision browserAssessment, report luaFailureReport, scope []string) (string, error) {
	if decision.Action != "request_browser" || decision.CauseCode != "anti_bot" || strings.TrimSpace(decision.Reason) == "" {
		code := agentstatus.Unclassified
		switch decision.CauseCode {
		case "program", "parameters", "fields", "evidence":
			code = agentstatus.AgentExhausted
		case "access", "rate_limit", "transport", "anti_bot":
			code = agentstatus.SourceUnavailable
		case "provider":
			for _, diagnostic := range report.Diagnostics {
				if diagnostic.Tool == "model" && diagnostic.Failure != nil {
					status := aiapp.OperationalFailureStatus(diagnostic.Failure)
					if status.Owner == "business" || status.Owner == "system" {
						code = status.Code
					}
				}
			}
		}
		return "", agentstatus.Wrap(code, actionFailure("LUA_FAILURE_RECORDED", fmt.Errorf("%s: %s", decision.CauseCode, decision.Reason), "Inspect the saved lua_failure_report and original crawler reasoning; repair only the verified owning boundary."))
	}
	for _, diagnostic := range report.Diagnostics {
		if diagnostic.OperationID == decision.SourceOperationID && diagnostic.Tool != "model" && diagnostic.Receipt != nil && urlInScope(diagnostic.Receipt.URL, scope) {
			return diagnostic.Receipt.URL, nil
		}
	}
	return "", actionFailure("BROWSER_SOURCE_REFERENCE_INVALID", fmt.Errorf("browser request must reference an observed source HTTP operation, not a model/provider error or invented URL"), "Use source_receipt and the original error code to identify the source barrier. Provider HTTP errors and Lua/field failures belong in the error log.")
}

func (s *Service) reviewBrowser(ctx context.Context, r *Request, in Input, state *routeContext) (string, error) {
	if state.LuaFailure == nil {
		return "failed", actionFailure("LUA_FAILURE_REPORT_MISSING", fmt.Errorf("no finished Lua acquisition failure report"), "Wait for normal Lua correction to finish and preserve its actual failure before requesting Browser.")
	}
	child, err := s.Crawler.Data.Run(ctx, r.ChildRunID)
	if err != nil {
		return "review_browser", err
	}
	if !child.IsTerminal() {
		return "wait_crawler", nil
	}
	if child.Status == "cancelled" || child.Status == "expired" {
		return child.Status, nil
	}
	schema := map[string]any{"type": "object", "properties": map[string]any{
		"action":              map[string]any{"type": "string", "enum": []string{"request_browser", "record_failure"}},
		"cause_code":          map[string]any{"type": "string", "enum": []string{"anti_bot", "program", "parameters", "fields", "evidence", "transport", "provider", "access", "rate_limit", "unknown"}},
		"reason":              map[string]any{"type": "string"},
		"source_operation_id": map[string]any{"type": "integer", "minimum": 0},
	}, "required": []string{"action", "cause_code", "reason"}, "additionalProperties": false}
	raw, err := s.modelAction(ctx, r, "assess_lua_failure", schema, map[string]any{
		"finished_lua_report": state.LuaFailure,
		"method":              "Normal Lua execution and correction have finished without delivery. Submit the failure reason. Distinguish source HTTP statuses from provider/model HTTP statuses and structured Lua/field/parameter errors. Source 403/429/503 can indicate anti-bot controls, but authentication denial or an ordinary rate limit does not by itself require a browser. Request Browser as the final route only when anti-bot behavior exceeds the available Lua HTTP ecosystem; cite the observed source operation and explain the missing browser/session/JavaScript capability. Otherwise record_failure with the actual cause for later repair. No mandatory quote or particular status is required; make the decision from the actual report, tool facts and model reasoning, never from the fact of failure alone.",
	})
	if err != nil {
		return "review_browser", err
	}
	var decision browserAssessment
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decision); err != nil {
		return "review_browser", actionFailure("LUA_FAILURE_REPORT_INVALID", err, "Return the offered structured failure report, classifying the actual owning layer and source HTTP status before requesting Browser.")
	}
	if (decision.Action != "request_browser" && decision.Action != "record_failure") || strings.TrimSpace(decision.Reason) == "" {
		return "review_browser", actionFailure("ROUTING_ARGUMENTS_INVALID", fmt.Errorf("action must be request_browser/record_failure and reason must explain the actual failure"), "Submit a cause_code and a concrete failure explanation using the offered schema.")
	}
	step, fresh, err := s.reserveStep(ctx, *r, "lua_failure_report", map[string]any{"report": state.LuaFailure, "assessment": decision})
	if err != nil {
		return "review_browser", err
	}
	if fresh {
		if err := s.finishStep(ctx, step, map[string]any{"report": state.LuaFailure, "assessment": decision}, nil); err != nil {
			return "review_browser", err
		}
	}
	state.BrowserDecision = &decision
	target, err := browserTarget(decision, *state.LuaFailure, in.SourceScope)
	if err != nil {
		if decision.Action == "request_browser" && decision.CauseCode == "anti_bot" {
			return "review_browser", err
		}
		return "failed", err
	}
	state.URL = target
	if err := s.storeBrowserGrant(ctx, *r, target, decision.Reason, state.LuaFailure.RunID, ""); err != nil {
		return "review_browser", err
	}
	r.StatusJSON = string(mustMarshal(agentstatus.Of(agentstatus.AntiBotBrowserApproved)))
	return "inspect_browser", nil
}
