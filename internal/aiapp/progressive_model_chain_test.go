package aiapp

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"go-lua-crawler/internal/agentdata"
)

type progressiveCase struct {
	Name              string
	WrongPercent      int
	Fault             string
	LongReads         bool
	Delay             time.Duration
	Expected          string
	AfterAcquisition  bool
	CompleteText      bool
	ExtractiveAnswer  bool
	ActualRunner      bool
	KnowledgeFirst    bool
	PartialKnowledge  bool
	SelectorDiscovery bool
	AdaptiveCrawler   bool
	LayoutVariant     int
}

func TestKnowledgeFirstIndependentRunsFullChain(t *testing.T) {
	runCorrectModelSubsystemChain(t, false, 3, false, "", progressiveCase{Name: "knowledge-first-reuse-refresh", KnowledgeFirst: true, Expected: "completed"})
}

func TestPartialKnowledgeSupplementsOnlyMissingFieldsFullChain(t *testing.T) {
	runCorrectModelSubsystemChain(t, false, 2, false, "", progressiveCase{Name: "partial-knowledge-supplement", KnowledgeFirst: true, PartialKnowledge: true, Expected: "completed"})
}

func TestProgressiveOracleUsesPriorValidatedHostCandidate(t *testing.T) {
	valid, _ := toolData(ToolCall{ID: "prior-valid", Function: FunctionCall{Name: "validate_program"}}, map[string]any{"id": "candidate_from_host", "artifact_hash": "host_hash"})
	goodJSON, _ := json.Marshal(valid)
	badJSON, _ := json.Marshal(ToolResult{CallID: "later-invalid", Name: "validate_program", Error: "syntax rejected"})
	req := ChatRequest{Tools: []ToolDefinition{{Type: "function"}}, Messages: []Message{{Role: "tool", Content: string(goodJSON)}, {Role: "tool", Content: string(badJSON)}}}
	out, _, err := progressiveChainDecision(req, progressiveCase{WrongPercent: 50, Fault: "invalid_lua"}, 2)
	if err != nil || len(out.Message.ToolCalls) != 1 || out.Message.ToolCalls[0].Function.Name != "register_source" {
		t.Fatal("oracle forgot an already validated candidate after model error", out, err)
	}
}

func TestProgressiveModelFullChain(t *testing.T) {
	for _, rate := range []int{0, 25, 50, 75} {
		t.Run(fmt.Sprintf("wrong_%d", rate), func(t *testing.T) {
			scenario := progressiveCase{Name: fmt.Sprintf("wrong-%d", rate), WrongPercent: rate, Fault: "invalid_lua", Expected: "completed"}
			runCorrectModelSubsystemChain(t, true, 1, false, "", scenario)
		})
	}
}

func TestProductiveLongModelFullChain(t *testing.T) {
	// A two-call work window is deliberately much smaller than the total
	// necessary model turns. Only observed work may renew it.
	t.Setenv("GORELAY_EXPERIMENT_UNLIMITED_BUDGET", "0")
	t.Setenv("GORELAY_RUN_MODEL_CALLS", "2")
	runCorrectModelSubsystemChain(t, true, 1, false, "", progressiveCase{Name: "productive-long-reads", LongReads: true, Delay: 150 * time.Millisecond, Expected: "completed"})
}

func TestAllWrongModelFullChain(t *testing.T) {
	for _, fault := range []string{"invalid_lua", "empty_action", "outside_scope", "malformed_args", "hallucination", "wrong_goal", "code_churn", "self_receipt_read"} {
		t.Run(fault, func(t *testing.T) {
			runCorrectModelSubsystemChain(t, true, 1, false, "", progressiveCase{Name: "all-wrong-" + fault, WrongPercent: 100, Fault: fault, Expected: "failed"})
		})
	}
}

func TestPartialFallbackAfterModelStopsProgress(t *testing.T) {
	runCorrectModelSubsystemChain(t, true, 1, false, "", progressiveCase{Name: "partial-fallback", WrongPercent: 100, Fault: "invalid_lua", Expected: "partial", AfterAcquisition: true})
}

