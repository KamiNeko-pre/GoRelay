package aiapp

import (
	"context"
	"go-lua-crawler/internal/engine"
	"testing"
	"time"
)

func TestConfiguredProgramTimeoutRemainsExecutableWithoutTaskTimebox(t *testing.T) {
	for _, tc := range []struct {
		name, setting string
		want          time.Duration
	}{{"unset", "", engine.DefaultProgramLimits().Timeout}, {"explicit-no-task-timebox", "0s", engine.DefaultProgramLimits().Timeout}, {"configured-task-window", "17m", 17 * time.Minute}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GORELAY_RUN_TIMEOUT", tc.setting)
			limits, err := ConfiguredProgramLimits()
			if err != nil {
				t.Fatal(err)
			}
			if limits.Timeout != tc.want {
				t.Errorf("optional task deadline overwrote the executable resource budget: got %s want %s", limits.Timeout, tc.want)
			}
			result, err := engine.RunProgram(context.Background(), engine.ProgramRequest{Code: "return true,{items={{value='configured budget remains executable',source_url='https://example.com/'}}}"}, limits, nil)
			if err != nil || result.Collection == nil {
				t.Fatalf("configured host policy cannot execute a valid program: %v", err)
			}
		})
	}
}
