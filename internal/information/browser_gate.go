package information

import (
	"context"
	"encoding/json"

	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/agentdata"
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

// summary keeps failed execution diagnostics available without repeating the
// successful observations already held in the fact store.
func (report *luaFailureReport) summary() *luaFailureReport {
	if report == nil {
		return nil
	}
	out := *report
	out.Diagnostics = append([]luaDiagnostic(nil), report.Diagnostics...)
	for i := range out.Diagnostics {
		if out.Diagnostics[i].Failure == nil {
			out.Diagnostics[i].Data = nil
		}
	}
	return &out
}