func TestAutonomousExplorationFullChain(t *testing.T) {
	for _, recover := range []bool{true, false} {
		name, fault, expected := "recovery", "recover_exploration", "completed"
		if !recover {
			name, fault, expected = "drift-stop", "novel_exploration", "failed"
		}
		t.Run(name, func(t *testing.T) {
			runCorrectModelSubsystemChain(t, true, 1, false, "", progressiveCase{Name: name, WrongPercent: 100, Fault: fault, Expected: expected})
		})
	}
}

func TestRejectedModelEnvelopeFullChain(t *testing.T) {
	for _, always := range []bool{false, true} {
		name, expected := "format-recovery", "completed"
		if always {
			name, expected = "format-all-wrong", "failed"
		}
		rate := 0
		if always {
			rate = 100
		}
		t.Run(name, func(t *testing.T) {
			runCorrectModelSubsystemChain(t, true, 1, false, "", progressiveCase{Name: name, WrongPercent: rate, Fault: "invalid_envelope", Expected: expected})
		})
	}
}

func TestCompleteSourceAndGroundedSummaryFullChain(t *testing.T) {
	runCorrectModelSubsystemChain(t, true, 1, false, "", progressiveCase{Name: "complete-source-grounded-answer", CompleteText: true, ExtractiveAnswer: true, Expected: "completed"})
}

func TestLegalButIncompleteProgramFullChain(t *testing.T) {
	for _, rate := range []int{25, 100} {
		expected := "completed"
		if rate == 100 {
			expected = "failed"
		}
		t.Run(fmt.Sprint(rate), func(t *testing.T) {
			runCorrectModelSubsystemChain(t, true, 1, false, "", progressiveCase{Name: fmt.Sprintf("supported-excerpt-%d", rate), WrongPercent: rate, Fault: "supported_excerpt", CompleteText: true, ExtractiveAnswer: true, Expected: expected})
		})
	}
}

