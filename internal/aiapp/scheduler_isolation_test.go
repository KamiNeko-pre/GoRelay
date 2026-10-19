package aiapp

import (
	"context"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/testdb"
	"sync/atomic"
	"testing"
	"time"
)

type slowFirstModel struct {
	calls         atomic.Int32
	first, second chan struct{}
}

func (m *slowFirstModel) Chat(ctx context.Context, _ ChatRequest) (ChatResponse, error) {
	if m.calls.Add(1) == 1 {
		close(m.first)
		<-ctx.Done()
		return ChatResponse{}, ctx.Err()
	}
	select {
	case m.second <- struct{}{}:
	default:
	}
	return ChatResponse{Message: Message{Role: "assistant", Content: "fixture response"}, Usage: &Usage{TotalTokens: 1}}, nil
}
func TestSlowModelDoesNotBlockOtherRunScheduling(t *testing.T) {
	t.Setenv("GORELAY_AGENT_CONCURRENCY", "2")
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, key := range []string{"slow", "fast"} {
		if _, err := s.Create(ctx, CreateRunRequest{RequestKey: key, Goal: key}); err != nil {
			t.Fatal(err)
		}
	}
	m := &slowFirstModel{first: make(chan struct{}), second: make(chan struct{}, 1)}
	s.Model = m
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	select {
	case <-m.first:
	case <-time.After(4 * time.Second):
		t.Fatal("first run not scheduled")
	}
	select {
	case <-m.second:
	case <-time.After(2 * time.Second):
		cancel()
		<-done
		t.Fatal("slow model blocked the next independent run")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		t.Fatal("scheduler did not stop its owned work")
	}
}
func TestWaitingRunIsNotSelectedUntilItsWakeTime(t *testing.T) {
	s := agentdata.NewStore(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r, err := s.CreateRun(ctx, "waiting", "wait", "{}", "{}", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Model(&agentdata.Run{}).Where("id = ?", r.ID).Updates(map[string]any{"status": "waiting", "next_wake_at": time.Now().Add(time.Minute)}).Error; err != nil {
		t.Fatal(err)
	}
	rows, err := s.Pending(ctx, 8)
	if err != nil || len(rows) != 0 {
		t.Fatal("waiting run was repeatedly selected", err)
	}
	if err := s.DB.Model(&agentdata.Run{}).Where("id = ?", r.ID).Update("next_wake_at", time.Now().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	rows, err = s.Pending(ctx, 8)
	if err != nil || len(rows) != 1 {
		t.Fatal("due run did not wake", err)
	}
}
