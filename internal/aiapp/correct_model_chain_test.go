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
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/config"
	"go-lua-crawler/internal/engine"
	"go-lua-crawler/internal/knowledge"
	"go-lua-crawler/internal/queue"
	"go-lua-crawler/internal/repository"
	"go-lua-crawler/internal/service"
	"go-lua-crawler/internal/task"
	"go-lua-crawler/internal/testdb"
	"go-lua-crawler/internal/worker"
)

const correctProcurementLua = `local manifest=assert(json_decode(assert(http_get(TARGET_URL))));local items={};for _,url in ipairs(manifest.urls) do local h=assert(http_get(url));local title=assert(html_find(h,"h6.title"));local metadata=assert(html_find(h,"span.metax"));local date=assert(metadata:match("日期：([%d%-]+)"));local body=assert(html_find(h,"div.v_news_content"));table.insert(items,{title=title,published_date=date,body=body,source_url=url,_evidence={title={selector="h6.title",index=0},published_date={selector="span.metax",index=0,pattern="日期：([0-9-]+)"},body={selector="div.v_news_content",index=0}}}) end;return true,{items=items}`

// A correct model is an immutable decision function over the current request.
// No URL->progress/submitted ledger exists outside the Go application.
func correctChainDecision(request ChatRequest) (ChatResponse, error) {
	if len(request.Tools) == 0 {
		var input struct {
			Evidence knowledge.EvidenceSet `json:"evidence"`
		}
		for _, message := range request.Messages {
			if message.Role == "user" {
				_ = json.Unmarshal([]byte(message.Content), &input)
			}
		}
		pattern := regexp.MustCompile(`预算金额[：:]\s*([0-9]+万元)`)
		answer := map[string]any{"status": "insufficient_evidence", "answer": "当前证据未包含预算金额。", "citations": []string{}}
		for _, evidence := range input.Evidence.Entries {
			if match := pattern.FindStringSubmatch(evidence.Text); len(match) == 2 {
				answer = map[string]any{"status": "answered", "answer": "公开正文中的预算金额为" + match[1] + "。", "citations": []string{evidence.ID}}
				break
			}
		}
		content, _ := json.Marshal(answer)
		return ChatResponse{Message: Message{Role: "assistant", Content: string(content)}, Model: "correct-mock"}, nil
	}
	receipts, facts := auditVisibleReceipts(request)
	dataOf := func(out ToolResult) map[string]json.RawMessage {
		var data map[string]json.RawMessage
		_ = json.Unmarshal(out.Data, &data)
		if data["observation"] != nil {
			var value map[string]json.RawMessage
			if json.Unmarshal(data["observation"], &value) == nil {
				data = value
			}
		}
		return data
	}
	propose := func(name string, args any) (ChatResponse, error) {
		raw, _ := json.Marshal(args)
		return ChatResponse{Message: Message{Role: "assistant", ToolCalls: []ToolCall{{ID: agentdata.Hash(request)[:24], Type: "function", Function: FunctionCall{Name: name, Arguments: string(raw)}}}}, Model: "correct-mock"}, nil
	}
	var children []map[string]any
	_ = json.Unmarshal(facts["children"], &children)
	var coverage CoverageReport
	_ = json.Unmarshal(facts["coverage"], &coverage)
	if len(children) > 0 || (receipts["reuse_knowledge"].Name != "" && coverage.Satisfied) {
		if !coverage.Satisfied {
			return ChatResponse{}, fmt.Errorf("correct model observed incomplete child delivery")
		}
		var spec TaskSpec
		_ = json.Unmarshal(facts["frozen_spec"], &spec)
		required := spec.RequireSummary
		for _, message := range request.Messages {
			if message.Role == "user" {
				var submitted CreateRunRequest
				if json.Unmarshal([]byte(message.Content), &submitted) == nil {
					required = required || submitted.RequireSummary
				}
			}
		}
		if required {
			if out := receipts["read_data"]; out.Name != "" && oracleEvidenceRead(request, out) {
				if out.Error != "" {
					if out.Failure != nil && out.Failure.Recovery != nil && out.Failure.Recovery.Tool == "search_knowledge" {
						return propose("search_knowledge", map[string]any{"query": "公开公告中的预算金额是多少？请引用当前原文证据。"})
					}
					return ChatResponse{}, fmt.Errorf("correct evidence read failed: %s", out.Error)
				}
				data := dataOf(out)
				if data["text"] == nil && data["value"] != nil {
					var object map[string]json.RawMessage
					if json.Unmarshal(data["value"], &object) == nil {
						if raw := object["entries"]; raw != nil {
							var ref struct {
								ReadWith map[string]any `json:"read_with"`
							}
							_ = json.Unmarshal(raw, &ref)
							if len(ref.ReadWith) > 0 {
								return propose("read_data", ref.ReadWith)
							}
						}
						var id string
						_ = json.Unmarshal(object["id"], &id)
						if strings.HasPrefix(id, "ev_") {
							return propose("read_data", map[string]any{"handle": id, "pointer": "/text", "limit": 1000})
						}
					}
					var entries []map[string]json.RawMessage
					if json.Unmarshal(data["value"], &entries) == nil && len(entries) > 0 {
						var ref struct {
							ReadWith map[string]any `json:"read_with"`
						}
						raw, _ := json.Marshal(entries[0])
						_ = json.Unmarshal(raw, &ref)
						if len(ref.ReadWith) > 0 {
							return propose("read_data", ref.ReadWith)
						}
						var id string
						_ = json.Unmarshal(entries[0]["id"], &id)
						if strings.HasPrefix(id, "ev_") {
							return propose("read_data", map[string]any{"handle": id, "pointer": "/text", "limit": 1000})
						}
					}
					var cursor string
					_ = json.Unmarshal(data["next_cursor"], &cursor)
					if cursor != "" {
						return propose("read_data", map[string]any{"cursor": cursor})
					}
				}
				var text, handle, cursor string
				_ = json.Unmarshal(data["text"], &text)
				_ = json.Unmarshal(data["handle"], &handle)
				_ = json.Unmarshal(data["next_cursor"], &cursor)
				if text == "" && data["text"] != nil {
					var omitted struct {
						Preview  string         `json:"preview"`
						ReadWith map[string]any `json:"read_with"`
					}
					if json.Unmarshal(data["text"], &omitted) == nil {
						if len(omitted.ReadWith) > 0 {
							return propose("read_data", omitted.ReadWith)
						}
						if omitted.Preview != "" {
							var offset int
							_ = json.Unmarshal(data["offset"], &offset)
							return propose("read_data", map[string]any{"handle": handle, "pointer": "/text", "offset": offset + max(0, len([]rune(omitted.Preview))-32), "limit": 256})
						}
					}
				}
				match := regexp.MustCompile(`预算金额[：:]\s*([0-9]+万元)`).FindStringSubmatch(text)
				if len(match) == 2 && strings.HasPrefix(handle, "ev_") {
					citations := []string{handle}
					requirements := []RequirementResult{}
					for _, r := range spec.Requirements {
						requirements = append(requirements, RequirementResult{ID: r.ID, Status: "satisfied", Explanation: "The retrieved current notice explicitly states its budget", Citations: citations})
					}
					summary := "公开正文中的预算金额为" + match[1] + "。"
					if spec.RequireExtractiveSummary {
						summary = match[0]
					}
					return propose("finish_task", map[string]any{"status": "completed", "reason": "full records and current evidence support the requested summary", "summary": summary, "citations": citations, "requirements": requirements})
				}
				if cursor != "" {
					return propose("read_data", map[string]any{"cursor": cursor})
				}
				var catalog []struct {
					ID string `json:"id"`
				}
				var completed map[string]bool
				_ = json.Unmarshal(facts["evidence_catalog"], &catalog)
				_ = json.Unmarshal(facts["evidence_reading"], &completed)
				for _, entry := range catalog {
					if entry.ID != handle && !completed[entry.ID] {
						return propose("read_data", map[string]any{"handle": entry.ID, "pointer": "/text", "limit": 1000})
					}
				}
				return ChatResponse{}, fmt.Errorf("correct evidence reader exhausted without the requested budget")
			}
			if out := receipts["search_knowledge"]; out.Name != "" {
				if out.Error != "" {
					return ChatResponse{}, fmt.Errorf("knowledge search failed: %s", out.Error)
				}
				var readWith map[string]any
				_ = json.Unmarshal(dataOf(out)["read_with"], &readWith)
				if len(readWith) > 0 {
					return propose("read_data", readWith)
				}
				var entries []struct {
					ID   string          `json:"id"`
					Text json.RawMessage `json:"text"`
				}
				_ = json.Unmarshal(dataOf(out)["entries"], &entries)
				for _, entry := range entries {
					var text string
					if json.Unmarshal(entry.Text, &text) != nil {
						var partial struct {
							Preview string `json:"preview"`
						}
						_ = json.Unmarshal(entry.Text, &partial)
						text = partial.Preview
					}
					if strings.Contains(text, "预算") {
						return propose("read_data", map[string]any{"handle": entry.ID, "pointer": "/text", "limit": 1000})
					}
				}
				if len(entries) > 0 {
					// A bounded preview is a locator, not a guarantee that the
					// requested fact appears in the first visible characters.
					return propose("read_data", map[string]any{"handle": entries[0].ID, "pointer": "/text", "limit": 1000})
				}
				return ChatResponse{}, fmt.Errorf("no current evidence is available to read")
			}
			return propose("search_knowledge", map[string]any{"query": "公开公告中的预算金额是多少？请引用原文证据。"})
		}
		return propose("finish_task", map[string]any{"status": "completed", "reason": "three complete source records satisfy frozen contract", "requirements": []any{}})
	}
	if out := receipts["register_source"]; out.Name != "" {
		if out.Error != "" {
			return ChatResponse{}, fmt.Errorf("correct registration failed: %s", out.Error)
		}
		return propose("submit_acquisition", map[string]any{"source_id": "audit-source", "url": "https://fixture.example/releases"})
	}
	if out := receipts["validate_program"]; out.Name != "" {
		if out.Error != "" {
			return ChatResponse{}, fmt.Errorf("correct program failed: %s", out.Error)
		}
		var id string
		_ = json.Unmarshal(dataOf(out)["id"], &id)
		if id == "" {
			return ChatResponse{}, fmt.Errorf("valid candidate identity disappeared from model-visible receipt")
		}
		return propose("register_source", map[string]any{"candidate_id": id, "description": "complete notice list-to-detail adapter"})
	}
	if out := receipts["list_sources"]; out.Name != "" {
		if out.Error != "" {
			return ChatResponse{}, fmt.Errorf("catalog failed: %s", out.Error)
		}
		var catalog []SourceCapability
		_ = json.Unmarshal(dataOf(out)["active"], &catalog)
		for _, source := range catalog {
			if source.Source.SourceID == "audit-source" {
				target := "https://fixture.example/releases"
				for _, m := range request.Messages {
					if m.Role != "user" {
						continue
					}
					var in CreateRunRequest
					if json.Unmarshal([]byte(m.Content), &in) == nil {
						if observed := regexp.MustCompile(`https?://[^\s"<>]+`).FindString(in.Goal); observed != "" {
							target = observed
						}
					}
				}
				args := map[string]any{"source_id": source.Source.SourceID, "url": target}
				var params map[string]any
				if source.InputExampleJSON != "" && json.Unmarshal([]byte(source.InputExampleJSON), &params) == nil {
					args["params"] = params
				}
				return propose("submit_acquisition", args)
			}
		}
		return propose("validate_program", map[string]any{"code": correctProcurementLua, "kind": "adapter", "source_id": "audit-source", "purpose": "complete public procurement notices", "url": "https://fixture.example/releases"})
	}
	return propose("list_sources", map[string]any{})
}

