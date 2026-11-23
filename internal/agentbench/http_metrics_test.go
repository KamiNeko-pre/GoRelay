package agentbench

import (
	"encoding/json"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/aiapp"
	"testing"
)

func TestHTTPCountsIncludeFailedAttemptsAndKeepUnknown(t *testing.T) {
	raw, _ := json.Marshal(aiapp.ToolResult{Error: "failed after redirects", HTTP: &aiapp.HTTPObservation{Known: true, Requests: 3, Bytes: 100}})
	rows := []Trial{{CaseID: "failed", Operations: []agentdata.Operation{{Kind: "validate_program", Stage: "failed", ResultJSON: string(raw)}, {Kind: "worker_attempt", Stage: "observed", ResultJSON: `{"runtime":{"stats":{"requests":2,"bytes":80}},"outcome_known":true}`}, {Kind: "inspect_source", Stage: "failed", ResultJSON: `{"error":"transport lost"}`}, {Kind: "model", UsageJSON: "null"}}}}
	s := Summarize(rows)[0]
	if s.KnownHTTPRequests != 5 || s.KnownHTTPBytes != 180 || s.UnknownHTTPObservations != 1 {
		t.Fatalf("incorrect network costs: %+v", s)
	}
}
