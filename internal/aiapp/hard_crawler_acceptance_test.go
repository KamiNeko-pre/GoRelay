package aiapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/config"
	"go-lua-crawler/internal/queue"
	"go-lua-crawler/internal/repository"
	"go-lua-crawler/internal/service"
	"go-lua-crawler/internal/task"
	"go-lua-crawler/internal/testdb"
	"go-lua-crawler/internal/worker"
)

type hardCrawlerCase struct {
	Name, Entry, Goal string
	Fields            []string
	Pages             map[string]string
	Gold              map[string]map[string]string
}

// The provider never receives these fixtures, locators or Gold. Only normal
// HTTP inspections expose their structure. Gold is used after terminal delivery.
func hardCrawlerCases() []hardCrawlerCase {
	base := "http://8.8.8.8"
	salt := agentdata.Token()[:8]
	htmlCase := hardCrawlerCase{Name: "html-table-procurement", Entry: base + "/tenders", Fields: []string{"project_id", "title", "buyer", "budget", "source_url"}, Pages: map[string]string{}, Gold: map[string]map[string]string{}}
	jsonCase := hardCrawlerCase{Name: "json-dataset-catalog", Entry: base + "/datasets", Fields: []string{"dataset_id", "title", "publisher", "license", "source_url"}, Pages: map[string]string{}, Gold: map[string]map[string]string{}}
	htmlCase.Goal = "从入口 " + htmlCase.Entry + " 遍历全部分页及公告详情，按项目编号去重，采集每条采购项目的 project_id（项目编号）、title（项目名称）、buyer（采购单位）、budget（预算原文）、source_url（详情地址）。保存并实际使用可复用采集能力。可选说明缺失不影响交付；不得补造数据。"
	jsonCase.Goal = "从入口 " + jsonCase.Entry + " 遍历全部分页及数据集详情，按数据集编号去重，采集每条数据集的 dataset_id（数据集编号）、title（数据集名称）、publisher（发布机构）、license（许可协议原文）、source_url（详情地址）。保存并实际使用可复用采集能力。可选说明缺失不影响交付；不得补造数据。"
	for i := 1; i <= 3; i++ {
		path := fmt.Sprintf("/tender/%d", i)
		row := map[string]string{"project_id": fmt.Sprintf("CG-%s-%d", salt, i), "title": fmt.Sprintf("公共实验设备采购第%d批", i), "buyer": fmt.Sprintf("第%d技术中心", i), "budget": fmt.Sprintf("%d.50万元", i*17), "source_url": base + path}
		htmlCase.Gold[row["source_url"]] = row
		var cells strings.Builder
		for _, field := range []struct{ Key, Label string }{{"project_id", "项目编号"}, {"buyer", "采购单位"}, {"budget", "预算金额"}} {
			fmt.Fprintf(&cells, `<tr><th>%s</th><td id="cell_%s_%s">%s</td></tr>`, field.Label, salt, field.Key, row[field.Key])
		}
		htmlCase.Pages[path] = fmt.Sprintf(`<html><body><nav><a href="/tenders">公告目录</a></nav><main><h2 class="heading_%s">%s</h2><table>%s</table><p>%s</p></main></body></html>`, salt, row["title"], cells.String(), strings.Repeat("本段属于公告说明，不是分页或字段定义。", 50))
		path = fmt.Sprintf("/dataset/%d", i)
		row = map[string]string{"dataset_id": fmt.Sprintf("DS-%s-%d", salt, i), "title": fmt.Sprintf("第%d组公共观测数据", i), "publisher": fmt.Sprintf("第%d开放数据研究所", i), "license": "CC-BY-4.0", "source_url": base + path}
		jsonCase.Gold[row["source_url"]] = row
		payload := map[string]any{"dataset": map[string]any{"code": row["dataset_id"], "name": row["title"], "organization": map[string]string{"name": row["publisher"]}, "rights": map[string]string{"license": row["license"]}}}
		if i != 2 {
			payload["optional_note"] = "已公开"
		}
		raw, _ := json.Marshal(payload)
		jsonCase.Pages[path] = string(raw)
	}
	for page, indexes := range [][]int{{1, 2}, {2, 3}} {
		path := "/tenders"
		if page == 1 {
			path += "?page=2"
		}
		var listing strings.Builder
		listing.WriteString(`<html><body><nav><a href="/help">使用说明</a></nav><main><ul class="rows_` + salt + `">`)
		for _, i := range indexes {
			fmt.Fprintf(&listing, `<li><a href="/tender/%d">公共实验设备采购第%d批</a></li>`, i, i)
		}
		listing.WriteString("</ul>")
		if page == 0 {
			listing.WriteString(`<a rel="next" href="/tenders?page=2">下一页</a>`)
		}
		listing.WriteString("</main></body></html>")
		htmlCase.Pages[path] = listing.String()
		path = "/datasets"
		if page == 1 {
			path += "?page=2"
		}
		entries := []any{}
		for _, i := range indexes {
			entries = append(entries, map[string]any{"label": fmt.Sprintf("第%d组公共观测数据", i), "detail": fmt.Sprintf("/dataset/%d", i)})
		}
		next := ""
		if page == 0 {
			next = "/datasets?page=2"
		}
		raw, _ := json.Marshal(map[string]any{"results": entries, "pagination": map[string]any{"next": next}})
		jsonCase.Pages[path] = string(raw)
	}
	return []hardCrawlerCase{htmlCase, jsonCase}
}

