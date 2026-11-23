package information

import (
	"fmt"

	"encoding/json"
	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/engine"
)

// This records the bounded source response, never global absence.
type sourceCheckReceipt = acquisition.SourceCheck

func sourceCheckID(step Step) string { return fmt.Sprintf("source-check:%d", step.ID) }

// Native routes share the same bounded observation contract; empty output by itself is insufficient.
func emptyAssetObservation(asset Asset, receipt string, runtime engine.ProgramResult) *sourceCheckReceipt {
	items, ok := runtime.Collection["items"].([]any)
	if !ok || len(items) != 0 || len(runtime.SourceEvidence) == 0 || len(runtime.Requests) == 0 {
		return nil
	}
	var locator acquisition.EmptyResultEvidence
	switch asset.Kind {
	case "api":
		var plan APIPlan
		if json.Unmarshal([]byte(asset.ManifestJSON), &plan) != nil {
			return nil
		}
		locator.JSONPointer = &plan.ItemsPointer
	case "browser":
		var plan BrowserPlan
		if json.Unmarshal([]byte(asset.ManifestJSON), &plan) != nil || plan.EmptyState == nil {
			return nil
		}
		locator = browserEmptyLocator(plan)
	default:
		return nil
	}
	result := sourceCheckReceipt{ReceiptID: receipt, Outcome: "no_match"}
	for _, source := range runtime.SourceEvidence {
		check, err := acquisition.CheckEmptySource(source, locator)
		if err != nil {
			return nil
		}
		result.URLs = append(result.URLs, check.URLs...)
		result.Evidence = append(result.Evidence, check.Evidence...)
		result.Meaning = check.Meaning
		if check.CheckedAt.After(result.CheckedAt) {
			result.CheckedAt = check.CheckedAt
		}
	}
	return &result
}
