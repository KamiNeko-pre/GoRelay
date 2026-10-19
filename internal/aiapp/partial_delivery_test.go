package aiapp

import (
	"strings"
	"testing"
)

func TestHostStopProducesReadableHonestPartialDelivery(t *testing.T) {
	for _, name := range []string{"budget", "unchanged loop"} {
		result := TaskResult{Coverage: CoverageReport{PartialValidRecords: 1}, Records: []MergedRecord{{Fields: map[string]any{"title": "Observed notice", "budget": "not available"}, Sources: []Provenance{{URL: "https://source.example/notice"}}}}, Gaps: []string{"deadline missing"}}
		attachHostStopDelivery(&result, name)
		if !strings.Contains(result.Summary, "Observed notice") || !strings.Contains(result.Summary, "https://source.example/notice") || !strings.Contains(result.Summary, "deadline missing") || result.Answer != nil {
			t.Fatalf("facts or gaps lost, or fabricated answer: %+v", result)
		}
	}
	existing := TaskResult{Summary: "Existing cited model answer"}
	attachHostStopDelivery(&existing, "stop")
	if existing.Summary != "Existing cited model answer" {
		t.Fatal("existing answer replaced")
	}
	empty := TaskResult{}
	attachHostStopDelivery(&empty, "upstream unavailable")
	if !strings.Contains(empty.Summary, "upstream unavailable") || len(empty.Records) != 0 || empty.Answer != nil {
		t.Fatal("empty failure must disclose no usable result")
	}
}
