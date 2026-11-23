package engine

import (
	"context"
	"testing"
)

func TestRunProgramProbeModeAcceptsArbitraryDiagnosticTables(t *testing.T) {
	code := `
local sparse = {}
sparse[1] = "first"
sparse[5] = "fifth"
local mixed = { "a", key = "b" }
return true, {
  keys = {"k1", "k2"},
  total = 42,
  sparse = sparse,
  mixed = mixed,
}
`
	res, err := RunProgramWithRequester(context.Background(), ProgramRequest{
		Code:      code,
		ProbeMode: true,
	}, DefaultProgramLimits(), nil)
	if err != nil {
		t.Fatalf("expected probe mode to accept arbitrary tables without items, got err=%v", err)
	}
	if res.Collection["total"] != float64(42) {
		t.Fatalf("unexpected probe collection: %+v", res.Collection)
	}
	if _, err := RunProgramWithRequester(context.Background(), ProgramRequest{
		Code:      code,
		ProbeMode: false,
	}, DefaultProgramLimits(), nil); err == nil {
		t.Fatal("expected non-probe mode to keep strict items array validation")
	}
}

func TestRunProgramPrintCaptureAndBudget(t *testing.T) {
	res, err := RunProgramWithRequester(context.Background(), ProgramRequest{
		Code: `
print("hello", 42, {a = 1, b = "two"})
print("line2")
`,
		ProbeMode: true,
	}, DefaultProgramLimits(), nil)
	if err != nil {
		t.Fatalf("expected print-only probe script to succeed, got err=%v", err)
	}
	if len(res.PrintLogs) != 2 || !containsSubstring(res.PrintLogs[0], `"a":1`) || res.PrintLogs[1] != "line2" {
		t.Fatalf("unexpected PrintLogs: %+v", res.PrintLogs)
	}

	noisy, err := RunProgramWithRequester(context.Background(), ProgramRequest{
		Code: `
for i = 1, 200 do
  print(string.rep("x", 150))
end
return true, { count = 200 }
`,
		ProbeMode: true,
	}, DefaultProgramLimits(), nil)
	if err != nil {
		t.Fatalf("expected bounded noisy print probe to succeed, got err=%v", err)
	}
	if len(noisy.PrintLogs) == 0 || noisy.PrintLogs[len(noisy.PrintLogs)-1] != "...(print log budget reached)" {
		t.Fatalf("expected print log truncation marker, got len=%d last=%q", len(noisy.PrintLogs), noisy.PrintLogs[len(noisy.PrintLogs)-1])
	}
}

func containsSubstring(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexString(s, sub) >= 0)
}

func indexString(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
