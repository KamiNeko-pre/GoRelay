package aiapp

import (
	"encoding/json"
	"go-lua-crawler/internal/agentdata"
	"strings"
	"testing"
)

func TestFailureCausesSurviveDurableFeedbackAndCompaction(t *testing.T) {
	for _, name := range []string{"validate_program", "submit_extraction", "read_data", "finish_task"} {
		state := decisionState()
		state.Run.ID = "failure-feedback"
		state.Run.BudgetJSON = `{"loop_version":5}`
		state.Budget.LoopVersion = 5
		cause := "original cause from " + name
		call := ToolCall{ID: "failed", Function: FunctionCall{Name: name, Arguments: `{}`}}
		receipt := ToolResult{CallID: call.ID, Name: name, Error: cause}
		raw, _ := json.Marshal(receipt)
		state.Operations = []agentdata.Operation{{ID: 1, RunID: state.Run.ID, Key: "tool:failed", Kind: name, Stage: "failed", ResultJSON: string(raw)}}
		state.Messages = []Message{{Role: "assistant", ToolCalls: []ToolCall{call}}, {Role: "tool", ToolCallID: call.ID, Content: string(raw)}}
		feedback := recentToolFeedback(state)
		if len(feedback) != 1 || feedback[0].Error != cause {
			t.Fatal("durable failure changed", name, feedback)
		}
		messages, err := CompactContext(state, state.Messages)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, m := range messages {
			if strings.Contains(m.Content, cause) {
				if m.Role != "tool" {
					t.Fatal("failure promoted to system instructions")
				}
				found = true
			}
		}
		if !found {
			t.Fatal("failure lost before model", name)
		}
	}
}

func TestFailedReadRecoveryDataIsNotTreatedAsLegacyContent(t *testing.T) {
	for _, cause := range []string{"selector does not exist", "reader offset is invalid"} {
		state := decisionState()
		state.Run.ID = "failed-read"
		state.Run.BudgetJSON = `{"loop_version":5}`
		state.Budget.LoopVersion = 5
		call := ToolCall{ID: "failure", Function: FunctionCall{Name: "read_data", Arguments: `{"handle":"page","view":"dom","selector":".missing"}`}}
		receipt := ToolResult{CallID: call.ID, Name: "read_data", Error: cause, Data: json.RawMessage(`{"handle":"page","selector_catalog":[{"selector":"h6","sample":"Actual title"}],"next_step":"select an observed node"}`)}
		raw, _ := json.Marshal(receipt)
		state.Operations = []agentdata.Operation{{RunID: state.Run.ID, Key: "tool:failure", Kind: "read_data", Stage: "failed", ResultJSON: string(raw)}}
		state.Messages = []Message{{Role: "assistant", ToolCalls: []ToolCall{call}}, {Role: "tool", ToolCallID: call.ID, Content: string(raw)}}
		for _, got := range recentToolFeedback(state) {
			if !strings.Contains(string(got.Data), "Actual title") {
				t.Fatalf("failure recovery facts lost: %s", got.Data)
			}
		}
	}
}

func TestRejectedModelProposalExposesItsExactCorrectionCause(t *testing.T) {
	for _,reason:=range []string{"invalid model proposal at function.arguments: truncated JSON", "invalid model proposal at choices: expected one chat choice, got 2"} {
		state:=decisionState();state.Run.ID="model-correction";state.Run.BudgetJSON=`{"loop_version":5}`;state.Budget.LoopVersion=5
		raw,_:=json.Marshal(ChatResponse{Failure:&FailureInfo{Kind:"rejected_proposal",Message:reason}})
		state.Operations=[]agentdata.Operation{{RunID:state.Run.ID,Kind:"model",Stage:"failed",ResultJSON:string(raw)}}
		messages,err:=CompactContext(state,nil);if err!=nil {t.Fatal(err)}
		found:=false
		for _,m:=range messages {if strings.Contains(m.Content,reason) {if m.Role!="tool" {t.Fatal("model error promoted to system instructions")};found=true}}
		if !found {t.Fatal("model asked to correct an envelope without its rejected path and cause")}
	}
}
