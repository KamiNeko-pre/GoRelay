//go:build windows

package aiapp

import (
	"context"
	"errors"
	"go-lua-crawler/internal/engine"
	"strings"
	"testing"
	"time"
)

func TestActualRunnerMemoryBreachHasActionableBudgetFailure(t *testing.T) {
	runner := actualRunnerForChain(t)
	for _, tc := range []struct{ name, code string }{
		{"gradual-live-allocations", "local x={};for i=1,512 do x[i]=string.rep('x',1024*1024) end;return true,{items={}}"},
		{"single-large-allocation", "local x=string.rep('y',128*1024*1024);return true,{items={}}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, err := runner.Execute(ctx, RunnerRequest{Program: engine.ProgramRequest{Code: tc.code}, Limits: engine.DefaultProgramLimits()})
			var failure *RunnerError
			if ctx.Err() != nil || !errors.As(err, &failure) || failure.Code != "budget_exceeded" || !strings.Contains(failure.Message, "memory") {
				t.Fatalf("memory fault lost host diagnosis: error=%v caller=%v", err, ctx.Err())
			}
			result, err := runner.Execute(context.Background(), RunnerRequest{Program: engine.ProgramRequest{Code: "return true,{items={{value='normal task after resource failure',source_url='https://example.com/'}}}"}, Limits: engine.DefaultProgramLimits()})
			if err != nil || result.Collection == nil {
				t.Fatalf("resource breach damaged reusable parent: %v", err)
			}
		})
	}
}
