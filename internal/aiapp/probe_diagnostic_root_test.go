package aiapp

import (
	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/agentdata"
	"testing"
	"time"
)

func TestProbeInternalDOMDiagnosticPreventsPrematureConclusion(t *testing.T) {
	s, r := evidenceRecoveryState()
	body := `<table><tr><td class="sku">A</td></tr></table>`
	structure := crawlerSourceStructure(acquisition.Response{Body: body, Receipt: acquisition.Receipt{URL: "https://source.example/item", Method: "GET", Status: 200, FetchedAt: time.Now()}})
	base := map[string]any{"mode": "probe", "items": []any{map[string]any{"report": "first"}}, "observed_sources": []any{map[string]any{"source_verified": true, "source_url": "https://source.example/item", "source_structure": structure}}}
	appendEvidenceRound(&s, "correcting", "validate_program", `{"code":"first","mode":"probe"}`, base)
	r = advanceDeliveryRecovery(r, s, HostProgress{}, false, "")
	appendEvidenceRound(&s, "correcting", "validate_program", `{"code":"count cells","mode":"probe"}`, base)
	r = advanceDeliveryRecovery(r, s, HostProgress{}, false, "")
	base["execution_diagnostics"] = []any{map[string]any{"code": "DOM_NO_MATCH", "api": "html_find", "selector": ".sku", "input_kind": "selected_text", "source_digest": agentdata.Hash(body), "instruction": "html_find_all returns text, not markup; extract from original source"}}
	appendEvidenceRound(&s, "correcting", "validate_program", `{"code":"test row scope","mode":"probe"}`, base)
	r = advanceDeliveryRecovery(r, s, HostProgress{}, false, "")
	if r.Phase != "correcting" || !r.NewEvidence {
		t.Fatal("actual new subcall diagnostic was collapsed into same source structure", r)
	}
	base["items"] = []any{map[string]any{"report": "cosmetic change"}}
	appendEvidenceRound(&s, "correcting", "validate_program", `{"code":"cosmetic1","mode":"probe"}`, base)
	r = advanceDeliveryRecovery(r, s, HostProgress{}, true, "")
	appendEvidenceRound(&s, "correcting", "validate_program", `{"code":"cosmetic2","mode":"probe"}`, base)
	r = advanceDeliveryRecovery(r, s, HostProgress{}, true, "")
	if r.Phase != "concluding" {
		t.Fatal("identical diagnostic rewording extended loop", r)
	}
}