func progressiveChainDecision(request ChatRequest, scenario progressiveCase, ordinal int64) (ChatResponse, bool, error) {
	if scenario.KnowledgeFirst && len(request.Tools) > 0 {
		out, err := knowledgeFirstDecision(request)
		if scenario.PartialKnowledge {
			out, err = partialKnowledgeDecision(request)
		}
		return out, false, err
	}
	if len(request.Tools) == 0 {
		out, err := correctChainDecision(request)
		return out, false, err
	}
	receipts, facts := auditVisibleReceipts(request)
	var children []any
	_ = json.Unmarshal(facts["children"], &children)
	wrong := (ordinal-1)%100 < int64(scenario.WrongPercent)
	// Small deterministic blocks spread errors throughout the task, rather
	// than injecting one prefix and letting the remaining task run correctly.
	switch scenario.WrongPercent {
	case 25:
		wrong = ordinal%4 == 1
	case 50:
		wrong = ordinal%2 == 1
	case 75:
		wrong = ordinal%4 != 0
	}
	if scenario.AfterAcquisition && len(children) == 0 {
		wrong = false
	}
	if scenario.Fault == "recover_exploration" && (len(facts["exploration_recovery"]) > 0 || receipts["validate_program"].Name != "" || receipts["register_source"].Name != "" || len(children) > 0) {
		// Switch only in response to the application's actual host feedback.
		// The harness neither resumes the run nor provides source/code hints.
		wrong = false
	}
	propose := func(name string, args any) ChatResponse {
		raw, _ := json.Marshal(args)
		if scenario.CompleteText && !wrong && name == "validate_program" {
			raw = []byte(strings.ReplaceAll(string(raw), "html_find(h,\\\"div.v_news_content\\\")", "source_text(h)"))
		}
		if scenario.ActualRunner {
			raw = []byte(strings.ReplaceAll(string(raw), "https://fixture.example", "http://8.8.8.8"))
		}
		return ChatResponse{Message: Message{Role: "assistant", ToolCalls: []ToolCall{{ID: agentdata.Hash(request)[:24], Type: "function", Function: FunctionCall{Name: name, Arguments: string(raw)}}}}, Model: "progressive-mock"}
	}
	if wrong {
		switch scenario.Fault {
		case "alternating_review":
			name := "get_task_results"
			if ordinal%2 == 0 {
				name = "evaluate_result"
			}
			return propose(name, map[string]any{}), true, nil
		case "self_receipt_read":
			if out := receipts["read_data"]; out.Name != "" {
				return propose("read_data", map[string]any{"handle": out.CallID, "view": "json", "pointer": "/text", "limit": 64}), true, nil
			}
			if out := receipts["inspect_source"]; out.Name != "" {
				return propose("read_data", map[string]any{"handle": out.CallID, "view": "content", "limit": 64}), true, nil
			}
			return propose("inspect_source", map[string]any{"url": "http://8.8.8.8/navigation/seed"}), true, nil
		case "novel_exploration", "recover_exploration":
			return propose("inspect_source", map[string]any{"url": fmt.Sprintf("http://8.8.8.8/navigation/%d", ordinal)}), true, nil
		case "empty_action":
			return ChatResponse{Message: Message{Role: "assistant", Content: fmt.Sprintf("I claim to make progress %d but provide no executable action", ordinal)}, Model: "progressive-mock"}, true, nil
		case "outside_scope":
			return propose("inspect_source", map[string]any{"url": "http://127.0.0.1/private"}), true, nil
		case "wrong_goal":
			return propose("plan_task", map[string]any{"required_fields": []map[string]string{{"name": "unrequested", "type": "string"}}}), true, nil
		case "malformed_args":
			out := propose("validate_program", nil)
			out.Message.ToolCalls[0].Function.Arguments = "{broken"
			return out, true, nil
		case "hallucination":
			return propose("validate_program", map[string]any{"code": strings.ReplaceAll(correctProcurementLua, "body=body", "body=\"invented unsupported facts\""), "kind": "adapter", "source_id": "audit-source", "purpose": "complete public notices", "url": "https://fixture.example/releases"}), true, nil
		case "code_churn":
			return propose("validate_program", map[string]any{"code": correctProcurementLua + fmt.Sprintf("\n-- new spelling %d", ordinal), "kind": "adapter", "source_id": "audit-source", "purpose": "repeatedly recreate identical output without publication", "url": "https://fixture.example/releases"}), true, nil
		case "supported_excerpt":
			code := strings.Replace(correctProcurementLua, `local body=assert(html_find(h,"div.v_news_content"));`, `local body_selector="div.v_news_content p";local body=html_find(h,body_selector);if not body or body=="" then body_selector="div.v_news_content";body=assert(html_find(h,body_selector)) end;`, 1)
			code = strings.Replace(code, `body={selector="div.v_news_content",index=0}`, `body={selector=body_selector,index=0}`, 1)
			return propose("validate_program", map[string]any{"code": code, "kind": "adapter", "source_id": "audit-source", "purpose": "collect sourced but incomplete paragraphs", "url": "https://fixture.example/releases"}), true, nil
		default:
			return propose("validate_program", map[string]any{"code": "return true,{items=}", "kind": "adapter", "source_id": "audit-source", "purpose": "complete public notices", "url": "https://fixture.example/releases"}), true, nil
		}
	}
	if scenario.LongReads && len(children) > 0 {
		if receipt := receipts["read_data"]; receipt.Name != "" && receipt.Error == "" {
			var data struct {
				Handle string `json:"handle"`
				Offset int    `json:"offset"`
				Text   string `json:"text"`
				Cursor string `json:"next_cursor"`
			}
			_ = json.Unmarshal(receipt.Data, &data)
			if data.Cursor != "" && len([]rune(data.Text)) < 1000 {
				return propose("read_data", map[string]any{"cursor": data.Cursor}), false, nil
			}
			if data.Cursor == "" && data.Offset > 0 {
				return propose("read_data", map[string]any{"handle": data.Handle, "pointer": "/text", "limit": 1000}), false, nil
			}
		}
	}
	if len(children) == 0 {
		if out := receipts["register_source"]; out.Name != "" && out.Error == "" {
			return propose("submit_acquisition", map[string]any{"source_id": "audit-source", "url": "https://fixture.example/releases"}), false, nil
		}
		// The application, rather than the model's private state, owns the
		// prior successful candidate. An intervening error must not hide it.
		for n := len(request.Messages) - 1; n >= 0; n-- {
			m := request.Messages[n]
			if m.Role != "tool" {
				continue
			}
			visible := []ToolResult{}
			var snapshot struct {
				Feedback []ToolResult `json:"recent_tool_feedback"`
			}
			_ = json.Unmarshal([]byte(m.Content), &snapshot)
			visible = append(visible, snapshot.Feedback...)
			var direct ToolResult
			if json.Unmarshal([]byte(m.Content), &direct) == nil && direct.Name != "" {
				visible = append(visible, direct)
			}
			for _, out := range visible {
				if out.Name != "validate_program" || out.Error != "" {
					continue
				}
				var data map[string]json.RawMessage
				_ = json.Unmarshal(out.Data, &data)
				if data["observation"] != nil {
					var projected map[string]json.RawMessage
					if json.Unmarshal(data["observation"], &projected) == nil {
						data = projected
					}
				}
				var id string
				_ = json.Unmarshal(data["id"], &id)
				if id != "" {
					return propose("register_source", map[string]any{"candidate_id": id, "description": "validated host capability"}), false, nil
				}
			}
		}
		if out := receipts["validate_program"]; out.Name != "" && out.Error != "" {
			return propose("validate_program", map[string]any{"code": correctProcurementLua, "kind": "adapter", "source_id": "audit-source", "purpose": "complete public notices", "url": "https://fixture.example/releases"}), false, nil
		}
	}
	out, err := correctChainDecision(request)
	if scenario.CompleteText && len(out.Message.ToolCalls) == 1 && out.Message.ToolCalls[0].Function.Name == "validate_program" {
		out.Message.ToolCalls[0].Function.Arguments = strings.ReplaceAll(out.Message.ToolCalls[0].Function.Arguments, "html_find(h,\\\"div.v_news_content\\\")", "source_text(h)")
	}
	if scenario.ActualRunner {
		for n := range out.Message.ToolCalls {
			out.Message.ToolCalls[n].Function.Arguments = strings.ReplaceAll(out.Message.ToolCalls[n].Function.Arguments, "https://fixture.example", "http://8.8.8.8")
		}
	}
	if scenario.LongReads && len(children) > 0 && len(out.Message.ToolCalls) == 1 && out.Message.ToolCalls[0].Function.Name == "read_data" {
		var args map[string]any
		_ = json.Unmarshal([]byte(out.Message.ToolCalls[0].Function.Arguments), &args)
		if args["limit"] != nil {
			args["limit"] = 80
			raw, _ := json.Marshal(args)
			out.Message.ToolCalls[0].Function.Arguments = string(raw)
		}
	}
	return out, false, err
}

func TestExplorationMockRetainsRecoveryAfterMilestone(t *testing.T) {
	out, _ := toolData(ToolCall{ID: "validated", Function: FunctionCall{Name: "validate_program"}}, map[string]string{"id": "host_candidate", "artifact_hash": "host_artifact"})
	raw, _ := json.Marshal(out)
	req := ChatRequest{Tools: []ToolDefinition{{Type: "function"}}, Messages: []Message{{Role: "tool", Content: string(raw)}}}
	response, wrong, err := progressiveChainDecision(req, progressiveCase{WrongPercent: 100, Fault: "recover_exploration"}, 99)
	if err != nil || wrong || len(response.Message.ToolCalls) != 1 || response.Message.ToolCalls[0].Function.Name != "register_source" {
		t.Fatal("recovery mock returned to exploration after verified progress", response, wrong, err)
	}
}
