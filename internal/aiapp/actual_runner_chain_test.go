package aiapp

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go-lua-crawler/internal/engine"
)

func actualRunnerForChain(t *testing.T) *RunnerClient {
	t.Helper()
	binary := os.Getenv("GORELAY_TEST_RUNNER_BINARY")
	if binary == "" {
		t.Fatal("actual runner binary path required; this gate cannot use a fixture VM")
	}
	binary, err := filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	command := exec.CommandContext(ctx, binary, "-serve", "-listen", address)
	command.Env = append(os.Environ(), "GORELAY_RUNNER_TOKEN=baseline-only")
	logFile, err := os.Create(filepath.Join(t.TempDir(), "runner-process.log"))
	if err != nil {
		t.Fatal(err)
	}
	command.Stdout, command.Stderr = logFile, logFile
	if err := command.Start(); err != nil {
		cancel()
		logFile.Close()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("actual runner parent did not stop")
		}
		_ = logFile.Close()
	})
	client := &http.Client{Timeout: time.Second}
	endpoint := "http://" + address
	ready := false
	for n := 0; n < 40; n++ {
		response, err := client.Get(endpoint + "/healthz")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == 200 {
				ready = true
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !ready {
		t.Fatal("actual runner process never became healthy")
	}
	runner, err := NewRunnerClient(endpoint, "baseline-only")
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

func TestActualRunnerProcurementFullChain(t *testing.T) {
	runCorrectModelSubsystemChain(t, false, 1, false, "", progressiveCase{Name: "actual-runner", ActualRunner: true, SelectorDiscovery: true, AdaptiveCrawler: true, LayoutVariant: 1, CompleteText: true, Expected: "completed"})
}

func TestActualRunnerMemoryFailureDoesNotKillParent(t *testing.T) {
	runner := actualRunnerForChain(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := runner.Execute(ctx, RunnerRequest{Program: engine.ProgramRequest{Code: `local x={};for i=1,512 do x[i]=string.rep("x",1024*1024) end;return true,{items={}}`}, Limits: engine.DefaultProgramLimits()})
	if err == nil || ctx.Err() != nil || !strings.Contains(err.Error(), "runner child terminated") {
		t.Fatal("memory failure was not isolated before the watchdog", err, ctx.Err())
	}
	got, err := runner.Execute(context.Background(), RunnerRequest{Program: engine.ProgramRequest{Code: `return true,{items={{value="parent survived",source_url="https://example.com"}}}`}, Limits: engine.DefaultProgramLimits()})
	if err != nil || got.Collection == nil {
		t.Fatal("parent could not accept a subsequent legitimate task", got, err)
	}
	auditSave(t, "actual-runner-memory-isolation", map[string]any{"platform": "Windows Job Object", "child_hard_memory_bytes": 96 << 20, "parent_survived": true, "followup_task_succeeded": true, "failure": "child process terminated before test watchdog"})
}

func TestActualRunnerKeepsExplicitExecutionDeadlineAcrossProcessBoundary(t *testing.T) {
	runner := actualRunnerForChain(t)
	limits := engine.DefaultProgramLimits()
	limits.Timeout = 60 * time.Millisecond
	started := time.Now()
	_, err := runner.Execute(context.Background(), RunnerRequest{Program: engine.ProgramRequest{Code: `while true do end`}, Limits: limits})
	if err == nil || time.Since(started) > 2*time.Second || !strings.Contains(err.Error(), "deadline") {
		t.Fatal("RPC/subprocess replaced the explicit execution deadline", time.Since(started), err)
	}
	result, err := runner.Execute(context.Background(), RunnerRequest{Program: engine.ProgramRequest{Code: `return true,{items={{value="deadline preserved",source_url="https://example.com"}}}`}, Limits: engine.DefaultProgramLimits()})
	if err != nil || result.Collection == nil {
		t.Fatal("timed-out child damaged its parent", err)
	}
}
