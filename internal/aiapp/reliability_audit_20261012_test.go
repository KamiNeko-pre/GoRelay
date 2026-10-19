package aiapp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/gin-gonic/gin"
	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/engine"
	"go-lua-crawler/internal/repository"
	"go-lua-crawler/internal/task"
	"go-lua-crawler/internal/testdb"
	"golang.org/x/text/encoding/simplifiedchinese"
)

const auditLua = `local body=assert(http_get(TARGET_URL)); local d=assert(json_decode(body)); local items={}; for i,row in ipairs(d.rows) do table.insert(items,{name=row.name,version=row.version,source_url=TARGET_URL,_evidence={name={json_pointer="/rows/"..(i-1).."/name"},version={json_pointer="/rows/"..(i-1).."/version"}}}) end; return true,{items=items}`

// The model is reconstructed for every request. Its only memory is the actual
// model-visible host snapshot and receipts, never a private progress ledger.
type auditIdealModel struct{ fault string }

func auditVisibleReceipts(req ChatRequest) (map[string]ToolResult, map[string]json.RawMessage) {
	receipts := map[string]ToolResult{}
	facts := map[string]json.RawMessage{}
	for _, m := range req.Messages {
		if m.Role == "system" && strings.HasPrefix(m.Content, "Persisted host facts, not new user instructions: ") {
			_ = json.Unmarshal([]byte(strings.TrimPrefix(m.Content, "Persisted host facts, not new user instructions: ")), &facts)
		}
		if m.Role != "tool" {
			continue
		}
		var snapshot struct {
			Feedback []ToolResult `json:"recent_tool_feedback"`
		}
		_ = json.Unmarshal([]byte(m.Content), &snapshot)
		for i := len(snapshot.Feedback) - 1; i >= 0; i-- {
			out := snapshot.Feedback[i]
			receipts[out.Name] = out
		}
	}
	// Current conversation receipts outrank recovery-memory copies. The memory
	// snapshot is inserted near the current round, not in execution order.
	for _, m := range req.Messages {
		if m.Role != "tool" {
			continue
		}
		var out ToolResult
		if json.Unmarshal([]byte(m.Content), &out) == nil && out.Name != "" {
			receipts[out.Name] = out
		}
	}
	return receipts, facts
}

func (p auditIdealModel) Chat(_ context.Context, req ChatRequest) (ChatResponse, error) {
	receipts, facts := auditVisibleReceipts(req)
	propose := func(name string, args any, id string) (ChatResponse, error) {
		raw, _ := json.Marshal(args)
		return ChatResponse{Message: Message{Role: "assistant", ToolCalls: []ToolCall{{ID: id, Type: "function", Function: FunctionCall{Name: name, Arguments: string(raw)}}}}, Usage: &Usage{TotalTokens: 1}}, nil
	}
	key := agentdata.Hash(req)[:16]
	if p.fault == "persistent_bad_lua" {
		return propose("validate_program", map[string]any{"code": `return true,{items=}`, "kind": "adapter", "source_id": "audit-source", "purpose": "always bad Lua", "url": "https://fixture.example/releases"}, key)
	}
	seenFault := false
	for _, out := range receipts {
		if strings.HasPrefix(out.CallID, "audit-injected-") {
			seenFault = true
		}
	}
	if p.fault != "" && !seenFault && len(receipts) == 0 {
		switch p.fault {
		case "wrong_task":
			return propose("plan_task", map[string]any{"required_fields": []map[string]string{{"name": "name", "type": "string"}}}, "audit-injected-plan")
		case "premature_finish":
			return propose("finish_task", map[string]any{"status": "completed", "reason": "model claims finished"}, "audit-injected-finish")
		case "malformed_arguments":
			return ChatResponse{Message: Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "audit-injected-format", Type: "function", Function: FunctionCall{Name: "validate_program", Arguments: `{broken`}}}}, Usage: &Usage{TotalTokens: 1}}, nil
		case "invalid_lua", "hallucination":
			code := `return true,{items=}`
			if p.fault == "hallucination" {
				code = strings.ReplaceAll(auditLua, "version=row.version", "version=\"invented\"")
			}
			return propose("validate_program", map[string]any{"code": code, "kind": "adapter", "source_id": "audit-source", "purpose": "audit source facts", "url": "https://fixture.example/releases"}, "audit-injected-program")
		}
	}
	var children []map[string]any
	_ = json.Unmarshal(facts["children"], &children)
	if len(children) > 0 {
		var coverage CoverageReport
		_ = json.Unmarshal(facts["coverage"], &coverage)
		if coverage.Satisfied {
			return propose("finish_task", map[string]any{"status": "completed", "reason": "host coverage satisfied", "requirements": []any{}}, key)
		}
		return propose("finish_task", map[string]any{"status": "partial", "reason": "only available supported records", "requirements": []any{}}, key)
	}
	if out := receipts["register_source"]; out.Name != "" && out.Error == "" {
		response, err := propose("submit_acquisition", map[string]any{"source_id": "audit-source", "url": "https://fixture.example/releases"}, key)
		if p.fault == "repeat_same_id" || p.fault == "repeat_new_id" {
			second := response.Message.ToolCalls[0]
			if p.fault == "repeat_new_id" {
				second.ID += "-retry"
			}
			response.Message.ToolCalls = append(response.Message.ToolCalls, second)
		}
		return response, err
	}
	if out := receipts["validate_program"]; out.Name != "" && out.Error == "" {
		var candidate Candidate
		if err := json.Unmarshal(out.Data, &candidate); err != nil {
			return ChatResponse{}, err
		}
		return propose("register_source", map[string]any{"candidate_id": candidate.ID, "description": "validated audit adapter"}, key)
	}
	if out := receipts["list_sources"]; out.Name != "" && out.Error == "" {
		var catalog struct {
			Active []struct {
				Source agentdata.SourceVersion `json:"source"`
			} `json:"active"`
		}
		_ = json.Unmarshal(out.Data, &catalog)
		for _, item := range catalog.Active {
			if item.Source.SourceID == "audit-source" {
				return propose("submit_acquisition", map[string]any{"source_id": "audit-source", "url": "https://fixture.example/releases"}, key)
			}
		}
		return propose("validate_program", map[string]any{"code": auditLua, "kind": "adapter", "source_id": "audit-source", "purpose": "audit source facts", "url": "https://fixture.example/releases"}, key)
	}
	return propose("list_sources", map[string]any{}, key)
}

