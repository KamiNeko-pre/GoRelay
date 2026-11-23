package information

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/engine"
)

type BrowserField struct {
	URLTransform *aiapp.URLTransform `json:"url_transform,omitempty"`
	Selector     string              `json:"selector"`
	Attribute    string              `json:"attribute,omitempty"`
}
type BrowserEmptyState struct {
	Selector string `json:"selector"`
	Text     string `json:"text"`
}
type BrowserPlan struct {
	EmptyState   *BrowserEmptyState      `json:"empty_state,omitempty"`
	URL          string                  `json:"url"`
	RowsSelector string                  `json:"rows_selector,omitempty"`
	Fields       map[string]BrowserField `json:"fields"`
	WaitSelector string                  `json:"wait_selector,omitempty"`
	NextSelector string                  `json:"next_selector,omitempty"`
}
type browserWorkerProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
}

func (w *browserWorkerProcess) close() {
	if w == nil {
		return
	}
	if w.cmd != nil && w.cmd.Process != nil {
		_ = w.cmd.Process.Kill()
	}
	if w.stdin != nil {
		_ = w.stdin.Close()
	}
	if w.cmd != nil && w.cmd.Process != nil {
		_ = w.cmd.Wait()
	}
}

type BrowserRunner struct {
	NodeBinary     string
	ScriptPath     string
	ModulePath     string
	ExecutablePath string
	ActionTimeout  time.Duration

	workerMu  sync.Mutex
	worker    *browserWorkerProcess
	idleTimer *time.Timer
}

// Close shuts down any warm persistent browser worker process.
func (b *BrowserRunner) Close() {
	if b == nil {
		return
	}
	b.workerMu.Lock()
	defer b.workerMu.Unlock()
	if b.idleTimer != nil {
		b.idleTimer.Stop()
		b.idleTimer = nil
	}
	if b.worker != nil {
		b.worker.close()
		b.worker = nil
	}
}

func (b *BrowserRunner) startWorkerLocked(node string) (*browserWorkerProcess, error) {
	cmd := exec.Command(node, b.ScriptPath, "--worker")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, err
	}
	return &browserWorkerProcess{
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewReaderSize(stdoutPipe, 64*1024),
	}, nil
}

func (b *BrowserRunner) runWorkerPayload(ctx context.Context, node string, payload []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// An occupied warm worker keeps its request. Render can use the isolated
	// one-shot path for independent requests without waiting for this lock.
	if !b.workerMu.TryLock() {
		return nil, fmt.Errorf("warm browser worker is busy")
	}
	defer b.workerMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if b.idleTimer != nil {
		b.idleTimer.Stop()
		b.idleTimer = nil
	}
	resetIdle := func() {
		if b.worker != nil {
			b.idleTimer = time.AfterFunc(2*time.Minute, func() {
				b.Close()
			})
		}
	}
	if b.worker == nil {
		w, err := b.startWorkerLocked(node)
		if err != nil {
			return nil, err
		}
		b.worker = w
	}
	type readResult struct {
		line []byte
		err  error
	}
	ch := make(chan readResult, 1)
	worker := b.worker
	go func() {
		if _, err := worker.stdin.Write(append(payload, '\n')); err != nil {
			ch <- readResult{err: err}
			return
		}
		line, err := worker.stdout.ReadBytes('\n')
		ch <- readResult{line: line, err: err}
	}()
	select {
	case <-ctx.Done():
		b.worker.close()
		b.worker = nil
		return nil, ctx.Err()
	case res := <-ch:
		if res.err != nil {
			b.worker.close()
			b.worker = nil
			return nil, res.err
		}
		resetIdle()
		return bytes.TrimSpace(res.line), nil
	}
}

