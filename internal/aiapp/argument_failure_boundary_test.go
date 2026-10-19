package aiapp

import (
	"errors"
	"strings"
	"testing"
)

func TestArgumentDecodeFailureHasCorrectRecoveryBoundary(t *testing.T) {
	for _, raw := range []string{`{"other":1}`, `{"count":"bad"}`, `{"count":`, `{"count":1} {}`, strings.Repeat("x", 97<<10)} {
		var args struct {
			Count int `json:"count"`
		}
		err := decodeArgs(raw, &args)
		if err == nil {
			t.Fatal("expected invalid args")
		}
		f := captureFailure(err, 0, false)
		if f.Code != "ARGUMENT_JSON_ERROR" || f.ExecutionStatus != "not_started" || f.Recovery.Strategy != "correct_arguments" || f.Details == nil {
			t.Fatalf("wrong boundary for %q: %+v", raw[:min(40, len(raw))], f)
		}
	}
	var valid struct {
		Count int `json:"count"`
	}
	if err := decodeArgs(`{"count":2}`, &valid); err != nil || valid.Count != 2 {
		t.Fatal(err)
	}
	f := captureFailure(errors.New("remote acknowledgement lost"), 0, false)
	if f.ExecutionStatus != "unknown" {
		t.Fatal("unknown side effect mislabeled safe to retry")
	}
}