func auditSave(t *testing.T, name string, value any) {
	t.Helper()
	if dir := os.Getenv("GORELAY_TEST_EVIDENCE_DIR"); dir != "" {
		raw, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(dir, name+".json"), append(raw, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAuditIdealModelAndRecoverableErrors(t *testing.T) {
	for _, mode := range []string{"ideal", "wrong_task", "premature_finish", "malformed_arguments", "invalid_lua", "hallucination", "partial_execution", "repeat_same_id", "repeat_new_id", "persistent_bad_lua"} {
		t.Run(mode, func(t *testing.T) {
			db := testdb.Open(t)
			s := NewService(db)
			if err := s.Migrate(); err != nil {
				t.Fatal(err)
			}
			oldDB := repository.DB
			repository.DB = db
			t.Cleanup(func() { repository.DB = oldDB })
			s.Fingerprint = "audit-fixture"
			runnerCalls := 0
			runner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var in RunnerRequest
				if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				// Timeout is runner-owned, deliberately absent from the wire.
				in.Limits.Timeout = engine.DefaultProgramLimits().Timeout
				runnerCalls++
				body := `{"rows":[{"name":"Alpha","version":"1.0"},{"name":"Beta","version":"2.0"}]}`
				if mode == "partial_execution" && runnerCalls > 1 {
					body = `{"rows":[{"name":"Alpha","version":"1.0"}]}`
				}
				replay := acquisition.NewReplay([]acquisition.ReplayEntry{{Method: "GET", URL: in.Program.TargetURL, Status: 200, Body: body, FetchedAt: time.Now()}})
				result, err := engine.RunProgram(r.Context(), in.Program, in.Limits, replay.Fetch)
				response := RunnerResponse{Result: &result}
				if err != nil {
					response.Error = &RunnerError{Code: "program_failed", Message: err.Error()}
				}
				_ = json.NewEncoder(w).Encode(response)
			}))
			defer runner.Close()
			s.Runner, _ = NewRunnerClient(runner.URL, "audit-fixture")
			fault := mode
			if mode == "ideal" || mode == "partial_execution" {
				fault = ""
			}
			s.Model = auditIdealModel{fault: fault}
			gin.SetMode(gin.TestMode)
			t.Setenv("GORELAY_ADMIN_TOKEN", "audit-only")
			router := gin.New()
			RegisterRoutes(router, s)
			spec := TaskSpec{Goal: "collect two complete source facts", ContractVersion: "v2", SchemaVersion: "v1", RequiredFields: []FieldRequirement{{Name: "name", Type: "string"}, {Name: "version", Type: "string"}}, JoinKeys: []string{"name"}, MinRecords: 2, RequireFieldSupport: true, SourceScope: []string{"fixture.example"}}
			request := CreateRunRequest{RequestKey: "audit-" + mode, Goal: spec.Goal, Spec: &spec, SourceScope: spec.SourceScope}
			raw, _ := json.Marshal(request)
			req := httptest.NewRequest("POST", "/api/v1/agent/runs", strings.NewReader(string(raw)))
			req.Header.Set("Authorization", "Bearer audit-only")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, req)
			if response.Code != http.StatusAccepted && response.Code != http.StatusCreated && response.Code != http.StatusOK {
				t.Fatal(response.Code, response.Body.String())
			}
			var run agentdata.Run
			if err := json.Unmarshal(response.Body.Bytes(), &run); err != nil {
				t.Fatal(err)
			}
			if run.ID == "" {
				var envelope struct {
					Run agentdata.Run `json:"run"`
				}
				_ = json.Unmarshal(response.Body.Bytes(), &envelope)
				run = envelope.Run
			}
			if run.ID == "" {
				t.Fatal("normal entry did not return Run", response.Body.String())
			}
			ctx := context.Background()
			for step := 0; step < 45; step++ {
				stored, _ := s.Data.Run(ctx, run.ID)
				if stored.IsTerminal() {
					break
				}
				if err := s.Advance(ctx, run.ID); err != nil {
					failed, _ := s.Data.Run(ctx, run.ID)
					ops, _ := s.Data.Operations(ctx, run.ID)
					traces, _ := s.Data.Traces(ctx, run.ID)
					links, _ := s.Data.Children(ctx, run.ID)
					auditSave(t, "ideal-"+mode, map[string]any{"live_model": false, "run": failed, "operations": ops, "loop": BuildLoopReport(traces), "child_count": len(links), "advance_error": err.Error()})
					t.Fatal("advance", step, err)
				}
				rows, err := s.ChildResults(ctx, run.ID)
				if err != nil {
					t.Fatal(err)
				}
				for _, row := range rows {
					if row.Task.Status == task.StatusQueued {
						child := row.Task
						token, err := repository.ClaimCrawlTask(ctx, child, time.Minute, false)
						if err != nil {
							t.Fatal(err)
						}
						db.First(&child, child.ID)
						persist, err := s.ExecuteTask(ctx, child)
						if err != nil {
							t.Fatal(err)
						}
						if err = repository.CompleteCrawlTask(ctx, child.ID, task.StatusSucceeded, "", "", time.Now(), token, persist); err != nil {
							t.Fatal(err)
						}
					}
				}
				// Restart the host and the model at every committed boundary.
				s = NewService(db)
				s.Fingerprint = "audit-fixture"
				s.Model = auditIdealModel{fault: fault}
				s.Runner, _ = NewRunnerClient(runner.URL, "audit-fixture")
			}
			stored, _ := s.Data.Run(ctx, run.ID)
			result, err := s.Evaluate(ctx, stored)
			if err != nil {
				t.Fatal(err)
			}
			want := "completed"
			records := 2
			if mode == "partial_execution" {
				want = "partial"
				records = 1
			}
			if mode == "persistent_bad_lua" {
				want = "failed"
				records = 0
			}
			ops, _ := s.Data.Operations(ctx, run.ID)
			traces, _ := s.Data.Traces(ctx, run.ID)
			auditSave(t, "ideal-"+mode, map[string]any{"live_model": false, "source_transport": "frozen replay", "host_restart_each_step": true, "mock_memory": "request only", "run": stored, "result": result, "operations": ops, "loop": BuildLoopReport(traces), "runner_calls": runnerCalls})
			if stored.Status != want || len(result.Records) != records {
				t.Fatalf("mode %s got %s records=%d wanted %s/%d", mode, stored.Status, len(result.Records), want, records)
			}
			for _, row := range result.Records {
				if row.Fields["version"] != "1.0" && row.Fields["version"] != "2.0" {
					t.Fatal("incorrect fact persisted", row)
				}
			}
			links, _ := s.Data.Children(ctx, run.ID)
			t.Logf("submitted_tasks=%d distinct_records=%d", len(links), len(result.Records))
			t.Logf("status=%s records=%d operations=%d runner=%d; model and host restarted at each boundary", stored.Status, len(result.Records), len(ops), runnerCalls)
			if mode == "ideal" {
				request.RequestKey += "-independent-reuse"
				raw, _ := json.Marshal(request)
				req := httptest.NewRequest("POST", "/api/v1/agent/runs", strings.NewReader(string(raw)))
				req.Header.Set("Authorization", "Bearer audit-only")
				out := httptest.NewRecorder()
				router.ServeHTTP(out, req)
				if out.Code != http.StatusAccepted {
					t.Fatal("reuse entry", out.Code, out.Body.String())
				}
				var reuse agentdata.Run
				_ = json.Unmarshal(out.Body.Bytes(), &reuse)
				for step := 0; step < 20; step++ {
					stored, _ := s.Data.Run(ctx, reuse.ID)
					if stored.IsTerminal() {
						break
					}
					if err := s.Advance(ctx, reuse.ID); err != nil {
						t.Fatal("reuse advance", err)
					}
					rows, _ := s.ChildResults(ctx, reuse.ID)
					for _, row := range rows {
						if row.Task.Status == task.StatusQueued {
							child := row.Task
							token, err := repository.ClaimCrawlTask(ctx, child, time.Minute, false)
							if err != nil {
								t.Fatal(err)
							}
							db.First(&child, child.ID)
							persist, err := s.ExecuteTask(ctx, child)
							if err != nil {
								t.Fatal(err)
							}
							if err = repository.CompleteCrawlTask(ctx, child.ID, task.StatusSucceeded, "", "", time.Now(), token, persist); err != nil {
								t.Fatal(err)
							}
						}
					}
					s = NewService(db)
					s.Fingerprint = "audit-fixture"
					s.Model = auditIdealModel{}
					s.Runner, _ = NewRunnerClient(runner.URL, "audit-fixture")
				}
				finished, _ := s.Data.Run(ctx, reuse.ID)
				result, _ := s.Evaluate(ctx, finished)
				ops, _ := s.Data.Operations(ctx, reuse.ID)
				trials := 0
				for _, op := range ops {
					if op.Kind == "validate_program" {
						trials++
					}
				}
				auditSave(t, "independent-reuse", map[string]any{"live_model": false, "run": finished, "result": result, "operations": ops, "new_program_trials": trials})
				if finished.Status != "completed" || len(result.Records) != 2 || trials != 0 {
					t.Fatalf("independent reuse: status=%s records=%d new_trials=%d", finished.Status, len(result.Records), trials)
				}
				t.Log("independent Run found the catalog capability, reused it, and completed without regenerating Lua")
			}
		})
	}
}

