package agentbench

import (
	"encoding/json"
	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/engine"
)

// Only physical network measurements count, never budget reservations or number
// of successful receipts. Legacy operations are read conservatively.
func operationHTTP(op agentdata.Operation) (requests int, bytes int64, known, relevant bool) {
	if op.Kind != "inspect_source" && op.Kind != "validate_program" && op.Kind != "worker_attempt" {
		return
	}
	relevant = true
	var out aiapp.ToolResult
	if op.Kind != "worker_attempt" {
		if json.Unmarshal([]byte(op.ResultJSON), &out) != nil {
			return
		}
		if out.HTTP != nil {
			return out.HTTP.Requests, out.HTTP.Bytes, out.HTTP.Known, true
		}
	}
	var stats *acquisition.Stats
	switch op.Kind {
	case "inspect_source":
		var data struct {
			Stats *acquisition.Stats `json:"actual_stats"`
			Code  string             `json:"code"`
		}
		if json.Unmarshal(out.Data, &data) != nil {
			return
		}
		if data.Code != "" {
			return 0, 0, true, true
		} // Reading a persisted capability, no HTTP.
		stats = data.Stats
	case "validate_program":
		var candidate aiapp.Candidate
		if json.Unmarshal(out.Data, &candidate) != nil || candidate.ResultJSON == "" {
			return
		}
		var result struct {
			Stats *acquisition.Stats `json:"stats"`
		}
		if json.Unmarshal([]byte(candidate.ResultJSON), &result) != nil {
			return
		}
		stats = result.Stats
	case "worker_attempt":
		var attempt struct {
			Runtime engine.ProgramResult `json:"runtime"`
			Known   bool                 `json:"outcome_known"`
		}
		if json.Unmarshal([]byte(op.ResultJSON), &attempt) != nil || (!attempt.Known && attempt.Runtime.DurationMS == 0 && attempt.Runtime.Stats.Requests == 0 && len(attempt.Runtime.Requests) == 0) {
			return
		}
		stats = &attempt.Runtime.Stats
	}
	if stats != nil {
		return stats.Requests, stats.Bytes, true, true
	}
	return
}
