package aiapp

import (
	"context"
	"encoding/json"
	"fmt"
	"go-lua-crawler/internal/agentdata"
	"testing"
)

func TestLoopStopsIdenticalObservationsButAllowsNewPagesAndVersions(t *testing.T) {
	for _, name := range []string{"read_observation", "get_task_results"} {
		for _, mode := range []string{"repeat", "new_page", "new_version"} {
			t.Run(name+"/"+mode, func(t *testing.T) {
				state := decisionState()
				state.Result = &TaskResult{Coverage: CoverageReport{PartialValidRecords: 1}}
				for n := 0; n < 4; n++ {
					offset, version := 0, "v1"
					if mode == "new_page" {
						offset = n
					}
					if mode == "new_version" {
						version = fmt.Sprintf("v%d", n)
					}
					args := map[string]any{"pointer": "/data/records", "offset": offset}
					call := ToolCall{ID: fmt.Sprintf("call%d", n), Function: FunctionCall{Name: name}}
					argsRaw, _ := json.Marshal(args)
					call.Function.Arguments = string(argsRaw)
					out, _ := toolData(call, map[string]any{"version": version, "text": "same observed evidence"})
					raw, _ := json.Marshal(out)
					state.Operations = append(state.Operations, agentdata.Operation{Kind: "model", Stage: "succeeded"}, agentdata.Operation{Kind: name, Stage: "succeeded", InputHash: agentdata.Hash(call.Function), ResultJSON: string(raw)})
				}
				next, err := RunTaskLoop(context.Background(), state)
				if err != nil {
					t.Fatal(err)
				}
				if mode == "repeat" && (next.Kind != "finish" || next.Status != "partial") {
					t.Fatalf("identical unchanged observation did not stop: %+v", next)
				}
				if mode != "repeat" && next.Kind != "model" {
					t.Fatalf("legitimate progress stopped: %+v", next)
				}
			})
		}
	}
}
