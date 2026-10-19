package engine

import (
	"context"
	"encoding/json"
	"go-lua-crawler/internal/acquisition"
	"testing"
	"time"
)

func TestObservedDOMFailureSurvivesSuccessfulProbe(t *testing.T) {
	for _, body := range []string{`<table><tr><td class="sku">A</td></tr></table>`, `<section><span class="sku">B</span></section>`} {
		t.Run(body[:8], func(t *testing.T) {
			code := `local b=assert(http_get(TARGET_URL));local text=html_find(b,".sku");local missing,err=html_find(text,".sku");return true,{items={{diagnostic=tostring(err)}}}`
			r, e := RunProgram(context.Background(), ProgramRequest{Code: code, TargetURL: "https://source.example/item"}, DefaultProgramLimits(), func(context.Context, string, string) (acquisition.Response, error) {
				return acquisition.Response{Body: body, Receipt: acquisition.Receipt{URL: "https://source.example/item", Method: "GET", Status: 200, FetchedAt: time.Now()}}, nil
			})
			if e != nil {
				t.Fatal(e)
			}
			raw, _ := json.Marshal(r)
			var wire map[string]json.RawMessage
			json.Unmarshal(raw, &wire)
			var diagnostics []map[string]any
			json.Unmarshal(wire["execution_diagnostics"], &diagnostics)
			if len(diagnostics) != 1 || diagnostics[0]["code"] != "DOM_NO_MATCH" || diagnostics[0]["input_kind"] != "selected_text" || diagnostics[0]["source_digest"] == "" {
				t.Fatalf("successful tool swallowed actual DOM defect: %s", raw)
			}
		})
	}
}
func TestInventedDOMInputCannotCreateObservedDiagnostic(t *testing.T) {
	r, e := RunProgram(context.Background(), ProgramRequest{Code: `local v,e=html_find("fake text",".missing");return true,{items={{message=tostring(e)}}}`}, DefaultProgramLimits(), nil)
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(r)
	var m map[string]any
	json.Unmarshal(raw, &m)
	if rows, ok := m["execution_diagnostics"].([]any); ok && len(rows) > 0 {
		t.Fatal("invented input became observed source", m)
	}
}