func (b *BrowserRunner) Render(ctx context.Context, plan BrowserPlan, scope []string, fetcher *acquisition.PublicFetcher) (runtime engine.ProgramResult, cause error) {
	if err := ctx.Err(); err != nil {
		return runtime, err
	}
	started := time.Now()
	runtime.HTTPUsageKnown = true
	defer func() { runtime.Stats = fetcher.Stats(); runtime.DurationMS = time.Since(started).Milliseconds() }()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return runtime, err
	}
	server := &http.Server{Handler: fetcher.BrowserProxy(), ReadHeaderTimeout: 5 * time.Second}
	go server.Serve(listener)
	defer server.Close()
	node := b.NodeBinary
	if node == "" {
		node = "node"
	}
	var taskTimeout int64
	if deadline, ok := ctx.Deadline(); ok {
		taskTimeout = max(1, time.Until(deadline).Milliseconds())
	}
	actionTimeout := b.ActionTimeout
	input := map[string]any{"plan": plan, "scope": scope, "module_path": b.ModulePath, "executable_path": b.ExecutablePath, "proxy": "http://" + listener.Addr().String(), "timeout_ms": taskTimeout, "action_timeout_ms": actionTimeout.Milliseconds()}
	payload := mustMarshal(input)

	var rawOutput []byte
	var runErr error
	if strings.HasSuffix(strings.ToLower(strings.TrimSpace(b.ScriptPath)), "information-browser.cjs") {
		rawOutput, runErr = b.runWorkerPayload(ctx, node, payload)
		if runErr != nil && ctx.Err() == nil {
			// The one-shot path also serves requests while the warm worker is busy.
			cmd := exec.CommandContext(ctx, node, b.ScriptPath)
			cmd.Stdin = bytes.NewReader(payload)
			var output, diagnostics bytes.Buffer
			cmd.Stdout = &output
			cmd.Stderr = &diagnostics
			runErr = cmd.Run()
			rawOutput = output.Bytes()
		}
	} else {
		cmd := exec.CommandContext(ctx, node, b.ScriptPath)
		cmd.Stdin = bytes.NewReader(payload)
		var output, diagnostics bytes.Buffer
		cmd.Stdout = &output
		cmd.Stderr = &diagnostics
		runErr = cmd.Run()
		rawOutput = output.Bytes()
	}
	err = runErr
	if len(rawOutput) == 0 && err != nil {
		return runtime, err
	}
	var result struct {
		Sources     []acquisition.Response `json:"sources"`
		Requests    []acquisition.Receipt  `json:"requests"`
		NetworkAPIs []struct {
			Method         string            `json:"method"`
			URL            string            `json:"url"`
			Status         int               `json:"status"`
			ContentType    string            `json:"content_type"`
			RequestHeaders map[string]string `json:"request_headers"`
			SentCookies    bool              `json:"sent_cookies"`
			SetsCookie     bool              `json:"sets_cookie"`
			RequestBody    string            `json:"request_body"`
			ResponseBody   string            `json:"response_body"`
		} `json:"network_apis"`
		Error string `json:"error"`
	}
	if decode := json.Unmarshal(rawOutput, &result); decode != nil {
		return runtime, fmt.Errorf("browser runtime did not return a complete receipt: %v; process=%v", decode, err)
	}
	runtime.SourceEvidence = result.Sources
	runtime.Requests = result.Requests
	for _, api := range result.NetworkAPIs {
		cap := acquisition.NetworkAPICapture{
			Method:         api.Method,
			URL:            api.URL,
			Status:         api.Status,
			ContentType:    api.ContentType,
			RequestHeaders: api.RequestHeaders,
			SentCookies:    api.SentCookies,
			SetsCookie:     api.SetsCookie,
			RequestBody:    api.RequestBody,
		}
		if trimmed := strings.TrimSpace(api.ResponseBody); trimmed != "" {
			var parsed any
			if json.Unmarshal([]byte(trimmed), &parsed) == nil {
				if paths := observedJSONPaths(parsed, ""); len(paths) > 0 {
					cap.ResponseSchema = json.RawMessage(mustMarshal(paths))
				}
			}
		}
		runtime.NetworkAPIs = append(runtime.NetworkAPIs, cap)
	}
	if result.Error != "" {
		return runtime, fmt.Errorf("browser rendering failed: %s", result.Error)
	}
	if err != nil {
		return runtime, fmt.Errorf("browser process failed: %w", err)
	}
	cause = extractBrowserCollection(plan, &runtime)
	return runtime, cause
}

func browserEmptyLocator(plan BrowserPlan) acquisition.EmptyResultEvidence {
	if plan.EmptyState == nil {
		return acquisition.EmptyResultEvidence{}
	}
	return acquisition.EmptyResultEvidence{RowsSelector: plan.RowsSelector, EmptySelector: plan.EmptyState.Selector, EmptyText: plan.EmptyState.Text}
}

