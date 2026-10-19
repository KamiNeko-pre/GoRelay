package aiapp

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestDeliveryUsesWholeTaskDeadlineWithoutReservedMinute(t *testing.T) {
	t.Setenv("GORELAY_EXPERIMENT_UNLIMITED_BUDGET", "1")
	t.Setenv("GORELAY_RUN_TIMEOUT", "6m")
	s, r, _ := storedHTMLReader(t, "<main>evidence</main>")
	_ = s
	var budget map[string]json.RawMessage
	if err := json.Unmarshal([]byte(r.BudgetJSON), &budget); err != nil {
		t.Fatal(err)
	}
	var work time.Time
	_ = json.Unmarshal(budget["work_deadline"], &work)
	if !work.IsZero() || r.Deadline.IsZero() {
		t.Fatalf("task was shortened by a separate work deadline work=%v final=%v", work, r.Deadline)
	}
	if delta := r.Deadline.Sub(r.CreatedAt); delta < 6*time.Minute-time.Second || delta > 6*time.Minute+time.Second {
		t.Fatalf("task did not get its six-minute window: %v", delta)
	}
}
func TestDeliveryWorkDeadlineStartsConclusionWithoutWaitingForStagnation(t *testing.T) {
	s := efficiencyState()
	raw := map[string]any{}
	_ = json.Unmarshal(mustJSON(s.Budget), &raw)
	raw["work_deadline"] = time.Now().Add(-time.Second)
	b, _ := json.Marshal(raw)
	_ = json.Unmarshal(b, &s.Budget)
	h := advanceHostProgressForTest(HostProgress{}, s)
	if recoveryPhase(t, h) != "concluding" {
		t.Fatal("work timeout did not reserve conclusion")
	}
	s.HostProgress = h
	a, err := RunTaskLoop(context.Background(), s)
	if err != nil || a.Kind != "model" {
		t.Fatal(a, err)
	}
}