// The last read can belong to source discovery. Only an observed evidence
// handle or a receipt read of knowledge is an evidence-reading continuation.
func oracleEvidenceRead(request ChatRequest, out ToolResult) bool {
	// A fresh retrieval after a rejected read supersedes that old error.
	readPosition, searchPosition := -1, -1
	for i, m := range request.Messages {
		if m.Role != "tool" {
			continue
		}
		var receipt ToolResult
		if json.Unmarshal([]byte(m.Content), &receipt) != nil {
			continue
		}
		if receipt.CallID == out.CallID {
			readPosition = i
		}
		if receipt.Name == "search_knowledge" && receipt.Error == "" {
			searchPosition = i
		}
	}
	if readPosition >= 0 && searchPosition > readPosition {
		return false
	}
	var data struct {
		Handle string `json:"handle"`
	}
	_ = json.Unmarshal(out.Data, &data)
	if strings.HasPrefix(data.Handle, "ev_") {
		return true
	}
	for _, m := range request.Messages {
		for _, call := range m.ToolCalls {
			if call.ID != out.CallID {
				continue
			}
			var args dataReadArgs
			_ = json.Unmarshal([]byte(call.Function.Arguments), &args)
			if strings.HasPrefix(args.Handle, "ev_") {
				return true
			}
			for _, source := range request.Messages {
				var receipt ToolResult
				if source.Role == "tool" && json.Unmarshal([]byte(source.Content), &receipt) == nil && receipt.CallID == args.Handle && receipt.Name == "search_knowledge" {
					return true
				}
			}
		}
	}
	return false
}

