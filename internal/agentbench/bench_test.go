package agentbench

import (
	"encoding/json"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/aiapp"
	"testing"
	"time"
)

func TestIndependentJudgeRejectsSemanticWrongColumnAndMissingRows(t *testing.T) {
	now := time.Now()
	gold := GoldCase{Verification: "assistant_prepared", ExpectedStatus: "completed", Rows: []map[string]any{{"name": "Alpha", "version": "1.0"}}, MaxAgeSeconds: 60, AllowedDomains: []string{"fixture.example"}}
	provenance := aiapp.Provenance{URL: "https://fixture.example/releases", FetchedAt: now, DataVersion: "v1"}
	result := aiapp.TaskResult{Records: []aiapp.MergedRecord{{Fields: map[string]any{"name": "Alpha", "version": "1.0"}, FieldSources: map[string][]aiapp.Provenance{"name": {provenance}, "version": {provenance}}}}, Coverage: aiapp.CoverageReport{Satisfied: true}}
	if got := Judge(gold, "completed", result, now); got.Correct == nil || !*got.Correct {
		t.Fatalf("healthy control rejected: %+v", got)
	}
	result.Records[0].Fields["version"] = "Alpha"
	if got := Judge(gold, "completed", result, now); got.Correct == nil || *got.Correct || !got.FalseCompletion {
		t.Fatalf("wrong column accepted: %+v", got)
	}
	result.Records = nil
	if got := Judge(gold, "completed", result, now); got.Correct == nil || *got.Correct {
		t.Fatal("empty records accepted despite runtime coverage=true")
	}
	gold.Verification = "pending"
	if Judge(gold, "completed", result, now).Correct != nil {
		t.Fatal("missing gold became measured success/failure")
	}
}
func TestSummaryCostsIncludeFailuresAndUnknownAndSeparateVariants(t *testing.T) {
	good, bad := true, false
	usage := func(n int64) string {
		b, _ := json.Marshal(aiapp.Usage{PromptTokens: n, TotalTokens: n})
		return string(b)
	}
	rows := []Trial{{CaseID: "A", Variant: "code", Mode: "fixture", Status: "completed", Correct: &good, DurationMS: 10, Operations: []agentdata.Operation{{Kind: "model", UsageJSON: usage(10)}}}, {CaseID: "B", Variant: "code", Mode: "fixture", Status: "failed", Correct: &bad, DurationMS: 30, Operations: []agentdata.Operation{{Kind: "model", UsageJSON: usage(20)}}}, {CaseID: "C", Variant: "code", Mode: "fixture", Status: "failed", Correct: &bad, Operations: []agentdata.Operation{{Kind: "model", UsageJSON: "null"}}}, {CaseID: "A", Variant: "tools", Mode: "fixture", Status: "completed", Correct: &good, Operations: []agentdata.Operation{{Kind: "model", UsageJSON: usage(99)}}}}
	s := Summarize(rows)
	if len(s) != 2 {
		t.Fatal("different variants pooled")
	}
	for _, summary := range s {
		if summary.Variant != "code" {
			continue
		}
		if summary.Trials != 3 || summary.CorrectTasks != 1 || summary.ReportedTokens != 30 || summary.UnknownUsageAttempts != 1 || summary.TokensPerCorrectTask != nil || summary.P95MS != nil {
			t.Fatalf("invalid cost/denominator/percentile: %+v", summary)
		}
	}
	if value := Summarize([]Trial{{Mode: "live", Variant: "code", Correct: &bad, Operations: rows[0].Operations}})[0].TokensPerCorrectTask; value != nil {
		t.Fatal("all failures acquired defined per-correct cost")
	}
}
func TestFrozenManifestAndPublicRequestKeepGoldOutsideRuntime(t *testing.T) {
	m := Manifest{Status: "candidate", EnvironmentID: "local", Cases: []Case{{ID: "D1", Goal: "collect releases", Split: "development", Solvable: true}}}
	if m.Validate() == nil {
		t.Fatal("unfrozen candidates treated as benchmark")
	}
	m.Status = "frozen"
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	req := PublicRequest(m.Cases[0], "batch:D1:1")
	raw, _ := json.Marshal(req)
	var keys map[string]any
	json.Unmarshal(raw, &keys)
	if len(keys) != 2 || keys["goal"] == nil || keys["request_key"] == nil {
		t.Fatal("private truth or selected source injected into agent")
	}
}

func TestSafeStoppingDoesNotBecomeSuccessfulDelivery(t *testing.T) {
	good := true
	for _, status := range []string{"failed", "partial", "expired"} {
		s := Summarize([]Trial{{Status: status, Correct: &good}, {CaseID: "healthy", Status: "completed", Correct: &good}})[0]
		if s.CorrectTasks != 1 {
			t.Fatalf("correct stopping at %s inflated delivered tasks: %+v", status, s)
		}
	}
}