type tracedHardCrawler struct {
	Provider *ModelProvider
	T        *testing.T
	Calls    atomic.Int64
	Stop     context.CancelFunc
}

func (p *tracedHardCrawler) QuoteChat(request ChatRequest, overhead int64) (agentdata.SpendQuote, error) {
	return p.Provider.QuoteChat(request, overhead)
}

func (p *tracedHardCrawler) Chat(ctx context.Context, request ChatRequest) (ChatResponse, error) {
	ordinal := p.Calls.Add(1)
	_, facts := auditVisibleReceipts(request)
	// Repeated rejection data is deliberately in the host-context tool receipt,
	// not trusted system controls. The observer reads both without changing either.
	for _, message := range request.Messages {
		if message.Role == "tool" && strings.HasPrefix(message.ToolCallID, "host-context-") {
			var memory map[string]json.RawMessage
			if json.Unmarshal([]byte(message.Content), &memory) == nil {
				for _, key := range []string{"repeated_rejection", "read_information_recovery"} {
					if memory[key] != nil {
						facts[key] = memory[key]
					}
				}
			}
		}
	}
	var host HostProgress
	_ = json.Unmarshal(facts["host_progress"], &host)
	_, hasCorrection := facts["delivery_recovery"]
	loopSignal := facts["repeated_rejection"] != nil || facts["read_information_recovery"] != nil || host.SemanticCheck != nil && host.SemanticCheck.Repeated && host.NoProgressModelRounds > 0
	if loopSignal && !hasCorrection {
		auditSave(p.T, fmt.Sprintf("hard-real-%03d-loop-not-corrected", ordinal), request)
		p.Stop()
		return ChatResponse{}, errors.New("acceptance stopped: loop signal reached model boundary without delivery correction")
	}
	auditSave(p.T, fmt.Sprintf("hard-real-%03d-request", ordinal), request)
	p.T.Logf("REAL request=%d phase=%s idle=%d semantic=%+v", ordinal, host.Recovery.Phase, host.NoProgressModelRounds, host.SemanticCheck)
	started := time.Now()
	out, err := p.Provider.Chat(ctx, request)
	message := ""
	if err != nil {
		message = err.Error()
	}
	auditSave(p.T, fmt.Sprintf("hard-real-%03d-response", ordinal), map[string]any{"response": out, "error": message, "duration_ms": time.Since(started).Milliseconds(), "real_model": true})
	return out, err
}