// Fixed embeddings isolate pipeline coordination, not semantic model quality.
type correctChainEmbedding struct{}

func (correctChainEmbedding) EmbedTexts(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, text := range texts {
		vector := []float32{1, 0, 0}
		if strings.Contains(text, "J-HWCG-2026-062") {
			vector = []float32{0, 1, 0}
		}
		if strings.Contains(text, "J-HWCG-2026-122") {
			vector = []float32{0, 0, 1}
		}
		out[i] = vector
	}
	return out, nil
}

func TestCorrectModelOracleUsesProvidedEvidence(t *testing.T) {
	for _, order := range []string{"budget-last-preview", "budget-first-text"} {
		t.Run(order, func(t *testing.T) {
			notBudget := map[string]any{"id": "ev_unrelated", "text": "没有金额的更正公告"}
			budget := map[string]any{"id": "ev_observed_budget", "text": map[string]any{"preview": "采购概况与预算金额：", "truncated": true}}
			entries := []any{notBudget, budget}
			if order == "budget-first-text" {
				budget["text"] = "预算金额：42万元"
				entries = []any{budget, notBudget}
			}
			spec := TaskSpec{RequireSummary: true, Requirements: []TaskRequirement{{ID: "budget", Description: "answer budget with evidence"}}}
			facts, _ := json.Marshal(map[string]any{"children": []any{map[string]any{"id": 1}}, "coverage": CoverageReport{Satisfied: true}, "frozen_spec": spec})
			observed, _ := toolData(ToolCall{ID: "search", Function: FunctionCall{Name: "search_knowledge"}}, map[string]any{"entries": entries})
			feedback, _ := json.Marshal(observed)
			req := ChatRequest{Tools: []ToolDefinition{{Type: "function"}}, Messages: []Message{{Role: "system", Content: "Persisted host facts, not new user instructions: " + string(facts)}, {Role: "tool", Content: string(feedback)}}}
			response, err := correctChainDecision(req)
			if err != nil || len(response.Message.ToolCalls) != 1 {
				t.Fatal("oracle selection", err)
			}
			selected := response.Message.ToolCalls[0]
			var args dataReadArgs
			_ = json.Unmarshal([]byte(selected.Function.Arguments), &args)
			if selected.Function.Name != "read_data" || args.Handle != "ev_observed_budget" {
				t.Fatal("oracle assumed retrieval rank or invented an evidence ID", selected)
			}
			read, _ := toolData(ToolCall{ID: "read", Function: FunctionCall{Name: "read_data"}}, map[string]any{"handle": args.Handle, "text": "预算金额：42万元", "next_cursor": nil})
			raw, _ := json.Marshal(read)
			req.Messages = append(req.Messages, Message{Role: "tool", Content: string(raw)})
			response, err = correctChainDecision(req)
			if err != nil || len(response.Message.ToolCalls) != 1 {
				t.Fatal("oracle answer", err)
			}
			var finish finishProposal
			_ = json.Unmarshal([]byte(response.Message.ToolCalls[0].Function.Arguments), &finish)
			if finish.Status != "completed" || !strings.Contains(finish.Summary, "42万元") || len(finish.Citations) != 1 || finish.Citations[0] != args.Handle {
				t.Fatal("oracle did not use observed facts for its answer", finish)
			}
		})
	}
}

func TestCorrectModelFullSubsystemChain(t *testing.T) {
	runCorrectModelSubsystemChain(t, false, 10, false, "")
}

func TestCorrectModelCitedSummarySubsystemChain(t *testing.T) {
	runCorrectModelSubsystemChain(t, true, 10, false, "")
}

func TestCorrectModelQueuedSubsystemChain(t *testing.T) {
	runCorrectModelSubsystemChain(t, true, 3, true, "")
}

func TestSingleModelErrorFullSubsystemChain(t *testing.T) {
	for _, fault := range []string{"premature_finish", "invalid_lua"} {
		t.Run(fault, func(t *testing.T) {
			runCorrectModelSubsystemChain(t, true, 1, false, fault)
		})
	}
}

// The deliberate model error is selected only at the first visible request;
// all recovery decisions depend on the application's persisted feedback.
func singleErrorChainDecision(request ChatRequest, fault string) (ChatResponse, error) {
	if fault == "" || len(request.Tools) == 0 {
		return correctChainDecision(request)
	}
	receipts, facts := auditVisibleReceipts(request)
	propose := func(name string, args any) (ChatResponse, error) {
		raw, _ := json.Marshal(args)
		return ChatResponse{Message: Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "injected-model-" + fault, Type: "function", Function: FunctionCall{Name: name, Arguments: string(raw)}}}}, Model: "single-error-mock"}, nil
	}
	var children []any
	_ = json.Unmarshal(facts["children"], &children)
	if len(receipts) == 0 && len(children) == 0 {
		switch fault {
		case "premature_finish":
			return propose("finish_task", map[string]any{"status": "completed", "reason": "incorrect model claims completion"})
		case "invalid_lua":
			return propose("validate_program", map[string]any{"code": "return true,{items=}", "kind": "adapter", "source_id": "audit-source", "purpose": "complete public procurement notices", "url": "https://fixture.example/releases"})
		}
	}
	if out := receipts["validate_program"]; fault == "invalid_lua" && out.CallID == "injected-model-"+fault && out.Error != "" {
		raw, _ := json.Marshal(map[string]any{"code": correctProcurementLua, "kind": "adapter", "source_id": "audit-source", "purpose": "complete public procurement notices", "url": "https://fixture.example/releases", "repair_of": out.CallID})
		return ChatResponse{Message: Message{Role: "assistant", ToolCalls: []ToolCall{{ID: agentdata.Hash(request)[:24], Type: "function", Function: FunctionCall{Name: "validate_program", Arguments: string(raw)}}}}, Model: "single-error-mock"}, nil
	}
	return correctChainDecision(request)
}

