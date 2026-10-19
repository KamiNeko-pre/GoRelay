package aiapp

import (
	"encoding/json"
	"fmt"

	"go-lua-crawler/internal/agentdata"
)

// ModelCallAccounting identifies one persisted physical attempt. Restoring its
// receipt does not create another attempt. Nil Usage is unknown, including a
// reserved attempt whose provider outcome was never durably acknowledged.
type ModelCallAccounting struct {
	ID             string `json:"id"`
	ParentID       string `json:"parent_id"`
	Scope          string `json:"scope"`
	Purpose        string `json:"purpose"`
	Stage          string `json:"stage"`
	InputHash      string `json:"input_hash,omitempty"`
	PromptHash     string `json:"prompt_hash,omitempty"`
	ToolSchemaHash string `json:"tool_schema_hash,omitempty"`
	Model          string `json:"model,omitempty"`
	RequestID      string `json:"provider_request_id,omitempty"`
	DurationMS     int64  `json:"duration_ms"`
	Usage          *Usage `json:"usage"`
	Error          string `json:"error,omitempty"`
}

func CrawlerModelCalls(ops []agentdata.Operation) []ModelCallAccounting {
	out := []ModelCallAccounting{}
	for _, op := range ops {
		call := ModelCallAccounting{ID: fmt.Sprintf("crawler:%s/operation:%d", op.RunID, op.ID), ParentID: "crawler:" + op.RunID, Scope: "crawler", Purpose: op.Kind, Stage: op.Stage, InputHash: op.InputHash}
		switch op.Kind {
		case "model":
			var response ChatResponse
			if err := json.Unmarshal([]byte(op.ResultJSON), &response); err != nil {
				call.Error = "decode response: " + err.Error()
			}
			call.Model, call.RequestID, call.DurationMS = response.Model, response.RequestID, response.DurationMS
			if response.ContextProfile != nil {
				call.PromptHash, call.ToolSchemaHash = response.ContextProfile.PromptHash, response.ContextProfile.ToolSchemaHash
			}
			if err := json.Unmarshal([]byte(op.UsageJSON), &call.Usage); err != nil {
				call.Error = "decode usage: " + err.Error()
			}
		case "discover_sources":
			var result ToolResult
			var response SearchResponse
			_ = json.Unmarshal([]byte(op.ResultJSON), &result)
			_ = json.Unmarshal(result.Data, &response)
			if response.Model == "" && response.Usage == nil {
				continue
			}
			call.Model, call.RequestID, call.DurationMS, call.Usage = response.Model, response.RequestID, response.DurationMS, response.Usage
		default:
			continue
		}
		out = append(out, call)
	}
	return out
}
