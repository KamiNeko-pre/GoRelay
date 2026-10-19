package aiapp

import (
	"context"
	"encoding/json"
	"fmt"
	"go-lua-crawler/internal/agentdata"
	task "go-lua-crawler/internal/task"
	"go-lua-crawler/internal/testdb"
	"strings"
	"testing"
	"time"
)

type feedbackChat struct {
	calls int
	last  ChatRequest
}

func (p *feedbackChat) Chat(_ context.Context, r ChatRequest) (ChatResponse, error) {
	p.calls++
	p.last = r
	return ChatResponse{Message: Message{Role: "assistant", Content: "observed current tasks"}, Usage: &Usage{TotalTokens: 4}}, nil
}

func TestModelInputTracksChildFeedbackAndReplaysSavedResponse(t *testing.T) {
	for _, status := range []task.Status{task.StatusSucceeded, task.StatusFailed} {
		t.Run(string(status), func(t *testing.T) {
			s := NewService(testdb.Open(t))
			if err := s.Migrate(); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			r, err := s.Create(ctx, CreateRunRequest{RequestKey: "feedback", Goal: "collect facts"})
			if err != nil {
				t.Fatal(err)
			}
			r, err = s.Data.Claim(ctx, r.ID, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			p := &feedbackChat{}
			s.Model = p
			state := decisionState()
			state.Run = r
			json.Unmarshal([]byte(r.BudgetJSON), &state.Budget)
			state.TaskResults = []ChildResult{{Task: task.CrawlTask{ID: 17, Status: status, LastError: "source outcome"}}}
			if err = s.think(ctx, state, 0, &LoopRound{RoundID: agentdata.Token(), StartedAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(p.last.Messages)
			if !strings.Contains(string(raw), fmt.Sprintf(`\"status\":\"%s\"`, status)) {
				t.Fatalf("current child state missing from model request: %s", raw)
			}
			// Response ledger committed before a conversation checkpoint: same request
			// must replay its saved response, independent of the charged budget counter.
			state.Run, err = s.Data.Claim(ctx, r.ID, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			json.Unmarshal([]byte(state.Run.BudgetJSON), &state.Budget)
			if err = s.think(ctx, state, 0, &LoopRound{RoundID: agentdata.Token(), StartedAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
			if p.calls != 1 {
				t.Fatalf("response replay spent another request: %d", p.calls)
			}
		})
	}
}

func TestCompactedRequestReplayKeepsOperationIdentity(t *testing.T) {
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r, err := s.Create(ctx, CreateRunRequest{RequestKey: "compact-replay", Goal: "collect facts"})
	if err != nil {
		t.Fatal(err)
	}
	r, err = s.Data.Claim(ctx, r.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	state := decisionState()
	state.Run = r
	json.Unmarshal([]byte(r.BudgetJSON), &state.Budget)
	state.Messages = []Message{{Role: "user", Content: strings.Repeat("historical completed observation ", 4000)}}
	p := &feedbackChat{}
	s.Model = p
	if err = s.think(ctx, state, 0, &LoopRound{RoundID: agentdata.Token(), StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	state.Run, err = s.Data.Claim(ctx, r.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	json.Unmarshal([]byte(state.Run.BudgetJSON), &state.Budget)
	state.Operations, _ = s.Data.Operations(ctx, r.ID)
	if err = s.think(ctx, state, 0, &LoopRound{RoundID: agentdata.Token(), StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if p.calls != 1 {
		t.Fatalf("compaction changed saved request identity: %d", p.calls)
	}
}
