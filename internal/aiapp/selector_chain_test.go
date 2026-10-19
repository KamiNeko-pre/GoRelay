package aiapp

import (
	"encoding/json"
	"testing"
)

// Compatibility adapter for older protocol unit tests. Acceptance uses the same
// receipt-driven crawler policy as the current model HTTP adapter.
func selectorDiscoveryDecision(request ChatRequest) (ChatResponse, bool, error) {
	if len(request.Tools) == 0 {
		return ChatResponse{}, false, nil
	}
	_, facts := auditVisibleReceipts(request)
	var children []any
	json.Unmarshal(facts["children"], &children)
	if len(children) > 0 {
		return ChatResponse{}, false, nil
	}
	out, err := adaptiveCrawlerDecision(request, false)
	return out, true, err
}

func TestSelectorDrivenFullChainAndIndependentCapabilityReuse(t *testing.T) {
	runCorrectModelSubsystemChain(t, false, 2, false, "", progressiveCase{Name: "selector-discovery-full-chain", SelectorDiscovery: true, CompleteText: true, ActualRunner: true, AdaptiveCrawler: true, LayoutVariant: 1, Expected: "completed"})
}
