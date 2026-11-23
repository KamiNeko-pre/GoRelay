// lua-runner runs exactly one program per isolated OS child process.
// The long-lived HTTP parent owns admission caps and ONE shared fetchgov.SafeClient
// (persistent robots cache, per-domain rate limiter, 429/WAF circuit breaker,
// scoped AuthProfiles, and optional CDP Sidecar renderer); untrusted child
// processes never hold credentials or open outbound sockets directly.
package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"gorelay/internal/acquisition"
	"gorelay/internal/contract"
	"gorelay/internal/engine"
	"gorelay/internal/fetchgov"
	"gorelay/internal/programrunner"
)

var errChildMemoryBudget = errors.New("process memory budget exceeded")

// A deployment resource budget for an isolated process, not a task/output quota.
// Includes the Go/Lua heaps, parsed DOMs and retained source responses.
func childMemoryBudget() (uint64, error) {
	mb := uint64(0) // No application-invented quota; deployment owns resource budgets.
	if raw := os.Getenv("GORELAY_RUNNER_MEMORY_MB"); raw != "" {
		var err error
		mb, err = strconv.ParseUint(raw, 10, 64)
		if err != nil || mb == 0 || mb > ^uint64(0)>>20 {
			return 0, fmt.Errorf("GORELAY_RUNNER_MEMORY_MB must be a positive representable MiB value")
		}
	}
	return mb << 20, nil
}

type childIsolation interface {
	Close()
	Err() error
}

func main() {
	serve := flag.Bool("serve", false, "serve private /execute HTTP endpoint")
	child := flag.Bool("child", false, "internal one-shot child mode")
	snapshotPipe := flag.Bool("snapshot-pipe", false, "internal child host-IPC channel")
	listen := flag.String("listen", ":8091", "private runner bind address")
	flag.Parse()
	limits, configErr := programrunner.ConfiguredLimits()
	if configErr != nil {
		log.Fatal(configErr)
	}
	if *serve {
		token := os.Getenv("GORELAY_RUNNER_TOKEN")
		if token == "" {
			log.Fatal("GORELAY_RUNNER_TOKEN is required for HTTP mode")
		}
		govCfg, err := fetchgov.ConfigFromEnv()
		if err != nil {
			log.Fatalf("load SafeClient config: %v", err)
		}
		sharedClient, err := fetchgov.NewSafeClient(govCfg)
		if err != nil {
			log.Fatalf("initialize shared SafeClient: %v", err)
		}
		defer sharedClient.Close()

		var writeTimeout time.Duration
		if limits.Timeout > 0 {
			writeTimeout = limits.Timeout + 5*time.Second
		}
		execFn := func(ctx context.Context, in programrunner.Request) (programrunner.Response, error) {
			return executeChildWithClient(ctx, in, sharedClient)
		}
		server := &http.Server{Addr: *listen, Handler: runnerHandler(token, limits, execFn), ReadHeaderTimeout: 5 * time.Second, WriteTimeout: writeTimeout, MaxHeaderBytes: 8192}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		go func() {
			<-ctx.Done()
			closeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			server.Shutdown(closeCtx)
		}()
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
		return
	}
	if *child && *snapshotPipe {
		if err := runSnapshotChild(os.Stdin, os.Stdout); err != nil {
			log.Fatal(err)
		}
		return
	}
	_ = *child // one-shot CLI mode; no credentials are required.
	in, err := decodeRequest(os.Stdin)
	out := programrunner.Response{}
	if err == nil {
		if !*child {
			in.Limits = capLimits(in.Limits, limits)
		}
		out, err = programrunner.Run(context.Background(), in)
	}
	if err != nil {
		out.Error = classifyError(err)
	}
	if err = json.NewEncoder(os.Stdout).Encode(out); err != nil {
		log.Fatal(err)
	}
}

func decodeRequest(reader io.Reader) (programrunner.Request, error) {
	var in programrunner.Request
	err := decodeOneJSON(reader, &in)
	return in, err
}

func decodeOneJSON(reader io.Reader, value any) error {
	decoder := json.NewDecoder(reader)
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("runner accepts one JSON request")
	}
	return nil
}

func capLimits(in, host engine.ProgramLimits) engine.ProgramLimits {
	if host.Timeout > 0 && (in.Timeout <= 0 || in.Timeout > host.Timeout) {
		in.Timeout = host.Timeout
	}
	if host.MaxRequests > 0 && (in.MaxRequests <= 0 || in.MaxRequests > host.MaxRequests) {
		in.MaxRequests = host.MaxRequests
	}
	if host.MaxBytes > 0 && (in.MaxBytes <= 0 || in.MaxBytes > host.MaxBytes) {
		in.MaxBytes = host.MaxBytes
	}
	if host.MaxItems > 0 && (in.MaxItems <= 0 || in.MaxItems > host.MaxItems) {
		in.MaxItems = host.MaxItems
	}
	if host.MaxOutputBytes > 0 && (in.MaxOutputBytes <= 0 || in.MaxOutputBytes > host.MaxOutputBytes) {
		in.MaxOutputBytes = host.MaxOutputBytes
	}
	return in
}

