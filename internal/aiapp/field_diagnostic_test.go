package aiapp

import (
	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/engine"
	"strings"
	"testing"
	"time"
)

func TestRejectedFieldEvidencePreservesTheFailingBoundary(t *testing.T) {
	for _, tc := range []struct {
		name    string
		locator map[string]any
		want    string
	}{
		{"two-captures", map[string]any{"selector": "#name", "pattern": `(name) (Alpha)`}, "exactly one match and capture"},
		{"missing-node", map[string]any{"selector": "#absent"}, "DOM locator not found"},
		{"missing-json-path", map[string]any{"json_pointer": "/missing"}, ""},
		{"protected-valid", map[string]any{"selector": "#name", "pattern": `name (Alpha)`}, "supported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `<p id="name">name Alpha</p>`
			if tc.name == "missing-json-path" {
				body = `{"name":"Alpha"}`
			}
			url := "https://example.org/source"
			receipt := acquisition.Receipt{URL: url, Method: "GET", Status: 200, FetchedAt: time.Now()}
			program := engine.ProgramResult{Requests: []acquisition.Receipt{receipt}, SourceEvidence: []acquisition.Response{{Receipt: receipt, Body: body}}, Collection: map[string]any{"items": []any{map[string]any{"value": "Alpha", "source_url": url, "_evidence": map[string]any{"value": tc.locator}}}}}
			rows, err := BindRecords(program, RecordBinding{SourceID: "s", DataVersion: "v", TaskID: 1, SchemaVersion: "v1"})
			if err != nil {
				t.Fatal(err)
			}
			err = requireSupportedFields(TaskSpec{RequireFieldSupport: true}, rows)
			if tc.want == "supported" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			reason := rows[0].Provenance.FieldSupport["value"].Reason
			if err == nil || reason == "" || !strings.Contains(err.Error(), reason) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("original diagnostic disappeared: reason=%q error=%v", reason, err)
			}
		})
	}
}