func TestAuditSavedNoticeFullBodyVersusMockPrefix(t *testing.T) {
	file := filepath.Join("..", "..", "output", "experiments", "mvp-procurement", "gold", "A0-detail-1.html")
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Skip("saved notice unavailable", err)
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	article := doc.Find("div.v_news_content").First()
	full := normalizeEvidenceText(article.Text())
	first := normalizeEvidenceText(article.Find("p").First().Text())
	data := map[string]any{"source": "saved official page, no new HTTP", "first_paragraph_runes": len([]rune(first)), "full_body_runes": len([]rune(full)), "first_paragraph_contains_budget": strings.Contains(first, "75万元"), "full_body_contains_budget": strings.Contains(full, "75万元")}
	auditSave(t, "saved-notice-completeness", data)
	t.Logf("%+v", data)
	if full == "" || full == first {
		t.Fatal("saved fixture did not reproduce incomplete body")
	}
}

func TestAuditGBKCorruptionPrecedesPaging(t *testing.T) {
	text := "预算五十三万元，响应截止日期十月二十日。"
	for _, long := range []bool{false, true} {
		content := text
		if long {
			content = strings.Repeat(text, 160)
		}
		html := `<html><meta charset="gb2312"><main><p>` + content + `</p></main></html>`
		encoded, err := simplifiedchinese.GBK.NewEncoder().Bytes([]byte(html))
		if err != nil {
			t.Fatal(err)
		}
		info, err := buildHTMLInformation(string(encoded), "https://fixture.example/gbk", "main")
		if err != nil {
			t.Fatal(err)
		}
		good, err := buildHTMLInformation(html, "https://fixture.example/utf8", "main")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(good.Text(), text) {
			t.Fatal("UTF-8 counterexample failed")
		}
		t.Logf("long=%v contains_original=%v first_view=%q", long, strings.Contains(info.Text(), text), string([]rune(info.Text())[:min(90, len([]rune(info.Text())))]))
		if strings.Contains(info.Text(), text) {
			t.Fatal("GBK hypothesis changed; re-audit source decoding")
		}
	}
}

