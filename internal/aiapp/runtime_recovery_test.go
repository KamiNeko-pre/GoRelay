package aiapp

import (
	"context"
	"encoding/json"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/testdb"
	"testing"
	"time"
)

type countChat struct {
	calls   int
	request ChatRequest
}

func (c *countChat) Chat(_ context.Context, request ChatRequest) (ChatResponse, error) {
	c.request = request
	c.calls++
	return ChatResponse{Message: Message{Role: "assistant", Content: "saved before crash"}}, nil
}
func TestModelResponsePersistedBeforeCrashResumesWithoutExtraCall(t *testing.T) {
	for _, mode := range []string{"code", "per_tool"} {
		t.Run(mode, func(t *testing.T) { t.Setenv("GORELAY_ACTION_MODE", mode); verifyModelResponseRecovery(t) })
	}
}

func verifyModelResponseRecovery(t *testing.T) {
	db := testdb.Open(t)
	s := NewService(db)
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r, _ := s.Create(ctx, CreateRunRequest{RequestKey: "crash", Goal: "goal"})
	r, _ = s.Data.Claim(ctx, r.ID, time.Minute)
	client := &countChat{}
	s.Model = client
	state, err := s.state(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.think(ctx, state, 0, &LoopRound{RoundID: agentdata.Token(), StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	// Simulate loss of the conversation checkpoint, retaining the actual response
	// ledger and spent budget instead of manually constructing its private key.
	r, err = s.Data.Claim(ctx, r.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Data.SaveRun(ctx, r.ID, r.Token, map[string]any{"status": "reviewing", "plan_json": "[]"}); err != nil {
		t.Fatal(err)
	}
	s.Decide = func(context.Context, LoopState) (NextAction, error) { return NextAction{Kind: "model"}, nil }
	if err = s.Advance(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	stored, _ := s.Data.Run(ctx, r.ID)
	var got []Message
	json.Unmarshal([]byte(stored.PlanJSON), &got)
	// Host context can add protocol messages; the recovery invariant is one
	// paid request and one preserved reply, not a fixed transcript length.
	savedReplies := 0
	for _, message := range got {
		if message.Role == "assistant" && message.Content == "saved before crash" && len(message.ToolCalls) == 0 {
			savedReplies++
		}
	}
	ops, err := s.Data.Operations(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	calls := CrawlerModelCalls(ops)
	if len(calls) != 1 || calls[0].PromptHash != agentdata.Hash(client.request.Messages) || calls[0].ToolSchemaHash != agentdata.Hash(client.request.Tools) {
		t.Fatalf("actual request fingerprints missing or changed during replay: %+v", calls)
	}
	if client.calls != 1 || savedReplies != 1 {
		t.Fatalf("recovery repeated paid call: %d %+v", client.calls, got)
	}
}