// Shared by actual browser rendering and deterministic replay of captured DOM.
func extractBrowserCollection(plan BrowserPlan, runtime *engine.ProgramResult) error {
	items := []any{}
	for _, source := range runtime.SourceEvidence {
		runtime.Requests = append(runtime.Requests, source.Receipt)
		doc, err := goquery.NewDocumentFromReader(strings.NewReader(source.Body))
		if err != nil {
			return err
		}
		doc.Find("script,style,noscript,template").Remove()
		rowSelector := plan.RowsSelector
		if rowSelector == "" {
			rowSelector = "body"
		}
		rows := doc.Find(rowSelector)
		if rows.Length() == 0 {
			if plan.EmptyState == nil {
				return fmt.Errorf("rows_selector %q matched no rows; inspect rendered DOM and correct it, or declare an observed empty_state selector/text", rowSelector)
			}
			if _, err := acquisition.CheckEmptySource(source, browserEmptyLocator(plan)); err != nil {
				return err
			}
		}
		var extractErr error
		rows.Each(func(_ int, row *goquery.Selection) {
			if extractErr != nil {
				return
			}
			item := map[string]any{"source_url": source.Receipt.URL}
			locators := map[string]any{}
			for name, field := range plan.Fields {
				if name == "source_url" {
					continue
				}
				sel := strings.TrimSpace(field.Selector)
				var found *goquery.Selection
				var selector string
				if sel == "." || (sel == "" && rowSelector != "body") {
					found = row
					selector = rowSelector
				} else {
					if isRootPageSelector(sel) {
						extractErr = fmt.Errorf("field %s selector %q targets root page node instead of bounded DOM elements", name, field.Selector)
						return
					}
					found = row.Find(sel).First()
					selector = rowSelector + " " + sel
					if found.Length() == 0 && row.Is(sel) {
						found = row
						selector = rowSelector
					}
				}
				if found.Length() == 0 {
					extractErr = fmt.Errorf("field %s selector %q matched no node in %q", name, field.Selector, rowSelector)
					return
				}
				index := -1
				doc.Find(selector).Each(func(i int, match *goquery.Selection) {
					if match.Nodes[0] == found.Nodes[0] {
						index = i
					}
				})
				if index < 0 {
					extractErr = fmt.Errorf("field %s has no exact rendered DOM locator", name)
					return
				}
				value := found.Text()
				if field.Attribute != "" {
					var ok bool
					value, ok = found.Attr(field.Attribute)
					if !ok {
						extractErr = fmt.Errorf("field %s lacks attribute %s", name, field.Attribute)
						return
					}
				}
				value = strings.Join(strings.Fields(value), " ")
				if field.URLTransform != nil {
					var err error
					value, err = aiapp.TransformObservedURL(value, source, *field.URLTransform)
					if err != nil {
						extractErr = fmt.Errorf("field %s URL transformation: %w", name, err)
						return
					}
				}
				item[name] = value
				locator := map[string]any{"selector": selector, "index": index, "attribute": field.Attribute}
				if field.URLTransform != nil {
					locator["url_transform"] = field.URLTransform
				}
				locators[name] = locator
			}
			item["_evidence"] = locators
			items = append(items, item)
		})
		if extractErr != nil {
			return extractErr
		}
	}
	runtime.Collection = map[string]any{"items": items}
	return nil
}

func (s *Service) ConfigureBrowser() {
	if os.Getenv("GORELAY_BROWSER_BINARY") == "" {
		return
	}
	s.Browser = &BrowserRunner{NodeBinary: os.Getenv("GORELAY_BROWSER_NODE"), ScriptPath: os.Getenv("GORELAY_BROWSER_SCRIPT"), ModulePath: os.Getenv("GORELAY_PLAYWRIGHT_MODULE"), ExecutablePath: os.Getenv("GORELAY_BROWSER_BINARY")}
	if s.Browser.ScriptPath == "" {
		s.Browser.ScriptPath = "scripts/information-browser.cjs"
	}
}

func isRootPageSelector(sel string) bool {
	switch strings.ToLower(strings.TrimSpace(sel)) {
	case "", "html", "body", "*", ":root", "html body", "html > body":
		return true
	default:
		return false
	}
}
