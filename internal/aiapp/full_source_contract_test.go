package aiapp

import (
	"testing"
	"time"

	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/engine"
)

func TestCompleteSourceContractRejectsSupportedExcerpts(t *testing.T) {
	for _, tc := range []struct{ page, prefix, complete string }{
		{`<article><p>Opening.</p><p>Budget: 53. Deadline: 2026-10-20.</p></article>`, "Opening.", "Opening. Budget: 53. Deadline: 2026-10-20."},
		{`<main><section><p>SDK overview.</p><p>Removed endpoint /v1; migrate before November.</p></section></main>`, "SDK overview.", "SDK overview. Removed endpoint /v1; migrate before November."},
	} {
		for _, whole := range []bool{false, true} {
			value, selector := tc.prefix, "p"
			if whole {
				value, selector = tc.complete, "article, main"
			}
			url := "https://example.com/detail"
			receipt := acquisition.Receipt{Method: "GET", URL: url, Status: 200, FetchedAt: time.Now()}
			result := engine.ProgramResult{Collection: map[string]any{"items": []any{map[string]any{"raw_text": value, "source_url": url, "_evidence": map[string]any{"raw_text": map[string]any{"selector": selector, "index": 0}}}}}, Requests: []acquisition.Receipt{receipt}, SourceEvidence: []acquisition.Response{{Receipt: receipt, Body: tc.page}}}
			rows, err := BindRecords(result, RecordBinding{SourceID: "source", DataVersion: "v1", TaskID: 1, SchemaVersion: "v1"})
			if err != nil {
				t.Fatal(err)
			}
			spec := TaskSpec{Goal: "deliver complete fetched source text", ContractVersion: "v2", SchemaVersion: "v1", RequiredFields: []FieldRequirement{{Name: "raw_text", Type: "source_text"}}}
			coverage, err := EvaluateRecords(spec, rows, time.Now())
			if err != nil || coverage.Satisfied != whole {
				t.Fatalf("whole=%v coverage=%+v err=%v", whole, coverage, err)
			}
			merged, _ := MergeRecords(rows, nil)
			coverage, err = EvaluateMergedRecords(spec, merged, time.Now())
			if err != nil || coverage.Satisfied != whole {
				t.Fatalf("merged whole=%v coverage=%+v err=%v", whole, coverage, err)
			}
			// Ordinary source-supported text remains valid for excerpt tasks.
			if !whole {
				spec.RequiredFields[0].Type = "string"
				spec.RequireFieldSupport = true
				coverage, err = EvaluateRecords(spec, rows, time.Now())
				if err != nil || !coverage.Satisfied {
					t.Fatal("legitimate excerpt was rejected", coverage, err)
				}
			}
		}
	}
}
