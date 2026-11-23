package information

import (
	"context"
	"testing"
	"time"

	"go-lua-crawler/internal/aiapp"
)

func TestRequestTimeboxIsChosenPerTaskAndNeverRenewed(t *testing.T) {
	s, _ := retrievalService(t)
	s.RequestTimeout = 11 * time.Minute
	spec := aiapp.TaskSpec{Goal: "collect according to task difficulty", SchemaVersion: "v1", RequiredFields: []aiapp.FieldRequirement{{Name: "name", Type: "string"}}}
	for _, seconds := range []int64{90, 1200, 0} {
		in := Input{RequestKey: time.Duration(seconds).String(), Goal: spec.Goal, Spec: &spec, TimeoutSeconds: &seconds}
		r, err := s.Create(context.Background(), in)
		if err != nil {
			t.Fatal(err)
		}
		if seconds == 0 && !r.Deadline.IsZero() {
			t.Fatal("explicit no-timebox request inherited a hard timeout")
		}
		if seconds > 0 && time.Until(r.Deadline) < time.Duration(seconds)*time.Second-time.Second {
			t.Fatal("task difficulty budget was replaced", r.Deadline)
		}
		again, err := s.Create(context.Background(), in)
		if err != nil || !again.Deadline.Equal(r.Deadline) {
			t.Fatal("retry renewed the task timebox", err)
		}
	}
	r, err := s.Create(context.Background(), Input{RequestKey: "configured-default", Goal: spec.Goal, Spec: &spec})
	if err != nil || time.Until(r.Deadline) < 10*time.Minute {
		t.Fatal("host-configured default was ignored", r.Deadline, err)
	}
}
