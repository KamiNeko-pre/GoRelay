package information

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/engine"
)

type BrowserField struct {
	Selector   string `json:"selector"`
	Attribute  string `json:"attribute,omitempty"`
	SourceText bool   `json:"source_text,omitempty"`
}
type BrowserPlan struct {
	URL          string                  `json:"url"`
	RowsSelector string                  `json:"rows_selector,omitempty"`
	Fields       map[string]BrowserField `json:"fields"`
	WaitSelector string                  `json:"wait_selector,omitempty"`
	NextSelector string                  `json:"next_selector,omitempty"`
}
type BrowserRunner struct {
	NodeBinary     string
	ScriptPath     string
	ModulePath     string
	ExecutablePath string
	ActionTimeout  time.Duration
}

func (b *BrowserRunner) Render(ctx context.Context, plan BrowserPlan, scope []string, fetcher *acquisition.PublicFetcher) (runtime engine.ProgramResult, cause error) {
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
	if actionTimeout <= 0 {
		actionTimeout = 30 * time.Second
	}
	input := map[string]any{"plan": plan, "scope": scope, "module_path": b.ModulePath, "executable_path": b.ExecutablePath, "proxy": "http://" + listener.Addr().String(), "timeout_ms": taskTimeout, "action_timeout_ms": actionTimeout.Milliseconds(), "max_requests": 100, "max_bytes": 32 << 20}
	cmd := exec.CommandContext(ctx, node, b.ScriptPath)
	cmd.Stdin = bytes.NewReader(mustMarshal(input))
	var output, diagnostics bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &diagnostics
	err = cmd.Run()
	var result struct {
		Sources  []acquisition.Response `json:"sources"`
		Requests []acquisition.Receipt  `json:"requests"`
		Error    string                 `json:"error"`
	}
	if decode := json.Unmarshal(output.Bytes(), &result); decode != nil {
		return runtime, fmt.Errorf("browser runtime did not return a complete receipt: %v; process=%v", decode, err)
	}
	runtime.SourceEvidence = result.Sources
	runtime.Requests = result.Requests
	if result.Error != "" {
		return runtime, fmt.Errorf("browser rendering failed: %s", result.Error)
	}
	if err != nil {
		return runtime, fmt.Errorf("browser process failed: %w", err)
	}
	items := []any{}
	for _, source := range result.Sources {
		runtime.Requests = append(runtime.Requests, source.Receipt)
		doc, err := goquery.NewDocumentFromReader(strings.NewReader(source.Body))
		if err != nil {
			return runtime, err
		}
		doc.Find("script,style,noscript,template").Remove()
		rowSelector := plan.RowsSelector
		if rowSelector == "" {
			rowSelector = "body"
		}
		rows := doc.Find(rowSelector)
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
				if field.SourceText {
					item[name], extractErr = acquisition.SourceText(source.Body)
					continue
				}
				found := row.Find(field.Selector).First()
				if found.Length() == 0 {
					extractErr = fmt.Errorf("field %s selector %q matched no node in %q", name, field.Selector, rowSelector)
					return
				}
				selector := rowSelector + " " + field.Selector
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
				item[name] = strings.Join(strings.Fields(value), " ")
				locators[name] = map[string]any{"selector": selector, "index": index, "attribute": field.Attribute}
			}
			item["_evidence"] = locators
			items = append(items, item)
		})
		if extractErr != nil {
			return runtime, extractErr
		}
	}
	runtime.Collection = map[string]any{"items": items}
	return runtime, nil
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