func TestAuditSupportedPrefixIsNotCompleteBody(t *testing.T) {
	url := "https://fixture.example/notice"
	body := `<article><h1>Notice</h1><p>Opening paragraph.</p><p>Budget: 530000. Deadline: 2026-10-20.</p></article>`
	receipt := acquisition.Receipt{Method: "GET", URL: url, Status: 200, FetchedAt: time.Now()}
	for _, selector := range []string{"article p", "article"} {
		value := "Opening paragraph."
		if selector == "article" {
			value = "NoticeOpening paragraph.Budget: 530000. Deadline: 2026-10-20."
		}
		rows, err := BindRecords(engine.ProgramResult{Collection: map[string]any{"items": []any{map[string]any{"body": value, "source_url": url, "_evidence": map[string]any{"body": map[string]any{"selector": selector, "index": 0}}}}}, Requests: []acquisition.Receipt{receipt}, SourceEvidence: []acquisition.Response{{Receipt: receipt, Body: body}}}, RecordBinding{SchemaVersion: "v1", SourceID: "audit", DataVersion: "v", TaskID: 1})
		if err != nil {
			t.Fatal(err)
		}
		spec := TaskSpec{Goal: "deliver complete announcement body including budget and deadline", ContractVersion: "v2", SchemaVersion: "v1", RequiredFields: []FieldRequirement{{Name: "body", Type: "string"}}, RequireFieldSupport: true}
		coverage, err := EvaluateRecords(spec, rows, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("selector=%s field_supported=%v coverage=%v includes_deadline=%v", selector, fieldIsSupported(rows[0].Provenance, "body"), coverage.Satisfied, strings.Contains(value, "Deadline"))
		if !coverage.Satisfied {
			t.Fatal("reproduction changed; re-audit completeness", coverage)
		}
	}
}

func TestAuditProviderFormatAndTimeoutClassification(t *testing.T) {
	for _, mode := range []string{"broken_json", "empty_choice", "wrong_role", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "broken_json":
					fmt.Fprint(w, `{"choices":`)
				case "empty_choice":
					fmt.Fprint(w, `{"choices":[],"usage":{"total_tokens":10}}`)
				case "wrong_role":
					fmt.Fprint(w, `{"choices":[{"message":{"role":"tool","content":"not an assistant decision"},"finish_reason":"stop"}],"usage":{"total_tokens":10}}`)
				case "timeout":
					select {
					case <-r.Context().Done():
					case <-time.After(time.Second):
					}
				}
			}))
			defer remote.Close()
			provider, err := NewModelProvider(ProviderConfig{BaseURL: remote.URL, APIKey: "audit", Model: "audit", Timeout: 25 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			out, err := provider.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "public task"}}})
			failure := captureFailure(err, 0, true)
			t.Logf("mode=%s err=%v failure=%+v accepted_role=%q reported_usage=%v", mode, err, failure, out.Message.Role, out.Usage)
			auditSave(t, "provider-"+mode, map[string]any{"response": out, "failure": failure, "error": fmt.Sprint(err)})
		})
	}
}