func classifyError(err error) *programrunner.Error {
	var ge *fetchgov.GovernanceError
	if errors.As(err, &ge) && strings.TrimSpace(ge.Code) != "" {
		return &programrunner.Error{
			Code:    strings.ToLower(strings.TrimSpace(ge.Code)),
			Message: ge.Error(),
		}
	}
	code := "execution_failed"
	switch {
	case errors.Is(err, errChildMemoryBudget):
		code = "memory_limit"
	case errors.Is(err, acquisition.ErrBudgetExceeded):
		code = "budget_exceeded"
	case errors.Is(err, acquisition.ErrNotAllowed):
		code = "permission_denied"
	case errors.Is(err, context.DeadlineExceeded):
		code = "timeout"
	case errors.Is(err, context.Canceled):
		code = "cancelled"
	}
	return &programrunner.Error{Code: code, Message: err.Error()}
}

func configuredRunnerConcurrency() int {
	if raw := os.Getenv("GORELAY_RUNNER_CONCURRENCY"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			return n
		}
	}
	return 8
}

func runnerHandler(token string, limits engine.ProgramLimits, execute func(context.Context, programrunner.Request) (programrunner.Response, error)) http.Handler {
	mux := http.NewServeMux()
	slot := make(chan struct{}, configuredRunnerConcurrency())
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	auth := func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Content-Type", "application/json")
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
			w.WriteHeader(401)
			json.NewEncoder(w).Encode(programrunner.Response{Error: &programrunner.Error{Code: "unauthorized", Message: "runner token required"}})
			return false
		}
		return true
	}
	var batchesMu sync.RWMutex
	batches := map[string]map[string]acquisition.Response{}
	mux.HandleFunc("POST /snapshot-batch", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		controller := http.NewResponseController(w)
		batchReadTimeout := 60 * time.Second
		if limits.Timeout > 0 && limits.Timeout > batchReadTimeout {
			batchReadTimeout = limits.Timeout
		}
		_ = controller.SetReadDeadline(time.Now().Add(batchReadTimeout))
		var snapshots map[string]acquisition.Response
		if err := decodeOneJSON(r.Body, &snapshots); err != nil || len(snapshots) == 0 {
			http.Error(w, "snapshot batch requires a nonempty map", http.StatusBadRequest)
			return
		}
		if err := controller.SetReadDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
			http.Error(w, "snapshot connection unavailable", http.StatusInternalServerError)
			return
		}
		_ = controller.SetWriteDeadline(time.Time{})
		id := contract.RandomToken()
		batchesMu.Lock()
		batches[id] = snapshots
		batchesMu.Unlock()
		defer func() {
			batchesMu.Lock()
			delete(batches, id)
			batchesMu.Unlock()
		}()
		if err := json.NewEncoder(w).Encode(map[string]string{"id": id}); err != nil {
			return
		}
		if err := controller.Flush(); err != nil {
			return
		}
		<-r.Context().Done()
	})
	mux.HandleFunc("POST /execute", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		select {
		case slot <- struct{}{}:
			defer func() { <-slot }()
		default:
			w.WriteHeader(503)
			json.NewEncoder(w).Encode(programrunner.Response{Error: &programrunner.Error{Code: "busy", Message: "runner is executing another program"}})
			return
		}
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(30 * time.Second))
		in, err := decodeRequest(r.Body)
		if err != nil {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(programrunner.Response{Error: classifyError(err)})
			return
		}
		in.Limits = capLimits(in.Limits, limits)
		in.SnapshotReferences = r.Header.Get(programrunner.SnapshotReferencesHeader) == "1"
		if batch := r.Header.Get(programrunner.SnapshotBatchHeader); batch != "" {
			batchesMu.RLock()
			snapshots := batches[batch]
			batchesMu.RUnlock()
			if snapshots == nil {
				w.WriteHeader(http.StatusConflict)
				json.NewEncoder(w).Encode(programrunner.Response{Error: &programrunner.Error{Code: "snapshot_batch_unavailable", Message: "snapshot batch is no longer connected"}})
				return
			}
			in.Snapshots = snapshots
		}
		out, err := execute(r.Context(), in)
		if err != nil {
			out.Error = classifyError(err)
		}
		if out.Error != nil {
			w.WriteHeader(422)
		}
		json.NewEncoder(w).Encode(out)
	})
	return mux
}

type boundedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.limit > 0 && b.Len()+len(p) > b.limit {
		return 0, fmt.Errorf("child output limit exceeded")
	}
	return b.Buffer.Write(p)
}

