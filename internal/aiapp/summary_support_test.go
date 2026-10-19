package aiapp

import (
	"go-lua-crawler/internal/knowledge"
	"testing"
)

func TestExtractiveSummaryNeedsActualCitedTextNotJustValidID(t *testing.T) {
	for _, text := range []string{"预算金额：53万元。截止日期：2026-10-20。", "Version 2 removes /v1; migration is required."} {
		answer := RAGAnswer{Status: "answered", Answer: "invented conclusion", Citations: []string{"ev_current"}, Evidence: knowledge.EvidenceSet{Entries: []knowledge.Evidence{{ID: "ev_current", Text: text}}}}
		if err := checkExtractiveSummary(&answer); err == nil {
			t.Fatal("valid citation promoted an unsupported factual answer")
		}
		answer.Answer = text
		if err := checkExtractiveSummary(&answer); err != nil || answer.SupportLevel != "verified_excerpt" {
			t.Fatal("actual cited excerpt rejected", answer, err)
		}
		answer.Citations = []string{"ev_other"}
		if err := checkExtractiveSummary(&answer); err == nil {
			t.Fatal("quote supported by an uncited source was accepted")
		}
	}
}
