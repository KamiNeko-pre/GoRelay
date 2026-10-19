// lua-runner runs exactly one program per child process. The HTTP parent owns
// admission/caps; it never evaluates generated Lua in its own heap.
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
	"syscall"
	"time"

	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/engine"
)

const maxInputBytes = 96 << 10

var errChildMemoryBudget = errors.New("process memory budget exceeded")

type childIsolation interface {
	Close()
	Err() error
}

func main() {
	serve := flag.Bool("serve", false, "serve private /execute HTTP endpoint")
	child := flag.Bool("child", false, "internal one-shot child mode")
	listen := flag.String("listen", ":8091", "private runner bind address")
	flag.Parse()
	limits, configErr := aiapp.ConfiguredProgramLimits()
	if configErr != nil {
		log.Fatal(configErr)
	}
	if *serve {
		token := os.Getenv("GORELAY_RUNNER_TOKEN")
		if token == "" {
			log.Fatal("GORELAY_RUNNER_TOKEN is required for HTTP mode")
		}
		server := &http.Server{Addr: *listen, Handler: runnerHandler(token, limits, executeChild), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: limits.Timeout + 5*time.Second, MaxHeaderBytes: 8192}
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
	_ = *child // one-shot is also the default CLI mode; no credentials are required.
	in, err := decodeRequest(os.Stdin)
	out := aiapp.RunnerResponse{}
	if err == nil {
		if !*child {
			in.Limits = capLimits(in.Limits, limits)
		}
		out, err = aiapp.RunPublicProgram(context.Background(), in)
	}
	if err != nil {
		out.Error = classifyError(err)
	}
	if err = json.NewEncoder(os.Stdout).Encode(out); err != nil {
		log.Fatal(err)
	}
}

func decodeRequest(reader io.Reader) (aiapp.RunnerRequest, error) {
	var in aiapp.RunnerRequest
	data, err := io.ReadAll(io.LimitReader(reader, maxInputBytes+1))
	if err != nil {
		return in, err
	}
	if len(data) > maxInputBytes {
		return in, fmt.Errorf("runner input exceeds 96 KiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&in); err != nil {
		return in, err
	}
	if err = decoder.Decode(&struct{}{}); err != io.EOF {
		return in, fmt.Errorf("runner accepts one JSON request")
	}
	return in, nil
}
func capLimits(in, max engine.ProgramLimits) engine.ProgramLimits {
	if in.MaxRequests <= 0 || in.MaxRequests > max.MaxRequests {
		in.MaxRequests = max.MaxRequests
	}
	if in.MaxBytes <= 0 || in.MaxBytes > max.MaxBytes {
		in.MaxBytes = max.MaxBytes
	}
	if in.MaxItems <= 0 || in.MaxItems > max.MaxItems {
		in.MaxItems = max.MaxItems
	}
	if in.MaxOutputBytes <= 0 || in.MaxOutputBytes > max.MaxOutputBytes {
		in.MaxOutputBytes = max.MaxOutputBytes
	}
	if in.Timeout <= 0 || in.Timeout > max.Timeout {
		in.Timeout = max.Timeout
	}
	return in
}
func classifyError(err error) *aiapp.RunnerError {
	code := "execution_failed"
	switch {
	case errors.Is(err, acquisition.ErrBudgetExceeded), errors.Is(err, errChildMemoryBudget):
		code = "budget_exceeded"
	case errors.Is(err, acquisition.ErrNotAllowed):
		code = "permission_denied"
	case errors.Is(err, context.DeadlineExceeded):
		code = "timeout"
	case errors.Is(err, context.Canceled):
		code = "cancelled"
	}
	return &aiapp.RunnerError{Code: code, Message: err.Error()}
}

func runnerHandler(token string, limits engine.ProgramLimits, execute func(context.Context, aiapp.RunnerRequest) (aiapp.RunnerResponse, error)) http.Handler {
	mux := http.NewServeMux()
	slot := make(chan struct{}, 1)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("POST /execute", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
			w.WriteHeader(401)
			json.NewEncoder(w).Encode(aiapp.RunnerResponse{Error: &aiapp.RunnerError{Code: "unauthorized", Message: "runner token required"}})
			return
		}
		select {
		case slot <- struct{}{}:
			defer func() { <-slot }()
		default:
			w.WriteHeader(503)
			json.NewEncoder(w).Encode(aiapp.RunnerResponse{Error: &aiapp.RunnerError{Code: "busy", Message: "runner is executing another program"}})
			return
		}
		in, err := decodeRequest(r.Body)
		if err != nil {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(aiapp.RunnerResponse{Error: classifyError(err)})
			return
		}
		in.Limits = capLimits(in.Limits, limits)
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
	if b.Len()+len(p) > b.limit {
		return 0, fmt.Errorf("child output limit exceeded")
	}
	return b.Buffer.Write(p)
}
func executeChild(parent context.Context, in aiapp.RunnerRequest) (aiapp.RunnerResponse, error) {
	var out aiapp.RunnerResponse
	executable, err := os.Executable()
	if err != nil {
		return out, err
	}
	ctx, cancel := context.WithTimeout(parent, in.Limits.Timeout+2*time.Second)
	defer cancel()
	payload, err := json.Marshal(in)
	if err != nil {
		return out, err
	}
	cmd := exec.CommandContext(ctx, executable, "-child")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return out, err
	}
	// Do not inherit model/search keys or the runner token into code processes.
	cmd.Env = []string{"GOMEMLIMIT=96MiB", "GOGC=50"}
	// Network routing is a trusted host setting, not a Lua-visible credential.
	// Pass only the configured proxy; model/search keys and runner token stay out.
	if proxy := os.Getenv("GORELAY_ACQUISITION_PROXY"); proxy != "" {
		cmd.Env = append(cmd.Env, "GORELAY_ACQUISITION_PROXY="+proxy)
	}
	if root := os.Getenv("SystemRoot"); root != "" {
		cmd.Env = append(cmd.Env, "SystemRoot="+root)
	}
	stdout := &boundedBuffer{limit: int(aiapp.RunnerResponseByteLimit(in.Limits))}
	stderr := &boundedBuffer{limit: 4096}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err = cmd.Start(); err != nil {
		stdin.Close()
		return out, err
	}
	isolation, limitErr := isolateChild(cmd.Process)
	if limitErr != nil {
		stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return out, fmt.Errorf("child isolation unavailable: %w", limitErr)
	}
	defer isolation.Close()
	_, writeErr := stdin.Write(payload)
	_ = stdin.Close()
	if writeErr != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return out, writeErr
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
	if err = json.Unmarshal(stdout.Bytes(), &out); err != nil {
		return out, fmt.Errorf("runner child returned invalid JSON: %w", err)
	}
	return out, nil
}
