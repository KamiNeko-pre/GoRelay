package aiapp

import (
	"context"
	"encoding/json"
	"go-lua-crawler/internal/agentdata"
	task "go-lua-crawler/internal/task"
	"testing"
	"time"
)

func TestPendingSiblingDoesNotHideNewTerminalChildFeedback(t *testing.T) {
	for _, status := range []task.Status{task.StatusSucceeded, task.StatusFailed} {
		t.Run(string(status), func(t *testing.T) {
			state := decisionState()
			at := time.Now().Add(-time.Minute)
			state.TaskResults = []ChildResult{{Task: task.CrawlTask{ID: 1, Status: task.StatusRunning}}, {Task: task.CrawlTask{ID: 2, Status: status, FinishedAt: &at}}}
			action, err := RunTaskLoop(context.Background(), state)
			if err != nil || action.Kind != "model" {
				t.Fatalf("new terminal child blocked by slow sibling: %+v %v", action, err)
			}
			// The model has consumed this exact terminal feedback. Waiting now
			// avoids polling the model just because another child is still running.
			terminal := state
			terminal.TaskResults = state.TaskResults[1:]
			raw, _ := json.Marshal(map[string]any{"observed_children": agentdata.Hash(loopTaskFacts(terminal))})
			state.Operations = []agentdata.Operation{{Kind: "model", Stage: "succeeded", ResultJSON: string(raw)}}
			action, err = RunTaskLoop(context.Background(), state)
			if err != nil || action.Kind != "wait" {
				t.Fatalf("unchanged pending work spins model: %+v %v", action, err)
			}
			state.TaskResults[0].Task.Status = task.StatusSucceeded
			action, err = RunTaskLoop(context.Background(), state)
			if err != nil || action.Kind != "model" {
				t.Fatalf("all terminal children not observed: %+v %v", action, err)
			}
		})
	}
	state := decisionState()
	state.TaskResults = []ChildResult{{Task: task.CrawlTask{ID: 1, Status: task.StatusQueued}}}
	action, err := RunTaskLoop(context.Background(), state)
	if err != nil || action.Kind != "wait" {
		t.Fatal("all-pending tasks must wait without model polling")
	}
}
