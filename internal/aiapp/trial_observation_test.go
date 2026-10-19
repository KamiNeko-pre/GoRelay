package aiapp

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTrialObservationBoundsBodyButPreservesCandidateAndValidation(t *testing.T) {
	for _, name := range []string{"historical research", "release comparison"} {
		result, _ := json.Marshal(map[string]any{"collection": map[string]any{"items": []any{map[string]any{"title": name, "body": strings.Repeat("正文", 25000), "source_url": "https://example.org/report"}}}})
		data, err := candidateReceipt(Candidate{ID: "candidate-one", ArtifactHash: "version-one", ChecksJSON: `{"semantic_support_checked":false,"coverage":{"satisfied":true}}`, ResultJSON: string(result)})
		if err != nil {
			t.Fatal(err)
		}
		got := modelObservation(ToolResult{CallID: "original-trial", Name: "validate_program", Data: data})
		if len(got.Data) > 12000 || !strings.Contains(string(got.Data), "candidate-one") || !strings.Contains(string(got.Data), "semantic_support_checked") {
			t.Fatalf("unbounded/lost receipt: %d", len(got.Data))
		}
		if len(data) < 50000 {
			t.Fatal("fixture not large enough")
		}
		var view struct {
			Reads []observationRead `json:"reads"`
		}
		if err := json.Unmarshal(got.Data, &view); err != nil {
			t.Fatal(err)
		}
		var original any
		receipt, _ := json.Marshal(ToolResult{CallID: "original-trial", Name: "validate_program", Data: data})
		if err := json.Unmarshal(receipt, &original); err != nil {
			t.Fatal(err)
		}
		foundBody := false
		for _, read := range view.Reads {
			pointer := read.Arguments["pointer"].(string)
			value, err := jsonPointer(original, pointer)
			if err != nil {
				t.Fatal("reader does not address the original typed receipt", pointer, err)
			}
			if strings.HasSuffix(pointer, "/body") {
				foundBody = true
				if value != strings.Repeat("正文", 25000) {
					t.Fatal("trial body changed")
				}
			}
		}
		if !foundBody {
			t.Fatal("typed trial does not expose direct body reader")
		}
	}
	failure := ToolResult{Name: "validate_program", Error: "Lua runtime failed"}
	if modelObservation(failure).Error != failure.Error {
		t.Fatal("failure hidden")
	}
}
