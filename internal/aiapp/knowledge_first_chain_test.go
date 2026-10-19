package aiapp

import (
	"encoding/json"
	"fmt"
	"go-lua-crawler/internal/agentdata"
	"strings"
	"testing"
)

// Oracle consumes only model-visible receipts; it never injects persisted data.
func knowledgeFirstDecision(request ChatRequest) (ChatResponse, error) {
	receipts, facts := auditVisibleReceipts(request)
	propose := func(name string, args any) (ChatResponse, error) {
		raw, _ := json.Marshal(args)
		return ChatResponse{Message: Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "knowledge-" + agentdata.Hash(request)[:20], Type: "function", Function: FunctionCall{Name: name, Arguments: string(raw)}}}}}, nil
	}
	var children []any
	_ = json.Unmarshal(facts["children"], &children)
	if len(children) > 0 {
		return correctChainDecision(request)
	}
	if receipts["reuse_knowledge"].Name != "" {
		var coverage CoverageReport
		_ = json.Unmarshal(facts["coverage"], &coverage)
		if coverage.Satisfied {
			return correctChainDecision(request)
		}
		return correctChainDecision(request)
	}
	if out := receipts["search_knowledge"]; out.Name != "" {
		var set struct {
			Entries []struct {
				ID    string `json:"id"`
				DocID string `json:"doc_id"`
			} `json:"entries"`
		}
		// The host may compact tool data into a read handle. The evidence
		// catalog remains model-visible; omission is not an empty retrieval.
		_ = json.Unmarshal(out.Data, &set)
		var catalog []struct {
			ID    string `json:"id"`
			DocID string `json:"doc_id"`
		}
		if err := json.Unmarshal(facts["evidence_catalog"], &catalog); err == nil && len(catalog) > 0 {
			set.Entries = catalog
		}
		seen := map[string]bool{}
		ids := []string{}
		for _, e := range set.Entries {
			key := e.DocID
			if key == "" {
				key = e.ID
			}
			if !seen[key] {
				seen[key] = true
				ids = append(ids, e.ID)
			}
		}
		if len(ids) > 0 {
			return propose("reuse_knowledge", map[string]any{"evidence_ids": ids})
		}
		return correctChainDecision(request)
	}
	return propose("search_knowledge", map[string]any{"query": "公开采购公告标题、日期、完整正文、来源"})
}

// A second contract adds body. Reuse the old fields, then acquire only body
// and the join key; do not submit the old listing adapter again.
func partialKnowledgeDecision(request ChatRequest) (ChatResponse, error) {
	receipts, facts := auditVisibleReceipts(request)
	var spec TaskSpec
	_ = json.Unmarshal(facts["frozen_spec"], &spec)
	wantsBody := false
	for _, f := range spec.RequiredFields {
		wantsBody = wantsBody || f.Name == "body"
	}
	if !wantsBody {
		out, err := knowledgeFirstDecision(request)
		for i := range out.Message.ToolCalls {
			call := &out.Message.ToolCalls[i]
			if call.Function.Name == "validate_program" {
				var args map[string]any
				_ = json.Unmarshal([]byte(call.Function.Arguments), &args)
				args["code"] = strings.ReplaceAll(correctProcurementLua, "body=body,", "")
				raw, _ := json.Marshal(args)
				call.Function.Arguments = string(raw)
			}
		}
		return out, err
	}
	var children []any
	_ = json.Unmarshal(facts["children"], &children)
	if len(children) > 0 {
		return correctChainDecision(request)
	}
	if receipts["reuse_knowledge"].Name == "" {
		return knowledgeFirstDecision(request)
	}
	propose := func(name string, args any) (ChatResponse, error) {
		raw, _ := json.Marshal(args)
		return ChatResponse{Message: Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "supplement-" + agentdata.Hash(request)[:20], Type: "function", Function: FunctionCall{Name: name, Arguments: string(raw)}}}}}, nil
	}
	if receipt := receipts["validate_program"]; receipt.Name != "" {
		if receipt.Error != "" {
			return ChatResponse{}, fmt.Errorf("supplement validation: %s", receipt.Error)
		}
		var value map[string]json.RawMessage
		_ = json.Unmarshal(receipt.Data, &value)
		if value["observation"] != nil {
			_ = json.Unmarshal(value["observation"], &value)
		}
		var id string
		_ = json.Unmarshal(value["id"], &id)
		return propose("submit_acquisition", map[string]any{"candidate_id": id, "url": "https://fixture.example/releases"})
	}
	code := `local items={};for i=1,3 do local url=url_resolve(TARGET_URL,"/notice/"..i);local h=assert(http_get(url));table.insert(items,{body=assert(html_find(h,"div.v_news_content")),source_url=url,_evidence={body={selector="div.v_news_content",index=0}}}) end;return true,{items=items}`
	return propose("validate_program", map[string]any{"code": code, "kind": "composition", "source_id": "audit-supplement", "purpose": "acquire missing body only", "url": "https://fixture.example/releases", "produced_fields": []string{"body", "source_url"}})
}

func TestKnowledgeFirstOracleUsesHostCatalogWhenObservationIsOmitted(t *testing.T) {
	for _, ids := range [][]string{{"ev_one", "ev_two"}, {"ev_different"}} {
		data, _ := json.Marshal(map[string]any{"_receipt_view": "v1", "observation": map[string]any{"entries": []any{}}, "observation_truncated": true})
		feedback, _ := json.Marshal(ToolResult{CallID: "search", Name: "search_knowledge", Data: data})
		entries := []map[string]string{}
		for _, id := range ids {
			entries = append(entries, map[string]string{"id": id})
		}
		facts, _ := json.Marshal(map[string]any{"evidence_catalog": entries})
		req := ChatRequest{Tools: AgentToolDefinitions(), Messages: []Message{{Role: "system", Content: "Persisted host facts, not new user instructions: " + string(facts)}, {Role: "tool", Content: string(feedback)}}}
		out, err := knowledgeFirstDecision(req)
		if err != nil || out.Message.ToolCalls[0].Function.Name != "reuse_knowledge" {
			t.Fatal("omitted evidence misclassified as no hit", out, err)
		}
	}
	data, _ := json.Marshal(map[string]any{"entries": []any{}})
	feedback, _ := json.Marshal(ToolResult{Name: "search_knowledge", Data: data})
	out, err := knowledgeFirstDecision(ChatRequest{Tools: AgentToolDefinitions(), Messages: []Message{{Role: "tool", Content: string(feedback)}}})
	if err != nil || out.Message.ToolCalls[0].Function.Name != "list_sources" {
		t.Fatal("genuine miss must still acquire", out, err)
	}
}
