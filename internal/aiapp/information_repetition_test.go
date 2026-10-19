package aiapp

import (
	"encoding/json"
	"fmt"
	"go-lua-crawler/internal/agentdata"
	"testing"
)

func informationChurn(rounds int, productive bool) []agentdata.Operation {
	var ops []agentdata.Operation
	for n := 0; n < rounds; n++ {
		ops = append(ops, agentdata.Operation{Kind: "model", Stage: "succeeded"})
		for j := 0; j < 1+n%3; j++ {
			text := "already read source facts"
			if productive {
				text = fmt.Sprintf("distinct evidence %d %d", n, j)
			}
			data, _ := json.Marshal(map[string]any{"view_version": "v1", "offset": n, "next_cursor": fmt.Sprint(n, j), "text": text})
			raw, _ := json.Marshal(ToolResult{Name: "read_data", CallID: fmt.Sprint(n, j), Data: data})
			ops = append(ops, agentdata.Operation{Kind: "read_data", Stage: "succeeded", InputHash: fmt.Sprint(n, j), ResultJSON: string(raw)})
		}
	}
	return ops
}
func TestInformationRepetitionSurvivesBatchAndCursorChurn(t *testing.T) {
	for _, productive := range []bool{false, true} {
		s := decisionState()
		s.Budget.LoopVersion = 5
		s.Operations = informationChurn(6, productive)
		feedback := repeatedReadInformation(s.Operations)
		if (feedback != nil) == productive {
			t.Fatalf("productive=%v feedback=%+v", productive, feedback)
		}
	}
}

func TestReadInformationRecoveryReachesModelAndAllowsNewSources(t *testing.T) {
	s := decisionState()
	s.Budget.LoopVersion = 5
	s.Operations = informationChurn(6, false)
	raw, err := contextFacts(s)
	if err != nil {
		t.Fatal(err)
	}
	var facts map[string]any
	_ = json.Unmarshal(raw, &facts)
	if facts["read_information_recovery"] == nil {
		t.Fatal("diagnostic not delivered to model context")
	}
	// Same wording at another source is new material, not a repeat of that
	// source. Identity is the resource's URL — a handle is minted per fetch, so
	// changing it does not describe a different source.
	op := &s.Operations[len(s.Operations)-1]
	var reply ToolResult
	_ = json.Unmarshal([]byte(op.ResultJSON), &reply)
	var data map[string]any
	_ = json.Unmarshal(reply.Data, &data)
	data["url"] = "https://another.example.test/notice"
	reply.Data, _ = json.Marshal(data)
	encoded, _ := json.Marshal(reply)
	op.ResultJSON = string(encoded)
	if repeatedReadInformation(s.Operations) != nil {
		t.Fatal("new source mistaken for unchanged evidence")
	}
}
