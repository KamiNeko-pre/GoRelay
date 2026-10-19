package aiapp

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"go-lua-crawler/internal/acquisition"
)

func TestCrawlerFieldEvidenceFailureExplainsCorrectLocators(t *testing.T) {
	for _, tool := range []string{"validate_program", "submit_extraction"} {
		for _, reason := range []string{"DOM locator not found", "source locator value differs from proposed field value"} {
			record := Record{Fields: map[string]any{"title": "proposed"}}
			record.Provenance.URL = "https://example.test/detail"
			record.Provenance.FieldSupport = map[string]FieldSupport{"title": {Status: "unsupported", Reason: reason}}
			err := requireSupportedFields(TaskSpec{RequireFieldSupport: true}, []Record{record})
			out := toolFailureReceipt(ToolCall{ID: "bad-evidence", Function: FunctionCall{Name: tool}}, ToolResult{}, err)
			if out.Failure == nil || out.Failure.Code != "FIELD_EVIDENCE_UNSUPPORTED" {
				t.Errorf("unclassified field evidence: %+v", out.Failure)
				continue
			}
			raw, _ := json.Marshal(out.Failure)
			for _, s := range []string{"title", reason, "_evidence", "selector", "json_pointer"} {
				if !strings.Contains(string(raw), s) {
					t.Errorf("missing correct evidence guidance %s", s)
				}
			}
		}
	}
}

func TestCrawlerFetchFailurePreservesStatusAndCorrectsURL(t *testing.T) {
	for _, status := range []int{404, 403} {
		response := acquisition.Response{Receipt: acquisition.Receipt{URL: "https://example.test/missing", Method: "GET", Status: status}}
		err := sourceFetchFailure(fmt.Errorf("source HTTP status %d", status), response, "https://example.test/missing", []string{"example.test"})
		out := toolFailureReceipt(ToolCall{ID: "missing-page", Function: FunctionCall{Name: "inspect_source"}}, ToolResult{}, err)
		if out.Failure == nil || out.Failure.Code != "SOURCE_HTTP_ERROR" {
			t.Fatalf("HTTP failure lost known outcome: %+v", out.Failure)
		}
		raw, _ := json.Marshal(out.Failure)
		for _, s := range []string{fmt.Sprint(status), "missing", "observed", "URL"} {
			if !strings.Contains(string(raw), s) {
				t.Errorf("missing URL correction fact %s", s)
			}
		}
	}
}

func TestCrawlerFieldEvidenceReportsAllUnsupportedFields(t *testing.T) {
	for _, names := range [][]string{{"title", "budget"}, {"publisher", "deadline"}} {
		r := Record{Fields: map[string]any{"source_url": "https://example.test/detail"}}
		r.Provenance.URL = "https://example.test/detail"
		supported := FieldSupport{Status: "supported", CheckerVersion: "host-locator-v1", EvidenceRefs: []string{r.Provenance.URL + "#observed"}}
		r.Provenance.FieldSupport = map[string]FieldSupport{"source_url": supported}
		for _, name := range names {
			r.Fields[name] = "proposed"
			r.Provenance.FieldSupport[name] = FieldSupport{Status: "unsupported", Reason: "missing _evidence." + name + " locator"}
		}
		err := requireSupportedFields(TaskSpec{RequireFieldSupport: true}, []Record{r})
		out := toolFailureReceipt(ToolCall{ID: "missing-locators", Function: FunctionCall{Name: "validate_program"}}, ToolResult{}, err)
		raw, _ := json.Marshal(out.Failure)
		var f struct {
			Details struct {
				Unsupported []struct {
					Field string `json:"field"`
				} `json:"unsupported_fields"`
			} `json:"details"`
		}
		json.Unmarshal(raw, &f)
		if len(f.Details.Unsupported) != len(names) {
			t.Fatalf("only first error is actionable: %s", raw)
		}
		for _, name := range names {
			if !strings.Contains(string(raw), "missing _evidence."+name+" locator") {
				t.Errorf("missing original diagnostic for %s: %s", name, raw)
			}
			r.Provenance.FieldSupport[name] = supported
		}
		if err := requireSupportedFields(TaskSpec{RequireFieldSupport: true}, []Record{r}); err != nil {
			t.Fatal("supported fields rejected", err)
		}
	}
}
