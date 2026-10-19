package aiapp

import (
	"encoding/json"
	"go-lua-crawler/internal/acquisition"
	"strings"
	"testing"
)

func TestJSONInspectionShowsFieldsBeyondLongMetadata(t *testing.T) {
	for _, body := range []string{
		`[{"author":{"profile":"` + strings.Repeat("profile", 2000) + `"},"body":"` + strings.Repeat("notes", 2000) + `","tag_name":"v1.2","published_at":"2026-10-09T12:00:00Z","draft":false,"prerelease":false}]`,
		`{"data":{"items":[{"sku":"a","price":12},{"sku":"b","price":20}],"next":"cursor2"},"metadata":{"description":"` + strings.Repeat("metadata", 2000) + `"}}`,
	} {
		original := inspectResponse(acquisition.Response{Body: body, Receipt: acquisition.Receipt{URL: "https://fixture.example/api", Status: 200, Bytes: int64(len(body))}}, acquisition.Stats{})
		raw, _ := json.Marshal(original)
		view := modelObservation(ToolResult{CallID: "json", Name: "inspect_source", Data: raw})
		var decoded struct {
			JSON *json.RawMessage `json:"json_structure"`
		}
		if err := json.Unmarshal(view.Data, &decoded); err != nil || decoded.JSON == nil {
			t.Fatalf("structured API fields invisible: %s %v", view.Data, err)
		}
		text := string(*decoded.JSON)
		if strings.Contains(body, "tag_name") && (!strings.Contains(text, "tag_name") || !strings.Contains(text, "v1.2") || !strings.Contains(text, "published_at")) {
			t.Fatal("release fields were displaced by author/body metadata")
		}
		if strings.Contains(body, "sku") && (!strings.Contains(text, "sku") || !strings.Contains(text, "price") || !strings.Contains(text, "cursor2")) {
			t.Fatal("nested array/cursor shape lost")
		}
		if len(view.Data) > observationBytes || strings.Contains(text, strings.Repeat("profile", 30)) {
			t.Fatal("unbounded JSON inspection")
		}
		var stored sourceInspection
		_ = json.Unmarshal(raw, &stored)
		if stored.Source.Body != body {
			t.Fatal("canonical response changed")
		}
	}
}

func TestJSONEmbeddedMarkupDoesNotBecomeAnHTMLSource(t *testing.T) {
	jsonBody := `{"body":"<main><p>This HTML is a string value inside a JSON record, not the response document.</p></main>","version":"v1"}`
	jsonView := inspectResponse(acquisition.Response{Body: jsonBody}, acquisition.Stats{})
	if jsonView.JSONStructure == nil || len(jsonView.Structure) != 0 {
		t.Fatal("JSON string markup became source DOM")
	}
	htmlView := inspectResponse(acquisition.Response{Body: `<main><p>This is a real HTML source paragraph with sufficient content.</p></main>`}, acquisition.Stats{})
	if htmlView.JSONStructure != nil || len(htmlView.Structure) == 0 {
		t.Fatal("real HTML source lost structural inspection")
	}
}
