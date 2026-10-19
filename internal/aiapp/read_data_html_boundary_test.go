package aiapp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"go-lua-crawler/internal/agentdata"
)

func TestUnifiedHTMLContinuationBindsRepresentationAndReceipt(t *testing.T) {
	s, run, original := storedHTMLReader(t, `<main id="data"><style>raw-style-marker</style><p>`+strings.Repeat("documented condition. ", 240)+`</p></main>`)
	read := func(r agentdata.Run, args any) (map[string]any, error) {
		raw, _ := json.Marshal(args)
		out, err := s.readData(context.Background(), r, ToolCall{ID: "read", Function: FunctionCall{Name: "read_data", Arguments: string(raw)}})
		data := map[string]any{}
		_ = json.Unmarshal(out.Data, &data)
		return data, err
	}
	first, err := read(run, map[string]any{"handle": "source", "selector": "#data"})
	if err != nil || first["next_cursor"] == nil {
		t.Fatal(first, err)
	}
	second, err := read(run, map[string]any{"cursor": first["next_cursor"]})
	if err != nil {
		t.Fatal("unified cursor lost HTML representation", err)
	}
	if second["offset"].(float64) <= first["offset"].(float64) || second["view_version"] != htmlInformationVersion || second["new_http_requests"] != float64(0) {
		t.Fatal("cursor made no bound progress", second)
	}
	repeat, err := read(run, map[string]any{"cursor": first["next_cursor"]})
	if err != nil || repeat["text"] != second["text"] {
		t.Fatal("repeat read changed", err)
	}
	foreign := run
	foreign.ID = agentdata.Token()
	if _, err := read(foreign, map[string]any{"cursor": first["next_cursor"]}); err == nil {
		t.Fatal("cursor crossed Run")
	}
	if _, err := read(run, map[string]any{"cursor": nextReadCursor(dataReadArgs{Handle: "source", Selector: "#data"}, 2000)}); err == nil {
		t.Fatal("old HTML cursor reinterpreted silently")
	}
	var op agentdata.Operation
	if err := s.Data.DB.Where("run_id = ? AND `key` = ?", run.ID, "tool:source").First(&op).Error; err != nil || op.ResultJSON != original {
		t.Fatal("reading changed receipt", err)
	}
}

func TestHTMLAlternateReadersCannotRestoreRawMarkup(t *testing.T) {
	for _, body := range []string{`<main><style>raw-style-marker</style><p>Basic costs 1 credit per request.</p></main>`, `<article><svg>raw-style-marker</svg><table><tr><th>Mode</th><th>Unit</th></tr><tr><td>Advanced</td><td>2 credits per request</td></tr></table></article>`} {
		s, run, _ := storedHTMLReader(t, body)
		out, err := s.readData(context.Background(), run, ToolCall{Function: FunctionCall{Name: "read_data", Arguments: `{"handle":"source","fields":["body"]}`}})
		if err != nil || strings.Contains(string(out.Data), "raw-style-marker") || strings.Contains(string(out.Data), `\u003c`) {
			t.Fatal("body route restored raw HTML", string(out.Data), err)
		}
		for _, pointer := range []string{"/data/source/body", "/data"} {
			args, _ := json.Marshal(map[string]any{"tool_call_id": "source", "pointer": pointer})
			out, err = s.readObservation(context.Background(), run, ToolCall{Function: FunctionCall{Name: "read_observation", Arguments: string(args)}})
			if err != nil || strings.Contains(string(out.Data), "raw-style-marker") || strings.Contains(string(out.Data), `\u003cmain`) || strings.Contains(string(out.Data), `\u003carticle`) {
				t.Fatal("historical reader restored raw HTML", pointer, string(out.Data), err)
			}
		}
	}
}
