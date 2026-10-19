package aiapp

import "testing"

func TestDeliveryNormalEntryAcquisitionAndIndependentReuse(t *testing.T) {
	runCorrectModelSubsystemChain(t, false, 2, false, "", progressiveCase{Name: "delivery-normal-entry", ActualRunner: true, SelectorDiscovery: true, CompleteText: true, AdaptiveCrawler: true, LayoutVariant: 1, Expected: "completed"})
}
func TestDeliveryNormalEntryRepeatingModelReturnsPartial(t *testing.T) {
	runCorrectModelSubsystemChain(t, false, 1, false, "", progressiveCase{Name: "delivery-repeating-entry", ActualRunner: true, AdaptiveCrawler: true, CompleteText: true, Fault: "repeat_catalog", WrongPercent: 100, Expected: "failed"})
}
