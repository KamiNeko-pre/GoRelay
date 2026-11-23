package agentbench

import (
	"go-lua-crawler/internal/aiapp"
	"testing"
	"time"
)

func TestCorrectDeliveryLatencyExcludesFailuresAndUnscoredRuns(t *testing.T) {
	good, bad := true, false
	rows := []Trial{}
	for n := 0; n < 20; n++ {
		rows = append(rows, Trial{CaseID: string(rune('A' + n)), Status: "completed", Correct: &good, DurationMS: int64(100 + n)})
	}
	rows = append(rows, Trial{CaseID: "failure", Status: "failed", Correct: &bad, DurationMS: 99999}, Trial{CaseID: "unscored", Status: "completed", DurationMS: 77777}, Trial{CaseID: "safe stop", Status: "partial", Correct: &good, DurationMS: 55555})
	s := Summarize(rows)[0]
	if s.CorrectDeliverySamples != 20 || s.CorrectDeliveryP95MS == nil || *s.CorrectDeliveryP95MS != 118 || s.LatencySamples != 23 || *s.P95MS <= 118 {
		t.Fatalf("latency populations mixed: %+v", s)
	}
}

func TestGoldBusinessKeyCannotBeReplacedBySimilarRow(t *testing.T) {
	gold := GoldCase{Verification: "assistant_prepared", ExpectedStatus: "completed", KeyFields: []string{"product"}, Rows: []map[string]any{{"product": "Firecrawl", "price": 29, "unit": "USD"}}}
	now := time.Now()
	p := aiapp.Provenance{URL: "https://example.com/prices", FetchedAt: now, DataVersion: "v1"}
	row := aiapp.MergedRecord{Fields: map[string]any{"product": "Another", "price": 29, "unit": "USD"}, FieldSources: map[string][]aiapp.Provenance{"product": {p}, "price": {p}, "unit": {p}}}
	got := Judge(gold, "completed", aiapp.TaskResult{Records: []aiapp.MergedRecord{row}}, now)
	if got.Correct == nil || *got.Correct || got.MatchedFields != 0 {
		t.Fatalf("a different product matched by field similarity: %+v", got)
	}
	row.Fields["product"] = "Firecrawl"
	got = Judge(gold, "completed", aiapp.TaskResult{Records: []aiapp.MergedRecord{row}}, now)
	if got.Correct == nil || !*got.Correct || got.MatchedFields != 3 {
		t.Fatalf("healthy business identity rejected: %+v", got)
	}
	got = Judge(gold, "completed", aiapp.TaskResult{Records: []aiapp.MergedRecord{row, row}}, now)
	if got.Correct == nil || *got.Correct {
		t.Fatal("duplicate business identities accepted")
	}
}
