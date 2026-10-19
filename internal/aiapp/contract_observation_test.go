package aiapp

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestObservationKeepsRequestedFieldsAheadOfWideMetadata(t *testing.T) {
	for _, name := range []string{"get_task_results", "evaluate_result"} {
		fields := map[string]any{"z_price": 17, "z_version": "1.2", "id": "record-1"}
		for n := 0; n < 40; n++ {
			fields[fmt.Sprintf("a_metadata_%02d", n)] = strings.Repeat("noise", 3000)
		}
		raw, _ := json.Marshal(map[string]any{"records": []any{map[string]any{"fields": fields}}})
		spec := &TaskSpec{RequiredFields: []FieldRequirement{{Name: "z_price", Type: "integer"}, {Name: "z_version", Type: "string"}}, JoinKeys: []string{"id"}}
		view := modelObservation(ToolResult{CallID: "wide", Name: name, Data: raw}, spec)
		if len(view.Data) > observationBytes || !strings.Contains(string(view.Data), `"z_price":17`) || !strings.Contains(string(view.Data), `"z_version":"1.2"`) || !strings.Contains(string(view.Data), `"id":"record-1"`) {
			t.Fatalf("required fields displaced by metadata; view bytes=%d", len(view.Data))
		}
		if !strings.Contains(string(raw), strings.Repeat("noise", 3000)) {
			t.Fatal("canonical observation changed")
		}
	}
}
