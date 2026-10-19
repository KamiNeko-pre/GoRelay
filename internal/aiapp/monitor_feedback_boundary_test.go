package aiapp

import (
	"context"
	"strings"
	"testing"
)

func TestMonitorFallbackCauseIsVisibleAsToolData(t *testing.T) {
	s, m, _ := seedMonitorPlan(t, false)
	ctx := context.Background()
	r, err := s.RefreshMonitor(ctx, m.ID, "feedback")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Data.DB.Table("source_heads").Where("source_id = ?", "public").Update("status", "quarantined").Error; err != nil {
		t.Fatal(err)
	}
	if err := s.Advance(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	current, err := s.Data.Run(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	state, err := s.state(ctx, current)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := CompactContext(state, state.Messages)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range messages {
		if message.Role == "tool" && strings.Contains(message.Content, "quarantined") && strings.Contains(message.Content, "https://example.com/data") {
			return
		}
	}
	t.Fatal("monitor execution failure and reusable binding disappear before model correction")
}
