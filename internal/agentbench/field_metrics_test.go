package agentbench

import (
	"context"
	"encoding/json"
	"go-lua-crawler/internal/aiapp"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFieldMetricsPassThroughNormalBenchmarkClient(t *testing.T) {
	now := time.Now()
	p := aiapp.Provenance{URL: "https://example.com/docs", FetchedAt: now, DataVersion: "v1"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var request map[string]any
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			if len(request) != 2 || request["goal"] != "collect facts" {
				t.Errorf("private scoring inputs leaked: %+v", request)
			}
			_, _ = w.Write([]byte(`{"id":"r1"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"run":    map[string]any{"id": "r1", "status": "completed"},
			"result": aiapp.TaskResult{Records: []aiapp.MergedRecord{{Fields: map[string]any{"name": "A"}, FieldSources: map[string][]aiapp.Provenance{"name": {p}}}}},
		})
	}))
	defer srv.Close()
	m := Manifest{Status: "frozen", Mode: "fixture", EnvironmentID: "test", Cases: []Case{{ID: "a", Goal: "collect facts", Split: "development", Solvable: true}}}
	g := map[string]GoldCase{"a": {Verification: "assistant_prepared", ExpectedStatus: "completed", Rows: []map[string]any{{"name": "A", "value": "2"}}}}
	rows, err := Execute(context.Background(), ClientConfig{Endpoint: srv.URL, Token: "test", Batch: "field-metrics", Variant: "code", Repetitions: 1, Timeout: time.Second}, m, g, nil)
	if err != nil || len(rows) != 1 {
		t.Fatalf("client failed: %v %+v", err, rows)
	}
	r := rows[0]
	if !r.FieldScored || r.DeliveredFields != 1 || r.RequiredFields != 2 || r.FieldCoverage == nil || *r.FieldCoverage != .5 || r.FieldPrecision == nil || *r.FieldPrecision != 1 {
		t.Fatalf("metrics lost at client boundary: %+v", r)
	}
	s := Summarize(rows)[0]
	if s.FieldScoredTrials != 1 || s.FieldCoverage == nil || *s.FieldCoverage != .5 {
		t.Fatalf("metrics lost at summary: %+v", s)
	}
}

func TestFieldMetricsMeasurePartialExtraDuplicateAndUnscoredOutputs(t *testing.T) {
	now := time.Now()
	p := aiapp.Provenance{URL: "https://example.com/docs", FetchedAt: now, DataVersion: "v1"}
	gold := GoldCase{Verification: "assistant_prepared", ExpectedStatus: "completed", KeyFields: []string{"name"}, Rows: []map[string]any{{"name": "A", "value": "2", "unit": "credits/request"}}}
	row := func(fields map[string]any) aiapp.MergedRecord {
		sources := map[string][]aiapp.Provenance{}
		for k := range fields {
			sources[k] = []aiapp.Provenance{p}
		}
		return aiapp.MergedRecord{Fields: fields, FieldSources: sources}
	}
	tests := []struct {
		name                string
		rows                []aiapp.MergedRecord
		matched, delivered  int
		coverage, precision any
	}{
		{"complete", []aiapp.MergedRecord{row(map[string]any{"name": "A", "value": "2", "unit": "credits/request"})}, 3, 3, float64(1), float64(1)},
		{"partial", []aiapp.MergedRecord{row(map[string]any{"name": "A", "value": "2"})}, 2, 2, float64(2) / 3, float64(1)},
		{"extra", []aiapp.MergedRecord{row(map[string]any{"name": "A", "value": "2", "unit": "credits/request", "unsupported": "claim"})}, 3, 4, float64(1), float64(.75)},
		{"wrong_unit", []aiapp.MergedRecord{row(map[string]any{"name": "A", "value": "2", "unit": "USD/request"})}, 2, 3, float64(2) / 3, float64(2) / 3},
		{"empty", nil, 0, 0, float64(0), nil},
		{"duplicate", []aiapp.MergedRecord{row(map[string]any{"name": "A"}), row(map[string]any{"name": "A"})}, 0, 2, float64(0), float64(0)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Judge(gold, "completed", aiapp.TaskResult{Records: tc.rows}, now)
			raw, _ := json.Marshal(got)
			var wire map[string]any
			_ = json.Unmarshal(raw, &wire)
			if wire["field_scored"] != true || wire["delivered_fields"] != float64(tc.delivered) || wire["field_coverage"] != tc.coverage || wire["field_precision"] != tc.precision || got.MatchedFields != tc.matched || got.RequiredFields != 3 {
				t.Fatalf("wrong field metrics: %s", raw)
			}
		})
	}
	gold.Verification = "pending"
	raw, _ := json.Marshal(Judge(gold, "completed", aiapp.TaskResult{}, now))
	var wire map[string]any
	_ = json.Unmarshal(raw, &wire)
	if wire["field_scored"] != false || wire["field_coverage"] != nil || wire["field_precision"] != nil {
		t.Fatalf("unprepared gold produced measured metrics: %s", raw)
	}
}

func TestFieldMetricsSummaryUsesCountsAndDoesNotInventLegacyDenominators(t *testing.T) {
	var trials []Trial
	err := json.Unmarshal([]byte(`[{"case_id":"a","field_scored":true,"matched_fields":2,"required_fields":3,"delivered_fields":2},{"case_id":"b","field_scored":true,"matched_fields":1,"required_fields":1,"delivered_fields":2},{"case_id":"legacy","matched_fields":99,"required_fields":99}]`), &trials)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(Summarize(trials)[0])
	var wire map[string]any
	_ = json.Unmarshal(raw, &wire)
	if wire["field_scored_trials"] != float64(2) || wire["field_coverage"] != float64(.75) || wire["field_precision"] != float64(.75) {
		t.Fatalf("legacy counts or macro averaging polluted metrics: %s", raw)
	}
}