func TestHardCrawlerFixtureDifficulty(t *testing.T) {
	for _, c := range hardCrawlerCases() {
		if len(c.Pages) != 5 || len(c.Gold) != 3 || len(c.Fields) != 5 {
			t.Fatal("difficulty contract", c.Name)
		}
		for _, row := range c.Gold {
			for _, key := range c.Fields {
				if row[key] == "" {
					t.Fatal(c.Name, key)
				}
			}
		}
	}
}

// Explicit opt-in. Two different task types must both succeed consecutively.
// Each task gets its own six-minute deadline and three-minute review.
// Source is a controlled multi-page site; model, BGE, SQL, Redis and runner are
// real. This proves pipeline behavior on these fixtures, not arbitrary web sites.
func TestHardCrawlerRealTwoTypes(t *testing.T) {
	if os.Getenv("GORELAY_HARD_REAL_ACCEPTANCE") != "1" {
		t.Skip("explicit time-bounded real-model opt-in required")
	}
	cases := hardCrawlerCases()
	profile := envDefault("GORELAY_HARD_REAL_PROFILE", "baseline")
	if profile == "harder" {
		cases = harderCrawlerCases()
	} else if profile != "baseline" {
		t.Fatal("unknown real crawler acceptance profile", profile)
	}
	pages := map[string]string{}
	for _, c := range cases {
		for path, body := range c.Pages {
			pages[path] = body
		}
	}
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := pages[r.URL.RequestURI()]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if strings.HasPrefix(body, "{") {
			w.Header().Set("Content-Type", "application/json")
		} else {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
		}
		fmt.Fprint(w, body)
	}))
	defer source.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "CONNECT" || r.Host != "8.8.8.8:80" {
			w.WriteHeader(400)
			return
		}
		upstream, err := net.DialTimeout("tcp", strings.TrimPrefix(source.URL, "http://"), time.Second)
		if err != nil {
			w.WriteHeader(502)
			return
		}
		defer upstream.Close()
		conn, buffer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		buffer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		buffer.Flush()
		go func() { io.Copy(upstream, buffer) }()
		io.Copy(conn, upstream)
	}))
	defer proxy.Close()
	t.Setenv("GORELAY_ACQUISITION_PROXY", proxy.URL)
	t.Setenv("GORELAY_ACTION_MODE", "code")
	t.Setenv("GORELAY_EXPERIMENT_UNLIMITED_BUDGET", "1")
	window, err := time.ParseDuration(envDefault("GORELAY_HARD_REAL_WINDOW", "6m"))
	if err != nil || window <= 0 || window > 6*time.Minute {
		t.Fatal("each task window must be positive and at most six minutes")
	}
	t.Setenv("GORELAY_RUN_TIMEOUT", window.String())
	t.Setenv("GORELAY_ADMIN_TOKEN", "hard-acceptance-only")
	db := testdb.Open(t)
	svc := NewService(db)
	svc.RequireSpendPolicy = true
	if err := svc.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&task.TaskEvent{}); err != nil {
		t.Fatal(err)
	}
	sql, _ := db.DB()
	sql.SetMaxOpenConns(8)
	sql.SetMaxIdleConns(4)
	priorDB, priorQueue := repository.DB, queue.Client
	repository.DB = db
	t.Cleanup(func() { repository.DB, queue.Client = priorDB, priorQueue })
	runner := actualRunnerForChain(t)
	svc.Runner = runner
	provider, err := NewModelProvider(ProviderConfig{BaseURL: os.Getenv("GORELAY_HARD_REAL_BASE_URL"), APIKey: os.Getenv("GORELAY_HARD_REAL_API_KEY"), Model: os.Getenv("GORELAY_HARD_REAL_MODEL"), Timeout: window})
	if err != nil {
		t.Fatal(err)
	}
	embed, err := NewModelProvider(ProviderConfig{BaseURL: os.Getenv("GORELAY_TEST_EMBED_BASE_URL"), APIKey: "local", Model: "bge-small-zh-v1.5-f16", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	svc.LoopEmbedding = EmbeddingAdapter{Provider: embed}
	// No RAG/vector service or indexer participates in this acceptance.
	client := redis.NewClient(&redis.Options{Addr: os.Getenv("GORELAY_LIVE_REDIS_ADDR"), ContextTimeoutEnabled: true})
	defer client.Close()
	queue.Client = client
	stream := "hard-crawler-" + agentdata.Token()
	group := "hard"
	configPath := filepath.Join(t.TempDir(), "config.json")
	raw, _ := json.Marshal(map[string]any{"redis": map[string]any{"stream": stream, "addr": os.Getenv("GORELAY_LIVE_REDIS_ADDR")}})
	os.WriteFile(configPath, raw, 0600)
	t.Setenv("LUA_SPIDER_CONFIG", configPath)
	config.InitConfig()
	if err = client.XGroupCreateMkStream(context.Background(), stream, group, "0").Err(); err != nil {
		t.Fatal(err)
	}
	defer client.Del(context.Background(), stream)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	traced := &tracedHardCrawler{Provider: provider, T: t, Stop: stop}
	svc.Model = traced
	wcfg := worker.DefaultConfig(stream, group, "hard-real-worker")
	wcfg.ReadBlock = 100 * time.Millisecond
	processor, err := worker.NewProcessor(client, wcfg, svc.ExecuteTask)
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := worker.NewConsumer(client, wcfg, processor)
	if err != nil {
		t.Fatal(err)
	}
	reclaimer, err := worker.NewReclaimer(client, wcfg, processor)
	if err != nil {
		t.Fatal(err)
	}
	wsvc, err := worker.NewService(consumer, reclaimer)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 3)
	go func() { done <- svc.Run(ctx) }()
	go func() { done <- wsvc.Run(ctx) }()
	go func() {
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				done <- ctx.Err()
				return
			case <-tick.C:
				if _, e := service.PublishPendingTaskOutboxes(ctx, 8); e != nil {
					done <- e
					return
				}
			}
		}
	}()
	defer func() {
		stop()
		for i := 0; i < 3; i++ {
			select {
			case <-done:
			case <-time.After(8 * time.Second):
				t.Error("acceptance component failed to stop")
			}
		}
	}()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterRoutes(router, svc)
	request := func(method, path string, input any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(input)
		req := httptest.NewRequest(method, path, strings.NewReader(string(raw)))
		req.Header.Set("Authorization", "Bearer hard-acceptance-only")
		req.Header.Set("Content-Type", "application/json")
		out := httptest.NewRecorder()
		router.ServeHTTP(out, req)
		return out
	}
	started := time.Now()
	accepted := []map[string]any{}
	for _, c := range cases {
		taskStarted := time.Now()
		initialCalls := traced.Calls.Load()
		review := time.NewTicker(3 * time.Minute)
		deadline := time.NewTimer(window)
		defer review.Stop()
		defer deadline.Stop()
		fields := []FieldRequirement{}
		for _, name := range c.Fields {
			fields = append(fields, FieldRequirement{Name: name, Type: "string"})
		}
		spec := TaskSpec{Goal: c.Goal, ContractVersion: "v2", SchemaVersion: "v1", RequiredFields: fields, JoinKeys: []string{c.Fields[0]}, MinRecords: len(c.Gold), RequireFieldSupport: true, RequirePublishedCapability: true, SourceScope: []string{"8.8.8.8"}}
		submission := request("POST", "/api/v1/agent/runs", CreateRunRequest{RequestKey: c.Name, Goal: c.Goal, Spec: &spec, SourceScope: spec.SourceScope})
		if submission.Code != 202 {
			t.Fatal(submission.Code, submission.Body.String())
		}
		var run agentdata.Run
		json.Unmarshal(submission.Body.Bytes(), &run)
		for {
			out := request("GET", "/api/v1/agent/runs/"+run.ID, nil)
			var view struct {
				Run        agentdata.Run         `json:"run"`
				Result     TaskResult            `json:"result"`
				Operations []agentdata.Operation `json:"operations"`
			}
			if out.Code != 200 || json.Unmarshal(out.Body.Bytes(), &view) != nil {
				t.Fatal("observation", out.Code)
			}
			auditSave(t, "hard-"+c.Name+"-latest", json.RawMessage(out.Body.Bytes()))
			if view.Run.IsTerminal() {
				if view.Run.Status != "completed" {
					t.Fatalf("%s ended %s: %s", c.Name, view.Run.Status, view.Run.LastError)
				}
				if len(view.Result.Records) != len(c.Gold) || view.Result.ReusableCapabilities < 1 {
					t.Fatal("wrong record/capability count", c.Name, len(view.Result.Records), view.Result.ReusableCapabilities)
				}
				seen := map[string]bool{}
				for _, row := range view.Result.Records {
					url, _ := row.Fields["source_url"].(string)
					want := c.Gold[url]
					if want == nil || seen[url] {
						t.Fatal("unknown or duplicate record", url)
					}
					seen[url] = true
					for field, value := range want {
						if row.Fields[field] != value {
							t.Fatalf("%s field=%s got=%v want=%s", c.Name, field, row.Fields[field], value)
						}
					}
				}
				review.Stop()
				deadline.Stop()
				accepted = append(accepted, map[string]any{"type": c.Name, "records": len(view.Result.Records), "elapsed_seconds": time.Since(taskStarted).Seconds(), "model_calls": traced.Calls.Load() - initialCalls, "task_window_seconds": window.Seconds()})
				t.Logf("ACCEPTED %s records=%d task_elapsed=%s model_calls=%d", c.Name, len(view.Result.Records), time.Since(taskStarted), traced.Calls.Load()-initialCalls)
				break
			}
			select {
			case <-review.C:
				t.Logf("THREE MINUTE REVIEW task=%s status=%s ops=%d records=%d calls=%d host=%s", c.Name, view.Run.Status, len(view.Operations), len(view.Result.Records), traced.Calls.Load(), view.Run.HostProgressJSON)
				auditSave(t, "hard-"+c.Name+"-three-minute-review", json.RawMessage(out.Body.Bytes()))
			case <-deadline.C:
				request("POST", "/api/v1/agent/runs/"+run.ID+"/cancel", nil)
				auditSave(t, "hard-"+c.Name+"-stopped", json.RawMessage(request("GET", "/api/v1/agent/runs/"+run.ID, nil).Body.Bytes()))
				t.Fatalf("task %s exceeded its own %s window", c.Name, window)
			case <-ctx.Done():
				request("POST", "/api/v1/agent/runs/"+run.ID+"/cancel", nil)
				auditSave(t, "hard-"+c.Name+"-stopped", json.RawMessage(request("GET", "/api/v1/agent/runs/"+run.ID, nil).Body.Bytes()))
				t.Fatalf("task %s stopped at %s: %v", c.Name, time.Since(taskStarted), ctx.Err())
			case e := <-done:
				t.Fatal("background component stopped", e)
			case <-time.After(time.Second):
			}
		}
	}
	auditSave(t, "hard-real-acceptance-summary", map[string]any{"profile": profile, "consecutive_successes": len(accepted), "tasks": accepted, "different_types": true, "per_task_window_seconds": window.Seconds(), "model_calls": traced.Calls.Load(), "elapsed_seconds": time.Since(started).Seconds(), "source": "controlled multi-page HTML and JSON sites", "real_model": true, "rag": false})
}
