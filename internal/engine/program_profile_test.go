package engine

import (
	"context"
	"encoding/json"
	"go-lua-crawler/internal/acquisition"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func loadProfileFixture(t testing.TB) (ProgramRequest, []acquisition.ReplayEntry) {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate controlled fixture")
	}
	path := filepath.Join(filepath.Dir(source), "..", "..", "inputs", "ai", "replay-pagination-detail.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Program   ProgramRequest            `json:"program"`
		Responses []acquisition.ReplayEntry `json:"responses"`
	}
	if err = json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	return fixture.Program, fixture.Responses
}
func TestProfilingReplayContract(t *testing.T) {
	request, responses := loadProfileFixture(t)
	replay := acquisition.NewReplay(responses)
	out, err := RunProgram(context.Background(), request, DefaultProgramLimits(), replay.Fetch)
	if err != nil || len(out.Collection["items"].([]any)) != 3 || out.Stats.Requests != 5 {
		t.Fatalf("profiling load changed task output: %+v %v", out, err)
	}
}
func BenchmarkCodeAsActionReplay(b *testing.B) {
	request, responses := loadProfileFixture(b)
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		replay := acquisition.NewReplay(responses)
		out, err := RunProgram(context.Background(), request, DefaultProgramLimits(), replay.Fetch)
		if err != nil || len(out.Collection["items"].([]any)) != 3 || out.Stats.Requests != 5 {
			b.Fatal("replay task correctness failed", err)
		}
	}
}
