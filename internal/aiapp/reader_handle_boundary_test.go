package aiapp

import (
	"context"
	"encoding/json"
	"testing"

	"go-lua-crawler/internal/agentdata"
)

func TestModelReaderProjectionPreservesSourceHandle(t *testing.T) {
	for _, view := range []string{"dom", "content"} {
		t.Run(view, func(t *testing.T) {
			s, run, _ := storedHTMLReader(t, `<main id="article"><p>Original fact.</p></main>`)
			run.BudgetJSON = `{"loop_version":3}`
			args, _ := json.Marshal(map[string]any{"handle": "source", "view": view, "selector": "#article"})
			call := ToolCall{ID: "reader-result", Function: FunctionCall{Name: "read_data", Arguments: string(args)}}
			receipt, err := s.readData(context.Background(), run, call)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(receipt)
			if err := s.Data.DB.Create(&agentdata.Operation{RunID: run.ID, Key: "tool:" + call.ID, Kind: "read_data", Stage: "succeeded", ResultJSON: string(raw)}).Error; err != nil {
				t.Fatal(err)
			}
			projected := modelObservationForRun(run, receipt, nil)
			var data map[string]any
			if err := json.Unmarshal(projected.Data, &data); err != nil {
				t.Fatal(err)
			}
			if data["handle"] != "source" {
				t.Fatalf("source handle replaced by result receipt: %v", data["handle"])
			}
			if data["receipt_handle"] != call.ID {
				t.Fatalf("result receipt identity lost: %v", data["receipt_handle"])
			}
			follow, _ := json.Marshal(map[string]any{"handle": data["handle"], "view": view, "selector": "#article"})
			if _, err := s.readData(context.Background(), run, ToolCall{Function: FunctionCall{Name: "read_data", Arguments: string(follow)}}); err != nil {
				t.Fatal("source reference unusable", err)
			}
			read, _ := json.Marshal(data["read_with"])
			if _, err := s.readData(context.Background(), run, ToolCall{Function: FunctionCall{Name: "read_data", Arguments: string(read)}}); err != nil {
				t.Fatal("result receipt reference unusable", err)
			}
			foreign := run
			foreign.ID = "other-run"
			if _, err := s.readData(context.Background(), foreign, ToolCall{Function: FunctionCall{Name: "read_data", Arguments: string(follow)}}); err == nil {
				t.Fatal("source reference escaped Run ownership")
			}
		})
	}
}