func runCorrectModelSubsystemChain(t *testing.T, requireSummary bool, count int, queueAhead bool, modelFault string, scenarios ...progressiveCase) {
	var scenario *progressiveCase
	if len(scenarios) > 0 {
		scenario = &scenarios[0]
	}
	if scenario != nil && scenario.AdaptiveCrawler && (scenario.Fault == "wrong_program_kind" || scenario.Fault == "missing_field_evidence") {
		// Deliberate pressure fixture. Production's 500k-token trigger must
		// remain untouched while this test verifies actual compression repair.
		t.Setenv("GORELAY_AI_COMPACT_AT_TOKENS", "12000")
		t.Setenv("GORELAY_AI_COMPACT_TO_TOKENS", "9600")
	}
	explorationCase := scenario != nil && (scenario.Fault == "novel_exploration" || scenario.Fault == "recover_exploration" || scenario.Fault == "self_receipt_read")
	crawlerOnly := scenario != nil && scenario.AdaptiveCrawler
	actualRunnerCase := scenario != nil && scenario.ActualRunner
	networkBodies := map[string]string{}
	if explorationCase || actualRunnerCase {
		// Route the existing host-configured CONNECT transport to a frozen local
		// source. No production fetcher/validator is bypassed; no Internet fetch.
		source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if actualRunnerCase {
				if body, ok := networkBodies[r.URL.Path]; ok {
					if r.URL.Path == "/releases" && !scenario.SelectorDiscovery {
						w.Header().Set("Content-Type", "application/json")
					}
					_, _ = fmt.Fprint(w, body)
					return
				}
				w.WriteHeader(404)
				return
			}
			fmt.Fprintf(w, "<article>Unrelated navigation %s</article>", r.URL.Path)
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
			_, _ = buffer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
			_ = buffer.Flush()
			go func() { _, _ = io.Copy(upstream, buffer) }()
			_, _ = io.Copy(conn, upstream)
		}))
		defer proxy.Close()
		t.Setenv("GORELAY_ACQUISITION_PROXY", proxy.URL)
	}
	expectedStatus := "completed"
	if scenario != nil {
		expectedStatus = scenario.Expected
	}
	addr, qurl := os.Getenv("GORELAY_LIVE_REDIS_ADDR"), os.Getenv("GORELAY_TEST_QDRANT_URL")
	if addr == "" || !crawlerOnly && qurl == "" {
		t.Skip("explicit Redis/Qdrant endpoints required")
	}
	db := testdb.Open(t)
	s := NewService(db)
	s.Fingerprint = "correct-mock-embedding-v1"
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&task.TaskEvent{}); err != nil {
		t.Fatal(err)
	}
	sql, _ := db.DB()
	sql.SetMaxOpenConns(8)
	sql.SetMaxIdleConns(4)
	previousDB, previousQueue := repository.DB, queue.Client
	repository.DB = db
	t.Cleanup(func() { repository.DB, queue.Client = previousDB, previousQueue })
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	client := redis.NewClient(&redis.Options{Addr: addr, ContextTimeoutEnabled: true})
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	queue.Client = client
	stream := "correct-model-chain-" + agentdata.Token()
	group := "baseline"
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		if err := client.Del(cleanupCtx, stream).Err(); err != nil {
			t.Error("temporary stream cleanup", err)
		}
	})
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg, _ := json.Marshal(map[string]any{"redis": map[string]any{"stream": stream, "addr": addr}})
	if err := os.WriteFile(configPath, cfg, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LUA_SPIDER_CONFIG", configPath)
	config.InitConfig()
	if err := client.XGroupCreateMkStream(ctx, stream, group, "0").Err(); err != nil {
		t.Fatal(err)
	}
	gold := map[string]map[string]any{}
	bodies := map[string]string{}
	urls := []string{}
	for i := 1; i <= 3; i++ {
		file := filepath.Join("..", "..", "output", "experiments", "mvp-procurement", "gold", fmt.Sprintf("A0-detail-%d.html", i))
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		originalBody := string(raw)
		doc, err := goquery.NewDocumentFromReader(strings.NewReader(originalBody))
		if err != nil {
			t.Fatal(err)
		}
		if scenario != nil && scenario.AdaptiveCrawler {
			// Vary source markup behind the normal HTTP fetch. The provider gets
			// neither these names nor the original document/Gold.
			seed := agentdata.Hash([]any{scenario.LayoutVariant, "layout"})[:10]
			for _, item := range []struct{ selector, tag string }{{"h6.title", fmt.Sprintf("h%d", scenario.LayoutVariant+1)}, {"span.metax", "time"}, {"div.v_news_content", "div"}} {
				doc.Find(item.selector).Each(func(_ int, n *goquery.Selection) {
					n.Nodes[0].Data = item.tag
					n.SetAttr("class", "node_"+seed+"_"+item.tag)
				})
			}
			changed, _ := doc.Html()
			raw = []byte(changed)
			// Gold is assertion-only and is read using the original source
			// selectors below. Keep its parser separate from changed markup.
			doc, err = goquery.NewDocumentFromReader(strings.NewReader(originalBody))
			if err != nil {
				t.Fatal(err)
			}
		}
		url := fmt.Sprintf("https://fixture.example/notice/%d", i)
		if actualRunnerCase {
			url = fmt.Sprintf("http://8.8.8.8/notice/%d", i)
		}
		urls = append(urls, url)
		bodies[url] = string(raw)
		networkBodies[fmt.Sprintf("/notice/%d", i)] = string(raw)
		date := regexp.MustCompile(`日期：([0-9-]+)`).FindStringSubmatch(normalizeEvidenceText(doc.Find("span.metax").First().Text()))
		if len(date) != 2 {
			t.Fatal("frozen source date absent", i)
		}
		gold[url] = map[string]any{"title": normalizeEvidenceText(doc.Find("h6.title").First().Text()), "published_date": date[1], "body": normalizeEvidenceText(doc.Find("div.v_news_content").First().Text()), "source_url": url}
		if scenario != nil && scenario.CompleteText {
			full, err := acquisition.SourceText(string(raw))
			if err != nil {
				t.Fatal(err)
			}
			gold[url]["body"] = full
		}
	}
	manifest, _ := json.Marshal(map[string]any{"urls": urls})
	if scenario != nil && scenario.SelectorDiscovery {
		var listing strings.Builder
		listing.WriteString("<html><body>")
		for i := 0; i < 20; i++ {
			fmt.Fprintf(&listing, "<section><h5>Unrelated section %d</h5></section>", i)
		}
		row, container := "li", `<ul id="notices">`
		if scenario.AdaptiveCrawler {
			row = "div"
			container = `<section class="collection_` + agentdata.Hash(scenario.LayoutVariant)[:10] + `">`
		}
		listing.WriteString(container)
		for _, url := range urls {
			fmt.Fprintf(&listing, `<%s><a href="%s">%s</a></%s>`, row, url, gold[url]["title"], row)
		}
		if scenario.AdaptiveCrawler {
			listing.WriteString("</section>")
		} else {
			listing.WriteString("</ul>")
		}
		listing.WriteString("</body></html>")
		manifest = []byte(listing.String())
	}
	networkBodies["/releases"] = string(manifest)
	var runnerCalls atomic.Int32
	runner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in RunnerRequest
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			w.WriteHeader(400)
			return
		}
		in.Limits.Timeout = engine.DefaultProgramLimits().Timeout
		entries := []acquisition.ReplayEntry{{Method: "GET", URL: in.Program.TargetURL, Status: 200, Body: string(manifest), FetchedAt: time.Now()}}
		if scenario != nil && scenario.PartialKnowledge && strings.Contains(in.Program.Code, "for i=1,3") {
			// The supplemental action starts at details. This replay fixture
			// must not force a listing request that the task no longer needs.
			entries = nil
		}
		for _, url := range urls {
			entries = append(entries, acquisition.ReplayEntry{Method: "GET", URL: url, Status: 200, Body: bodies[url], FetchedAt: time.Now()})
		}
		runnerCalls.Add(1)
		replay := acquisition.NewReplay(entries)
		result, err := engine.RunProgram(r.Context(), in.Program, in.Limits, replay.Fetch)
		response := RunnerResponse{Result: &result}
		if err != nil {
			response.Error = &RunnerError{Code: "execution_failed", Message: err.Error()}
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer runner.Close()
	s.Runner, _ = NewRunnerClient(runner.URL, "baseline-only")
	if actualRunnerCase {
		s.Runner = actualRunnerForChain(t)
	}
	var modelCalls, modelErrors atomic.Int64
	var modelRequests atomic.Int64
	var injectedProgramKind, injectedAdmission bool
	var injectedFieldEvidence bool
	var restoredPrograms atomic.Int64
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestOrdinal := modelRequests.Add(1)
		var req ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(400)
			return
		}
		var out ChatResponse
		var err error
		ordinal := int64(0)
		if len(req.Tools) > 0 {
			ordinal = modelCalls.Add(1)
		}
		if scenario != nil && scenario.Fault == "invalid_envelope" && (ordinal == 1 || scenario.WrongPercent == 100) && len(req.Tools) > 0 {
			modelErrors.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{}, "usage": Usage{TotalTokens: 300000}})
			return
		}
		if scenario != nil && scenario.AdaptiveCrawler {
			if scenario.Fault == "repeat_catalog" {
				out = ChatResponse{Message: Message{Role: "assistant", ReasoningContent: "I will inspect the same source catalog again before doing any acquisition.", ToolCalls: []ToolCall{{ID: "repeated-" + agentdata.Hash(req)[:20], Type: "function", Function: FunctionCall{Name: "list_sources", Arguments: `{}`}}}}}
			} else {
				out, err = adaptiveCrawlerDecision(req, scenario.LayoutVariant == 2)
			}
			if scenario.Fault == "missing_field_evidence" && err == nil {
				for _, message := range req.Messages {
					if message.Role == "tool" && strings.Contains(message.Content, `"program_corrections"`) {
						restoredPrograms.Add(1)
					}
				}
				for i := range out.Message.ToolCalls {
					call := &out.Message.ToolCalls[i]
					if call.Function.Name == "validate_program" && !injectedFieldEvidence {
						var args map[string]any
						json.Unmarshal([]byte(call.Function.Arguments), &args)
						code, _ := args["code"].(string)
						if !strings.Contains(code, "_evidence={") {
							t.Error("fault injector found no generated evidence metadata")
						} else {
							args["code"] = strings.Replace(code, "_evidence={", "_evidence=false and {", 1)
							raw, _ := json.Marshal(args)
							call.Function.Arguments = string(raw)
							injectedFieldEvidence = true
							modelErrors.Add(1)
						}
					}
				}
				out.Message.Content += strings.Repeat(" ", 12000)
			}
			if scenario.Fault == "wrong_program_kind" && err == nil {
				for _, message := range req.Messages {
					if message.Role == "tool" && strings.Contains(message.Content, `"program_corrections"`) {
						restoredPrograms.Add(1)
					}
				}
				// Deliberately make two wrong choices, using only proposals and
				// candidate IDs actually visible on the provider wire. This fault
				// injector has no fixture selectors, gold values or hidden code.
				for i := range out.Message.ToolCalls {
					call := &out.Message.ToolCalls[i]
					if call.Function.Name == "validate_program" && !injectedProgramKind {
						var args map[string]any
						json.Unmarshal([]byte(call.Function.Arguments), &args)
						args["kind"] = "composition"
						raw, _ := json.Marshal(args)
						call.Function.Arguments = string(raw)
						injectedProgramKind = true
						modelErrors.Add(1)
					}
				}
				receipts, _ := auditVisibleReceipts(req)
				var candidate struct {
					ID   string `json:"id"`
					Kind string `json:"kind"`
				}
				json.Unmarshal(receipts["validate_program"].Data, &candidate)
				if candidate.ID != "" && candidate.Kind == "composition" && !injectedAdmission {
					raw, _ := json.Marshal(map[string]any{"candidate_id": candidate.ID, "description": "publish the visible validated candidate"})
					out.Message.ToolCalls = []ToolCall{{ID: "admission-" + agentdata.Hash(req)[:20], Type: "function", Function: FunctionCall{Name: "register_source", Arguments: string(raw)}}}
					injectedAdmission = true
					modelErrors.Add(1)
				}
				// Valid trailing whitespace is transport pressure, not new facts.
				out.Message.Content += strings.Repeat(" ", 12000)
			}
		} else if scenario != nil && scenario.SelectorDiscovery {
			var handled bool
			out, handled, err = selectorDiscoveryDecision(req)
			if !handled && err == nil {
				out, _, err = progressiveChainDecision(req, *scenario, ordinal)
			}
		} else if scenario == nil {
			out, err = singleErrorChainDecision(req, modelFault)
		} else {
			if scenario.Delay > 0 {
				select {
				case <-r.Context().Done():
					return
				case <-time.After(scenario.Delay):
				}
			}
			var injected bool
			out, injected, err = progressiveChainDecision(req, *scenario, ordinal)
			if injected {
				modelErrors.Add(1)
			}
			out.Usage = &Usage{PromptTokens: 200000, CompletionTokens: 100000, TotalTokens: 300000}
		}
		if err != nil {
			t.Log("correct mock fixture error:", err)
			auditSave(t, "correct-model-fixture-error-"+agentdata.Hash(req)[:12], map[string]any{"error": err.Error(), "request": req})
			w.WriteHeader(422)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": err.Error()}})
			return
		}
		traceName := "correct-chain"
		if scenario != nil {
			traceName = scenario.Name
		} else if modelFault != "" {
			traceName = "single-error-" + modelFault
		}
		auditSave(t, fmt.Sprintf("%s-model-%04d", traceName, requestOrdinal), map[string]any{"request": req, "response": out, "ordinal": requestOrdinal, "real_model": false})
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "correct-mock", "choices": []any{map[string]any{"message": out.Message, "finish_reason": "tool_calls"}}, "usage": out.Usage})
	}))
	defer model.Close()
	s.Model, _ = NewModelProvider(ProviderConfig{BaseURL: model.URL, APIKey: "baseline-only", Model: "correct-mock"})
	var indexer *knowledge.Indexer
	if crawlerOnly {
		endpoint := os.Getenv("GORELAY_TEST_EMBED_BASE_URL")
		if endpoint == "" {
			t.Fatal("adaptive acceptance requires the existing local BGE endpoint")
		}
		p, e := NewModelProvider(ProviderConfig{BaseURL: endpoint, APIKey: "local-only", Model: envDefault("GORELAY_TEST_EMBED_MODEL", "bge-small-zh-v1.5-f16")})
		if e != nil {
			t.Fatal(e)
		}
		s.LoopEmbedding = EmbeddingAdapter{Provider: p}
		s.Fingerprint = "adaptive-local-bge-512-v1"
	} else {
		collection := "gorelay_correct_baseline_" + agentdata.Token()
		q, err := knowledge.NewQdrant(qurl, collection, "")
		if err != nil {
			t.Fatal(err)
		}
		embed := correctChainEmbedding{}
		if err = q.EnsureCollection(ctx, 3); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			req, _ := http.NewRequest("DELETE", qurl+"/collections/"+collection, nil)
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Error(err)
			} else {
				res.Body.Close()
			}
		})
		s.Retriever = &knowledge.Retriever{Store: s.Knowledge, Embed: embed, Vectors: q}
		indexer = &knowledge.Indexer{Store: s.Knowledge, Embed: embed, Vectors: q, Fingerprint: s.Fingerprint, ChunkSize: 6000, ChunkOverlap: 40}
	}
	wcfg := worker.DefaultConfig(stream, group, "actual-worker")
	wcfg.ReadBlock = 100 * time.Millisecond
	wcfg.ClaimInterval = time.Second
	wcfg.ClaimMinIdle = 10 * time.Second
	processor, err := worker.NewProcessor(client, wcfg, s.ExecuteTask)
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
	errorsCh := make(chan error, 4)
	done := make(chan string, 4)
	start := func(name string, run func(context.Context) error) {
		go func() {
			e := run(ctx)
			if e != nil && !errors.Is(e, context.Canceled) {
				errorsCh <- fmt.Errorf("%s: %w", name, e)
			}
			done <- name
		}()
	}
	start("agent-scheduler", s.Run)
	start("redis-worker", wsvc.Run)
	start("outbox-publisher", func(ctx context.Context) error {
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-tick.C:
				if _, err := service.PublishPendingTaskOutboxes(ctx, 8); err != nil {
					return err
				}
			}
		}
	})
	components := 3
	if indexer != nil {
		components++
		start("indexer", func(ctx context.Context) error {
			tick := time.NewTicker(50 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-tick.C:
					if err := indexer.Once(ctx); err != nil {
						return err
					}
				}
			}
		})
	}
	defer func() {
		cancel()
		deadline := time.After(8 * time.Second)
		for i := 0; i < components; i++ {
			select {
			case name := <-done:
				t.Log("stopped", name)
			case <-deadline:
				t.Error("background service did not stop")
				return
			}
		}
	}()
	gin.SetMode(gin.TestMode)
	t.Setenv("GORELAY_ADMIN_TOKEN", "baseline-only")
	router := gin.New()
	RegisterRoutes(router, s)
	request := func(method, path string, input any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(input)
		req := httptest.NewRequest(method, path, strings.NewReader(string(raw)))
		req.Header.Set("Authorization", "Bearer baseline-only")
		req.Header.Set("Content-Type", "application/json")
		out := httptest.NewRecorder()
		router.ServeHTTP(out, req)
		return out
	}
	spec := TaskSpec{Goal: "交付三条公开采购公告的完整正文，并保存可复用采集能力", ContractVersion: "v2", SchemaVersion: "v1", RequiredFields: []FieldRequirement{{Name: "title", Type: "string"}, {Name: "published_date", Type: "string"}, {Name: "body", Type: "string"}, {Name: "source_url", Type: "string"}}, JoinKeys: []string{"source_url"}, MinRecords: 3, RequireFieldSupport: true, SourceScope: []string{"fixture.example"}}
	if explorationCase {
		spec.SourceScope = append(spec.SourceScope, "8.8.8.8")
	}
	if actualRunnerCase {
		spec.SourceScope = []string{"8.8.8.8"}
	}
	if scenario != nil && scenario.CompleteText {
		spec.RequirePublishedCapability = true
		for i := range spec.RequiredFields {
			if spec.RequiredFields[i].Name == "body" {
				spec.RequiredFields[i].Type = "source_text"
			}
		}
	}
	if scenario != nil && scenario.SelectorDiscovery {
		spec.Goal += "；从此入口自主读取页面结构：http://8.8.8.8/releases "
	}
	prefix := "correct-chain"
	if requireSummary {
		prefix = "correct-summary-chain"
		spec.Goal += "；检索已采集知识并引用原文回答预算金额"
		spec.RequireSummary = true
		if scenario != nil && scenario.ExtractiveAnswer {
			spec.RequireExtractiveSummary = true
		}
		spec.Requirements = []TaskRequirement{{ID: "budget", Description: "从本次完整公告中检索并引用原文回答预算金额"}}
	}
	if queueAhead {
		prefix = "correct-queued-chain"
	}
	if modelFault != "" {
		prefix = "single-model-error-" + modelFault
	}
	if scenario != nil {
		prefix = "progressive-" + scenario.Name
	}
	type submittedRun struct {
		run agentdata.Run
		at  time.Time
	}
	submit := func(n int) submittedRun {
		started := time.Now()
		spec.Goal = strings.TrimSpace(spec.Goal)
		if scenario != nil && scenario.KnowledgeFirst && scenario.CompleteText {
			// First acquisition and expired refresh prove published capability
			// use. A fresh knowledge-only query explicitly requests no new work;
			// requiring a child execution there contradicts zero-acquisition reuse.
			spec.RequirePublishedCapability = n != 1
		}
		if scenario != nil && scenario.PartialKnowledge {
			spec.Goal = "交付三条公开采购公告的标题、日期与出处，并保存可复用采集能力"
			spec.RequiredFields = []FieldRequirement{{Name: "title", Type: "string"}, {Name: "published_date", Type: "string"}, {Name: "source_url", Type: "string"}}
			if n > 0 {
				spec.Goal = "复用已获取标题、日期与出处，仅补齐三条公告的完整正文"
				spec.RequiredFields = append(spec.RequiredFields, FieldRequirement{Name: "body", Type: "string"})
			}
		}
		if scenario != nil && scenario.KnowledgeFirst && n == 2 {
			spec.MaxAgeSeconds = 10
			time.Sleep(11 * time.Second)
		}
		out := request("POST", "/api/v1/agent/runs", CreateRunRequest{RequestKey: fmt.Sprintf("%s-%02d", prefix, n), Goal: spec.Goal, Spec: &spec, SourceScope: spec.SourceScope, RequireSummary: requireSummary})
		if out.Code != http.StatusAccepted {
			t.Fatal("normal entry", out.Code, out.Body.String())
		}
		var submitted agentdata.Run
		if err := json.Unmarshal(out.Body.Bytes(), &submitted); err != nil {
			t.Fatal(err)
		}
		return submittedRun{run: submitted, at: started}
	}
	queued := []submittedRun{}
	if queueAhead {
		for n := 0; n < count; n++ {
			queued = append(queued, submit(n))
		}
		t.Logf("submitted all %d tasks before observing; actual single-slot scheduler owns execution", count)
	}
	ledger := []any{}
	for n := 0; n < count; n++ {
		var submission submittedRun
		if queueAhead {
			submission = queued[n]
		} else {
			submission = submit(n)
		}
		started, submitted := submission.at, submission.run
		var out *httptest.ResponseRecorder
		lastStatus := ""
		lastReport := time.Time{}
		limit := time.Now().Add(35 * time.Second)
		if scenario != nil {
			limit = time.Now().Add(110 * time.Second)
		}
		var view struct {
			Run        agentdata.Run         `json:"run"`
			Result     TaskResult            `json:"result"`
			Operations []agentdata.Operation `json:"operations"`
			Accounting json.RawMessage       `json:"accounting"`
		}
		for {
			out = request("GET", "/api/v1/agent/runs/"+submitted.ID, nil)
			if out.Code != 200 {
				t.Fatal("observation", out.Code, out.Body.String())
			}
			if err := json.Unmarshal(out.Body.Bytes(), &view); err != nil {
				t.Fatal(err)
			}
			if view.Run.Status != lastStatus || time.Since(lastReport) > 5*time.Second {
				t.Logf("run %02d status=%s operations=%d records=%d", n, view.Run.Status, len(view.Operations), len(view.Result.Records))
				lastStatus = view.Run.Status
				lastReport = time.Now()
			}
			if view.Run.IsTerminal() {
				if view.Run.Status != expectedStatus {
					auditSave(t, fmt.Sprintf("%s-failed-%02d", prefix, n), json.RawMessage(out.Body.Bytes()))
					t.Fatal("correct model task failed", view.Run.Status, view.Run.LastError)
				}
				// The API exposes index_settled on accounting.
				var settled struct {
					Accounting struct {
						Settled bool `json:"index_settled"`
					} `json:"accounting"`
				}
				_ = json.Unmarshal(out.Body.Bytes(), &settled)
				if crawlerOnly || settled.Accounting.Settled || len(view.Result.Records) == 0 {
					break
				}
			}
			select {
			case err := <-errorsCh:
				t.Fatal(err)
			case <-ctx.Done():
				t.Fatal("full chain global watchdog", ctx.Err())
			case <-time.After(100 * time.Millisecond):
			}
			if time.Now().After(limit) {
				auditSave(t, fmt.Sprintf("%s-stalled-%02d", prefix, n), json.RawMessage(out.Body.Bytes()))
				t.Fatal("correct model chain made no timely delivery", view.Run.Status)
			}
		}
		expectedRecords := 3
		if expectedStatus == "failed" {
			expectedRecords = 0
		}
		if len(view.Result.Records) != expectedRecords {
			t.Fatal("wrong record count")
		}
		for _, row := range view.Result.Records {
			url, _ := row.Fields["source_url"].(string)
			want := gold[url]
			if want == nil {
				t.Fatal("unexpected URL", url)
			}
			for field, value := range want {
				if scenario != nil && scenario.PartialKnowledge && n == 0 && field == "body" {
					continue
				}
				actual, _ := row.Fields[field].(string)
				if normalizeEvidenceText(actual) != value {
					t.Fatalf("incorrect full delivery: run=%d field=%s", n, field)
				}
			}
		}
		pending, err := client.XPending(ctx, stream, group).Result()
		if err != nil || pending.Count != 0 {
			t.Fatal("unacknowledged task", pending, err)
		}
		trials := 0
		if scenario != nil && scenario.SelectorDiscovery && n == 0 {
			reads, inspections := 0, 0
			for _, op := range view.Operations {
				if op.Kind == "read_data" {
					reads++
				}
				if op.Kind == "inspect_source" {
					inspections++
				}
			}
			if reads < 4 || inspections < 2 {
				t.Fatal("selector discovery was bypassed", reads, inspections)
			}
		}
		for _, op := range view.Operations {
			if op.Kind == "validate_program" {
				trials++
			}
		}
		if scenario != nil && scenario.KnowledgeFirst {
			children, e := s.Data.Children(ctx, submitted.ID)
			if e != nil {
				t.Fatal(e)
			}
			if !scenario.PartialKnowledge && n == 1 && (len(children) != 0 || trials != 0) {
				auditSave(t, prefix+"-reuse-failed", json.RawMessage(out.Body.Bytes()))
				t.Fatal("fresh knowledge created acquisition or regenerated code")
			}
			if n == 2 && (len(children) != 1 || trials != 0) {
				t.Fatal("expired knowledge did not refresh using saved capability", len(children), trials)
			}
			if scenario.PartialKnowledge && n == 1 {
				if len(children) != 1 || trials != 1 {
					t.Fatal("gap supplementation did not create exactly one composition", len(children), trials)
				}
				rows, err := s.ChildResults(ctx, submitted.ID)
				if err != nil {
					t.Fatal(err)
				}
				for _, child := range rows {
					for _, row := range child.Records {
						if len(row.Fields) != 2 || row.Fields["body"] == nil || row.Fields["source_url"] == nil {
							t.Fatal("supplement re-acquired existing columns", row.Fields)
						}
					}
				}
				for _, op := range view.Operations {
					if op.Kind == "worker_attempt" {
						var observed struct {
							Runtime engine.ProgramResult `json:"runtime"`
						}
						_ = json.Unmarshal([]byte(op.ResultJSON), &observed)
						if observed.Runtime.Stats.Requests != 3 {
							t.Fatal("supplement fetched the old listing", observed.Runtime.Stats.Requests)
						}
					}
				}
			}
		}
		expectedTrials := 1
		if modelFault == "invalid_lua" {
			expectedTrials++
		}
		if scenario == nil && ((n == 0 && trials != expectedTrials) || (n > 0 && trials != 0)) {
			t.Fatal("capability generation/reuse mismatch", n, trials)
		}
		if modelFault != "" {
			rejected := 0
			for _, op := range view.Operations {
				if op.Key == "tool:injected-model-"+modelFault && op.Stage == "failed" {
					rejected++
				}
			}
			if rejected != 1 {
				t.Fatal("injected error was not rejected exactly once", modelFault, rejected)
			}
		}
		if requireSummary && expectedStatus == "completed" {
			answer := view.Result.Answer
			if scenario != nil && scenario.ExtractiveAnswer && (answer == nil || answer.SupportLevel != "verified_excerpt") {
				t.Fatal("factual summary lacks host-checked source support", answer)
			}
			if !view.Result.SummaryRequired || answer == nil || answer.Status != "answered" || !strings.Contains(answer.Answer, "75万元") || len(answer.Citations) == 0 || len(view.Result.Requirements) != 1 {
				auditSave(t, fmt.Sprintf("%s-summary-failed-%02d", prefix, n), json.RawMessage(out.Body.Bytes()))
				t.Fatal("task-level cited summary was not delivered")
			}
			if len(answer.Evidence.Entries) == 0 || !strings.Contains(answer.Evidence.Entries[0].Text, "75万元") || gold[answer.Evidence.Entries[0].URL] == nil {
				t.Fatal("summary did not carry matching source evidence")
			}
		}
		if scenario != nil && expectedStatus != "completed" {
			if len(view.Result.Gaps) == 0 || view.Result.Answer != nil {
				t.Fatal("termination did not deliver honest fallback", view.Result)
			}
			if strings.TrimSpace(view.Result.Summary) == "" {
				t.Fatal("host termination lost readable saved-result delivery")
			}
			maxCalls := int64(12)
			if explorationCase {
				maxCalls = 2 * explorationReviewWindow(LoopState{Spec: &spec, Budget: BudgetSnapshot{Limits: DefaultBudgetLimits()}})
			}
			if modelCalls.Load() > maxCalls {
				t.Fatal("wrong model was not stopped promptly", modelCalls.Load())
			}
			auditSave(t, prefix+"-run", map[string]any{"run": json.RawMessage(out.Body.Bytes()), "model_calls": modelCalls.Load(), "model_errors": modelErrors.Load(), "expected_status": expectedStatus, "pending": pending.Count, "elapsed_ms": time.Since(started).Milliseconds()})
			ledger = append(ledger, map[string]any{"ordinal": n, "run_id": submitted.ID, "status": view.Run.Status, "records": len(view.Result.Records), "model_calls": modelCalls.Load(), "model_errors": modelErrors.Load(), "pending": pending.Count, "elapsed_ms": time.Since(started).Milliseconds()})
			continue
		}
		if scenario != nil && scenario.AdaptiveCrawler {
			if scenario.Fault == "wrong_program_kind" || scenario.Fault == "missing_field_evidence" {
				compressed := false
				for _, op := range view.Operations {
					if op.Kind != "model" {
						continue
					}
					var response ChatResponse
					json.Unmarshal([]byte(op.ResultJSON), &response)
					if response.ContextProfile != nil && response.ContextProfile.Compaction != nil && response.ContextProfile.Compaction.Triggered {
						compressed = true
					}
				}
				if !compressed {
					t.Fatal("pressure fixture never crossed the actual compaction boundary")
				}
			}
			if scenario.Fault == "missing_field_evidence" && (!injectedFieldEvidence || restoredPrograms.Load() == 0 || trials < 2) {
				t.Fatal("field evidence recovery did not use actual fault, saved program and revalidation", injectedFieldEvidence, restoredPrograms.Load(), trials)
			}
			if scenario.Fault == "wrong_program_kind" && (!injectedProgramKind || !injectedAdmission || restoredPrograms.Load() == 0 || trials < 2) {
				t.Fatal("recovery did not cross actual fault, compression and validation boundaries", injectedProgramKind, injectedAdmission, restoredPrograms.Load(), trials)
			}
			if n > 0 && trials != 0 {
				t.Fatal("independent crawler run regenerated an existing published capability")
			}
			auditSave(t, fmt.Sprintf("%s-run-%02d", prefix, n), map[string]any{"real_model": false, "manual_child_execution": false, "rag_calls": 0, "run": json.RawMessage(out.Body.Bytes()), "pending": pending.Count, "exact_gold_match": true, "program_trials": trials})
			ledger = append(ledger, map[string]any{"ordinal": n, "run_id": submitted.ID, "status": view.Run.Status, "records": len(view.Result.Records), "program_trials": trials, "pending": pending.Count, "rag_calls": 0})
			continue
		}
		rag := request("POST", "/api/v1/knowledge/query", map[string]any{"question": "公开公告中的预算金额是多少？请引用原文证据。"})
		var answer struct {
			Result RAGAnswer `json:"result"`
		}
		_ = json.Unmarshal(rag.Body.Bytes(), &answer)
		if (scenario == nil || !scenario.PartialKnowledge) && (rag.Code != 200 || answer.Result.Status != "answered" || !strings.Contains(answer.Result.Answer, "75万元") || len(answer.Result.Citations) == 0) {
			auditSave(t, fmt.Sprintf("%s-rag-failed-%02d", prefix, n), json.RawMessage(rag.Body.Bytes()))
			t.Fatal("knowledge delivery", rag.Code, rag.Body.String())
		}
		auditSave(t, fmt.Sprintf("%s-run-%02d", prefix, n), map[string]any{"real_model": false, "model_errors_injected": modelFault != "", "model_fault": modelFault, "manual_child_execution": false, "run": json.RawMessage(out.Body.Bytes()), "rag": json.RawMessage(rag.Body.Bytes()), "pending": pending.Count, "exact_gold_match": true, "elapsed_ms": time.Since(started).Milliseconds()})
		if scenario != nil && scenario.LongReads && modelCalls.Load() <= 12 {
			t.Fatal("long productive path did not exceed former total cap")
		}
		ledger = append(ledger, map[string]any{"ordinal": n, "run_id": submitted.ID, "status": view.Run.Status, "records": len(view.Result.Records), "program_trials": trials, "rag_status": answer.Result.Status, "pending": pending.Count, "elapsed_ms": time.Since(started).Milliseconds(), "model_calls": modelCalls.Load(), "model_errors": modelErrors.Load()})
		t.Logf("run %02d delivered full records=3 RAG=answered pending=0 new_program_trials=%d", n, trials)
	}
	var outboxes int64
	if err := db.Model(&task.TaskOutbox{}).Where("published_at IS NULL").Count(&outboxes).Error; err != nil {
		t.Fatal(err)
	}
	if outboxes != 0 {
		t.Fatal("unpublished outbox rows remain", outboxes)
	}
	auditSave(t, prefix+"-ledger", map[string]any{"scenario": "frozen official HTML; real SQL Redis Qdrant; actual Lua VM; HTTP model adapter", "model_fault": modelFault, "injection_case": scenario, "requires_task_summary": requireSummary, "queued_before_observation": queueAhead, "runs": ledger, "runner_calls": runnerCalls.Load(), "unpublished_outboxes": outboxes, "models_are_stateless": true, "error_injection_started": modelFault != "" || scenario != nil && scenario.WrongPercent > 0})
}