type auditSchedulingModel struct {
	mu    sync.Mutex
	calls map[string]int
}

func (m *auditSchedulingModel) Chat(_ context.Context, request ChatRequest) (ChatResponse, error) {
	goal := ""
	for _, message := range request.Messages {
		if message.Role == "user" {
			var input struct {
				Goal string `json:"goal"`
			}
			_ = json.Unmarshal([]byte(message.Content), &input)
			goal = input.Goal
		}
	}
	m.mu.Lock()
	m.calls[goal]++
	m.mu.Unlock()
	return ChatResponse{Message: Message{Role: "assistant", Content: "no executable proposal"}, Usage: &Usage{TotalTokens: 1}}, nil
}

func TestAuditSingleSlotSchedulingFairness(t *testing.T) {
	t.Setenv("GORELAY_AGENT_CONCURRENCY", "1")
	t.Setenv("GORELAY_EXPERIMENT_UNLIMITED_BUDGET", "1")
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	m := &auditSchedulingModel{calls: map[string]int{}}
	s.Model = m
	ctx, cancel := context.WithTimeout(context.Background(), 3400*time.Millisecond)
	defer cancel()
	for _, goal := range []string{"old-unproductive-run", "new-independent-run"} {
		if _, err := s.Create(ctx, CreateRunRequest{RequestKey: goal, Goal: goal}); err != nil {
			t.Fatal(err)
		}
	}
	_ = s.Run(ctx)
	m.mu.Lock()
	defer m.mu.Unlock()
	auditSave(t, "single-slot-fairness", m.calls)
	t.Logf("single-slot counts: %+v", m.calls)
	if m.calls["old-unproductive-run"] < 2 {
		t.Fatal("scheduler reproduction did not run")
	}
	if m.calls["new-independent-run"] == 0 {
		t.Errorf("oldest repeatedly selected; independent Run starved while each call completed normally")
	}
}
