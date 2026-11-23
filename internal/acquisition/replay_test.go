package acquisition

import (
	"context"
	"testing"
)

func TestReplayRequiresExactMethodURLAndOrder(t *testing.T) {
	r := NewReplay([]ReplayEntry{{Method: "GET", URL: "https://fixture.invalid/list?page=1", Status: 200, Body: "one"}, {Method: "GET", URL: "https://fixture.invalid/list?page=2", Status: 200, Body: "two"}})
	if _, err := r.Fetch(context.Background(), "GET", "https://fixture.invalid/list?page=2"); err == nil {
		t.Fatal("out-of-order response accepted")
	}
	got, err := r.Fetch(context.Background(), "GET", "https://fixture.invalid/list?page=1")
	if err != nil || got.Body != "one" {
		t.Fatalf("failed matching entry: %+v %v", got, err)
	}
	if _, err = r.Fetch(context.Background(), "POST", "https://fixture.invalid/list?page=2"); err == nil {
		t.Fatal("method mismatch accepted")
	}
	if _, err = r.Fetch(context.Background(), "GET", "https://fixture.invalid/list?page=2"); err != nil || r.Remaining() != 0 {
		t.Fatalf("replay did not finish: %v", err)
	}
	if _, err = r.Fetch(context.Background(), "GET", "https://fixture.invalid/list?page=2"); err == nil {
		t.Fatal("fabricated response after fixture exhausted")
	}
}
