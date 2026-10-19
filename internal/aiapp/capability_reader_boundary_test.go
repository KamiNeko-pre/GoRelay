package aiapp

import (
	"context"
	"encoding/json"
	"go-lua-crawler/internal/agentdata"
	"strings"
	"testing"
)

func TestCapabilityInspectionReadsAsAssetNotEmptyFetchedPage(t *testing.T) {
	for _, registered := range []bool{true, false} {
		s, r, _ := storedHTMLReader(t, `<article><h2>Real page title</h2><p>Real page facts</p></article>`)
		code := strings.Repeat("-- source-specific program code\n", 200)
		data := map[string]any{"code": code, "needs_public_typed_trial": true}
		pointer := "/code"
		if registered {
			data = map[string]any{"head": agentdata.SourceHead{SourceID: "registered"}, "artifact": agentdata.ProgramArtifact{Code: code, Hash: "immutable"}}
			pointer = "/artifact/code"
		}
		out := ToolResult{CallID: "asset", Name: "inspect_source"}
		out.Data, _ = json.Marshal(data)
		raw, _ := json.Marshal(out)
		if err := s.Data.DB.Create(&agentdata.Operation{RunID: r.ID, Key: "tool:asset", Kind: "inspect_source", Stage: "succeeded", ResultJSON: string(raw)}).Error; err != nil {
			t.Fatal(err)
		}
		args, _ := json.Marshal(map[string]any{"handle": "asset", "view": "json", "pointer": pointer, "limit": 500})
		read, err := s.readData(context.Background(), r, ToolCall{ID: "read-code", Function: FunctionCall{Name: "read_data", Arguments: string(args)}})
		if err != nil {
			t.Fatalf("asset mistaken for empty fetched page: %v", err)
		}
		var got struct {
			Text string `json:"text"`
			Next string `json:"next_cursor"`
		}
		if err := json.Unmarshal(read.Data, &got); err != nil {
			t.Fatal(err)
		}
		if got.Text == "" || !strings.HasPrefix(code, got.Text) || got.Next == "" {
			t.Fatal("code cannot be read exactly with continuation", string(read.Data))
		}
		page, err := s.readData(context.Background(), r, ToolCall{ID: "read-page", Function: FunctionCall{Name: "read_data", Arguments: `{"handle":"source","view":"dom","selector":"p"}`}})
		if err != nil || !strings.Contains(string(page.Data), "Real page facts") {
			t.Fatal("real HTML page no longer readable", err)
		}
		if _, err := s.inspection(context.Background(), r, "asset"); err == nil {
			t.Fatal("program asset accepted as HTTP evidence for extraction")
		}
	}
}
