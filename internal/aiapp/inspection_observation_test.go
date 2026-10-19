package aiapp

import (
	"encoding/json"
	"go-lua-crawler/internal/acquisition"
	"strings"
	"testing"
)

func TestLargeInspectionKeepsBoundedModelPreviewAndOriginalReceipt(t *testing.T) {
	page := sourceInspection{Source: acquisition.Response{Body: strings.Repeat("x", 600<<10), Receipt: acquisition.Receipt{URL: "https://fixture.example/docs", Status: 200, Bytes: 600 << 10}}}
	raw, _ := json.Marshal(page)
	view := modelObservation(ToolResult{CallID: "inspection", Name: "inspect_source", Data: raw})
	var got sourceInspection
	if err := json.Unmarshal(view.Data, &got); err != nil {
		t.Fatal(err)
	}
	if !got.PreviewTruncated || len(got.Source.Body) > 2000 || got.TotalRunes != 600<<10 || got.Source.Receipt.Bytes != 600<<10 || got.NextOffset == nil {
		t.Fatal("network limit leaked into model context or destroyed receipt")
	}
	if !strings.Contains(string(raw), strings.Repeat("x", 600<<10)) {
		t.Fatal("persisted original changed")
	}
}
