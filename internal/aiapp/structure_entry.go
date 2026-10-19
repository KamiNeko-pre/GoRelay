package aiapp

import (
	"encoding/json"
	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/agentdata"
)

// SourceStructure is a pure projection for other acquisition frontends. It
// creates no crawler Run and adds no knowledge/prose tool to that loop.
func SourceStructure(source acquisition.Response) json.RawMessage {
	inspection := inspectResponse(source, acquisition.Stats{Requests: 1, Bytes: source.Receipt.Bytes})
	out, _ := toolData(ToolCall{ID: "source", Function: FunctionCall{Name: "inspect_source"}}, inspection)
	return modelObservationForRun(agentdata.Run{BudgetJSON: `{"loop_version":5}`}, out, nil).Data
}