func executeChild(parent context.Context, in programrunner.Request) (programrunner.Response, error) {
	return executeChildWithClient(parent, in, nil)
}

func executeChildWithClient(parent context.Context, in programrunner.Request, sharedClient *fetchgov.SafeClient) (programrunner.Response, error) {
	var out programrunner.Response
	memoryBytes, err := childMemoryBudget()
	if err != nil {
		return out, err
	}
	executable, err := os.Executable()
	if err != nil {
		return out, err
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	if in.Limits.Timeout > 0 {
		var deadlineCancel context.CancelFunc
		ctx, deadlineCancel = context.WithTimeout(ctx, in.Limits.Timeout+2*time.Second)
		defer deadlineCancel()
	}
	// Keep snapshots, checkpoint callback secrets, and AuthProfile metadata in
	// the parent. When sharedClient != nil, the child runs in zero-socket IPC
	// mode and proxies every http_request/http_render/checkpoint frame over pipes.
	reqMeta := in
	snapshots := in.Snapshots
	in.Snapshots = nil
	cbURL := in.CheckpointCallbackURL
	cbToken := in.CheckpointCallbackToken
	in.CheckpointCallbackURL = ""
	in.CheckpointCallbackToken = ""
	usePipe := sharedClient != nil || len(snapshots) > 0 || cbURL != "" || in.HasCheckpoint
	if usePipe && cbURL != "" {
		in.HasCheckpoint = true
	}
	if sharedClient != nil {
		in.AuthProfile = ""
	}
	payload, err := json.Marshal(in)
	if err != nil {
		return out, err
	}
	args := []string{"-child"}
	if usePipe {
		args = append(args, "-snapshot-pipe")
	}
	cmd := exec.CommandContext(ctx, executable, args...)
	prepareChildCmd(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return out, err
	}
	// Never inherit runner token, DB/Redis DSNs, or AuthProfile secrets into child processes.
	cmd.Env = []string{}
	for _, name := range []string{"GOMEMLIMIT", "GOGC"} {
		if value := os.Getenv(name); value != "" {
			cmd.Env = append(cmd.Env, name+"="+value)
		}
	}
	// Only pass proxy to direct-socket fallback; host-IPC children make zero outbound sockets.
	if !usePipe {
		if proxy := os.Getenv("GORELAY_ACQUISITION_PROXY"); proxy != "" {
			cmd.Env = append(cmd.Env, "GORELAY_ACQUISITION_PROXY="+proxy)
		}
	}
	if root := os.Getenv("SystemRoot"); root != "" {
		cmd.Env = append(cmd.Env, "SystemRoot="+root)
	}
	stdout := &boundedBuffer{limit: int(programrunner.ResponseByteLimit(in.Limits))}
	stderr := &boundedBuffer{limit: 4096}
	var pipeOutput io.ReadCloser
	if usePipe {
		pipeOutput, err = cmd.StdoutPipe()
		if err != nil {
			_ = stdin.Close()
			return out, err
		}
		defer pipeOutput.Close()
	} else {
		cmd.Stdout = stdout
	}
	cmd.Stderr = stderr
	if err = cmd.Start(); err != nil {
		stdin.Close()
		return out, err
	}
	isolation, limitErr := isolateChild(cmd.Process, memoryBytes)
	if limitErr != nil {
		stdin.Close()
		killProcessTree(cmd)
		_ = cmd.Wait()
		return out, fmt.Errorf("child isolation unavailable: %w", limitErr)
	}
	defer isolation.Close()
	var protocolErr error
	if pipeOutput != nil {
		cpHooks := makeHTTPCallbackHooks(cbURL, cbToken)
		out, protocolErr = exchangeChildPipeWithHost(ctx, stdin, pipeOutput, reqMeta, payload, snapshots, programrunner.ResponseByteLimit(in.Limits), cpHooks, sharedClient)
	} else {
		_, protocolErr = stdin.Write(payload)
	}
	_ = stdin.Close()
	if protocolErr != nil {
		killProcessTree(cmd)
		_ = cmd.Wait()
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		if resourceErr := isolation.Err(); resourceErr != nil {
			return out, fmt.Errorf("runner child terminated: %w", resourceErr)
		}
		return out, protocolErr
	}
	err = cmd.Wait()
	isolation.Close()
	if resourceErr := isolation.Err(); resourceErr != nil {
		return out, fmt.Errorf("runner child terminated: %w", resourceErr)
	}
	if ctx.Err() != nil {
		return out, ctx.Err()
	}
	if err != nil {
		return out, fmt.Errorf("runner child terminated: %w", err)
	}
	if pipeOutput == nil {
		if err = json.Unmarshal(stdout.Bytes(), &out); err != nil {
			return out, fmt.Errorf("runner child returned invalid JSON: %w", err)
		}
	}
	return out, nil
}
