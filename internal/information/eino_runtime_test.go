package information

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/engine"
	"go-lua-crawler/internal/programrunner"
)

type informationScriptModel struct {
	calls  int
	script func(aiapp.ChatRequest, int) (aiapp.Message, error)
}

func (m *informationScriptModel) Chat(_ context.Context, r aiapp.ChatRequest) (aiapp.ChatResponse, error) {
	m.calls++
	msg, err := m.script(r, m.calls)
	return aiapp.ChatResponse{Message: msg, FinishReason: "tool_calls", Usage: &aiapp.Usage{PromptTokens: 11, CompletionTokens: 3, TotalTokens: 14}}, err
}
func informationCall(id, name string, args any) aiapp.Message {
	return aiapp.Message{Role: "assistant", ToolCalls: []aiapp.ToolCall{{ID: id, Type: "function", Function: aiapp.FunctionCall{Name: name, Arguments: string(mustMarshal(args))}}}}
}

func TestInlineAcquisitionRemovesOneEvidenceRound(t *testing.T) {
	const target = "http://8.8.8.8/timetable"
	const phrase = "Saturday departure"
	for _, route := range []string{"inspection", "http", "api", "lua"} {
		for _, inline := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/inline=%t", route, inline), func(t *testing.T) {
				var fetches atomic.Int32
				publicSource(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					fetches.Add(1)
					if route == "api" {
						w.Header().Set("Content-Type", "application/json")
						fmt.Fprint(w, `{"departures":[{"schedule":"Saturday departure is at 09:15."}]}`)
						return
					}
					w.Header().Set("Content-Type", "text/html")
					fmt.Fprint(w, "<html><title>Timetable</title><p>Saturday departure is at 09:15.</p></html>")
				}))
				s, _ := retrievalService(t)
				s.LuaRunner = func(ctx context.Context, req programrunner.Request) (engine.ProgramResult, error) {
					result, err := programrunner.Run(ctx, req)
					if result.Result == nil {
						return engine.ProgramResult{}, err
					}
					return *result.Result, err
				}
				ctx := context.Background()
				r, err := s.Create(ctx, Input{RequestKey: "schedule-evidence", Goal: "Find the Saturday departure and quote its source", SourceScope: []string{"8.8.8.8"}, EntryURLs: []string{target}})
				if err != nil {
					t.Fatal(err)
				}
				queries := []string{phrase}
				model := &informationScriptModel{script: func(req aiapp.ChatRequest, n int) (aiapp.Message, error) {
					if n == 1 {
						tool := "acquire_sources"
						args := map[string]any{}
						switch route {
						case "inspection":
							args["inspect_url"] = target
						case "http":
							args["url"] = target
						case "api":
							args["api"] = APIPlan{URL: target, ItemsPointer: "/departures", Fields: map[string]string{"schedule": "/schedule"}}
						case "lua":
							tool = "run_lua"
							args["url"] = target
							args["code"] = `local r = http_request({url=TARGET_URL}); return true, {items={{schedule=html_find(r.body,"p")}}}`
						}
						if inline {
							args["excerpt_queries"] = queries
						}
						proposal := informationCall("collect", tool, args)
						plan := informationCall("plan", "update_plan", WorkPlan{Interpretation: "Find the Saturday departure", CoverageRationale: "Read the official timetable"})
						proposal.ToolCalls = append(plan.ToolCalls, proposal.ToolCalls...)
						return proposal, nil
					}
					if !inline && n == 2 {
						return informationCall("read", "read_materials", MaterialReadQuery{Kind: "excerpts", Queries: queries}), nil
					}
					for _, message := range req.Messages {
						if message.Role != "tool" {
							continue
						}
						var receipt struct {
							Data struct {
								Matches  []excerptMatchEntry `json:"matches"`
								Evidence struct {
									Matches []excerptMatchEntry `json:"matches"`
								} `json:"evidence"`
							} `json:"data"`
						}
						if err := json.Unmarshal([]byte(message.Content), &receipt); err != nil {
							return aiapp.Message{}, err
						}
						for _, match := range append(receipt.Data.Matches, receipt.Data.Evidence.Matches...) {
							if match.EvidenceID != "" && match.URL == target && strings.Contains(match.Excerpt, "Saturday departure is at 09:15") {
								refs := []StoredReference{{DocID: match.DocID, Revision: match.Revision}}
								return informationCall("submit", "submit_delivery", DeliveryDraft{Outcome: "completed", Report: "Saturday departure is at 09:15.", References: refs}), nil
							}
						}
					}
					return aiapp.Message{}, fmt.Errorf("saved text did not reach the model after %d rounds", n)
				}}
				s.Model = model
				start := time.Now()
				if err := s.Advance(ctx, r.ID); err != nil {
					t.Fatal(err)
				}
				elapsed := time.Since(start)
				got, err := s.Request(ctx, r.ID)
				if err != nil {
					t.Fatal(err)
				}
				wantRounds := 3
				if inline {
					wantRounds = 2
				}
				if got.Status != "completed" || model.calls != wantRounds || fetches.Load() != 1 {
					t.Fatalf("status=%s, model calls=%d, fetches=%d", got.Status, model.calls, fetches.Load())
				}
				_, draft, err := s.SavedDelivery(ctx, got.CurrentDeliveryID)
				if err != nil || draft.Report != "Saturday departure is at 09:15." || len(draft.References) != 1 {
					t.Fatalf("saved evidence-backed delivery changed: %+v, %v", draft, err)
				}
				t.Logf("completed in %d model calls and %d fetch, local wall time %s", model.calls, fetches.Load(), elapsed)
			})
		}
	}
}

func TestNativeAPIReturnsSubmissionReadyEvidenceInNextRound(t *testing.T) {
	const target = "http://8.8.8.8/timetable.json"
	var fetches atomic.Int32
	publicSource(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "2" {
			fmt.Fprint(w, `{"departures":[{"seats":"17"}],"next":null}`)
			return
		}
		fmt.Fprint(w, `{"departures":[{"seats":24}],"next":"?page=2"}`)
	}))
	s, _ := retrievalService(t)
	ctx := context.Background()
	r, err := s.Create(ctx, Input{RequestKey: "timetable-capacity", Goal: "Calculate the combined seat capacity from the official timetable and cite the source", SourceScope: []string{"8.8.8.8"}, EntryURLs: []string{target}})
	if err != nil {
		t.Fatal(err)
	}
	model := &informationScriptModel{script: func(req aiapp.ChatRequest, n int) (aiapp.Message, error) {
		if n == 1 {
			plan := informationCall("plan", "update_plan", WorkPlan{Interpretation: "Calculate the combined seat capacity", CoverageRationale: "Follow the official timetable pagination"})
			fetch := informationCall("collect", "acquire_sources", informationAcquireArgs{API: &APIPlan{URL: target, ItemsPointer: "/departures", NextPointer: "/next", Fields: map[string]string{"seats": "/seats"}}})
			plan.ToolCalls = append(plan.ToolCalls, fetch.ToolCalls...)
			return plan, nil
		}
		if n != 2 {
			return aiapp.Message{}, fmt.Errorf("extra model round %d", n)
		}
		for _, m := range req.Messages {
			if m.Role != "tool" {
				continue
			}
			var receipt struct {
				Data struct {
					Evidence []calculationSampleEvidence `json:"sample_evidence"`
				} `json:"data"`
			}
			if err := json.Unmarshal([]byte(m.Content), &receipt); err != nil {
				return aiapp.Message{}, err
			}
			if len(receipt.Data.Evidence) != 2 {
				continue
			}
			first, second := receipt.Data.Evidence[0], receipt.Data.Evidence[1]
			a, b := first.Fields["seats"], second.Fields["seats"]
			if first.RowIndex != 0 || second.RowIndex != 1 || a.JSONPointer != "/departures/0/seats" || b.JSONPointer != a.JSONPointer || a.SourceRef.DocID == b.SourceRef.DocID {
				return aiapp.Message{}, fmt.Errorf("wrong pagination evidence: %+v", receipt.Data.Evidence)
			}
			return informationCall("submit", "submit_delivery", map[string]any{
				"outcome": "completed", "report": "The timetable has {{total}} seats in total.",
				"calculations":       []DeliveryCalculation{{ID: "total", Formula: "a+b"}},
				"calculation_inputs": DeliveryCalculationInputs{EvidenceInputs: map[string]EvidenceCalculationInput{"a": {SourceValue: &a}, "b": {SourceValue: &b}}},
			}), nil
		}
		return aiapp.Message{}, fmt.Errorf("the acquisition tool did not return source-bound numbers")
	}}
	audit := &informationScriptModel{script: func(req aiapp.ChatRequest, _ int) (aiapp.Message, error) {
		var packet deliveryAuditInput
		if err := json.Unmarshal([]byte(req.Messages[1].Content), &packet); err != nil {
			return aiapp.Message{}, err
		}
		if packet.SharedInputs["a"] != 24 || packet.SharedInputs["b"] != 17 || packet.Values["total"] != 41 {
			return aiapp.Message{}, fmt.Errorf("audit received wrong calculation evidence: %+v", packet.SharedInputs)
		}
		return aiapp.Message{Content: `{"verdict":"pass"}`}, nil
	}}
	s.Model, s.DeliveryAuditModel = model, audit
	if err := s.Advance(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.Request(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "completed" || model.calls != 2 || audit.calls != 1 || fetches.Load() != 2 {
		t.Fatalf("status=%s model rounds=%d audit calls=%d fetches=%d", got.Status, model.calls, audit.calls, fetches.Load())
	}
	_, draft, err := s.SavedDelivery(ctx, got.CurrentDeliveryID)
	if err != nil || draft.Report != "The timetable has 41 seats in total." || len(draft.References) != 2 {
		t.Fatalf("unexpected saved delivery: %+v, %v", draft, err)
	}
	t.Logf("%d Agent model rounds, %d independent audit call, %d source fetches; both original values used without a read_materials call", model.calls, audit.calls, fetches.Load())
}

func TestStateMachineToolScoping(t *testing.T) {
	if total := len(informationToolDefinitions()); total != 9 {
		t.Fatalf("expected 9 tools in informationToolDefinitions (including run_lua), got %d", total)
	}
	toSet := func(defs []aiapp.ToolDefinition) map[string]bool {
		m := make(map[string]bool, len(defs))
		for _, d := range defs {
			m[d.Function.Name] = true
		}
		return m
	}

	h := &informationHost{
		request: Request{ID: "test-req"},
		input:   Input{Goal: "Check Go release", EntryURLs: []string{"https://go.dev/dl/"}},
	}
	factsInitial := map[string]any{
		"saved_facts": map[string]any{"observations": int64(0), "documents": int64(0)},
	}

	// 1. Default surface exposes consistent 5-stage tools (with run_lua & acquire_sources, without browser_extract until unlocked).
	tools1, directive1 := h.stateMachineTools(factsInitial)
	names1 := toSet(tools1)
	for _, want := range []string{"update_plan", "discover_sources", "acquire_sources", "run_lua", "read_materials", "submit_delivery", "revise_requirement_interpretation", "ask_user"} {
		if !names1[want] {
			t.Fatalf("missing expected 5-stage tool %s: %+v", want, names1)
		}
	}
	if names1["browser_extract"] {
		t.Fatalf("browser_extract should stay omitted until an anti-bot/JS grant: %+v", names1)
	}
	if directive1 == "" {
		t.Fatal("expected non-empty plan & acquire directive")
	}

	// A plan does not grant Browser access.
	h.request.PlanJSON = `{"interpretation":"render dynamic DOM","route":"browser"}`
	toolsBrowser, _ := h.stateMachineTools(factsInitial)
	namesBrowser := toSet(toolsBrowser)
	if namesBrowser["browser_extract"] || !namesBrowser["run_lua"] || !namesBrowser["acquire_sources"] {
		t.Fatalf("browser route tools mismatch: %+v", namesBrowser)
	}

	// Source-discovery tasks can use the same tool for search and the saved directory.
	hDisc := &informationHost{
		request: Request{ID: "test-subtask", Purpose: "source_discovery"},
		input:   Input{Goal: "Find official Go release URLs"},
	}
	toolsDisc, _ := hDisc.stateMachineTools(factsInitial)
	namesDisc := toSet(toolsDisc)
	if !namesDisc["discover_sources"] {
		t.Fatalf("source_discovery subtask cannot read the saved directory: %+v", namesDisc)
	}
	if !namesDisc["acquire_sources"] || !namesDisc["run_lua"] || !namesDisc["submit_delivery"] {
		t.Fatalf("source_discovery subtask missing symmetric acquisition/delivery tools: %+v", namesDisc)
	}
}

func TestExtractRecordsFromResponses(t *testing.T) {
	longBody := strings.Repeat("x", 600)
	goJSON := `[
		{"version": "go1.27.4", "stable": true, "files": [{"filename": "go1.27.4.src.tar.gz", "size": 12345}]},
		{"version": "go1.27rc3", "stable": false, "files": [{"filename": "go1.27rc3.linux-amd64.tar.gz"}]}
	]`
	respGo := acquisition.Response{
		Receipt: acquisition.Receipt{URL: "https://go.dev/dl/?mode=json&include=all", Status: 200, FetchedAt: time.Now()},
		Body:    goJSON,
	}
	redisJSON := `[
		{"tag_name": "8.10.3", "name": "Redis 8.10.3", "prerelease": false, "published_at": "2026-10-15T12:00:00Z", "body": "` + longBody + `"},
		{"tag_name": "8.12-rc3", "name": "Redis 8.12-rc3", "prerelease": true, "published_at": "2026-11-01T12:00:00Z"}
	]`
	respRedis := acquisition.Response{
		Receipt: acquisition.Receipt{URL: "https://api.github.com/repos/redis/redis/releases?per_page=30", Status: 200, FetchedAt: time.Now()},
		Body:    redisJSON,
	}

	records := extractJSONRecordsFromResponses([]acquisition.Response{respGo, respRedis})
	if len(records) != 4 {
		t.Fatalf("expected 4 generic JSON records extracted, got %d", len(records))
	}
	if records[0].Fields["version"] != "go1.27.4" || records[0].Fields["stable"] != true {
		t.Fatalf("unexpected fields for go stable: %+v", records[0].Fields)
	}
	if _, hasFiles := records[0].Fields["files"]; hasFiles {
		t.Fatalf("expected nested non-scalar files array omitted from flat observation fields: %+v", records[0].Fields)
	}
	if records[2].Fields["tag_name"] != "8.10.3" {
		t.Fatalf("unexpected fields for redis release: %+v", records[2].Fields)
	}
	if bodyStr, ok := records[2].Fields["body"].(string); !ok || len([]rune(bodyStr)) != 600 {
		t.Fatalf("expected full scalar body preserved in record, got ok=%v len=%d", ok, len([]rune(bodyStr)))
	}

	previews := compactObservationPreviews([]Observation{
		{ID: "obs-1", Fields: records[0].Fields},
		{ID: "obs-2", Fields: records[2].Fields},
	}, 2000)
	if len(previews) != 2 {
		t.Fatalf("expected 2 observation previews, got %+v", previews)
	}
	large := respGo
	large.Body = "[" + strings.Repeat(`{"version":"1.0"},`, 100) + `{"version":"2.0"}]`
	if got := extractJSONRecordsFromResponses([]acquisition.Response{large}); len(got) != 101 || got[100].Fields["version"] != "2.0" {
		t.Fatalf("acquisition truncated the stored result: %d rows", len(got))
	}
	ambiguous := respGo
	ambiguous.Body = `{"stable":[{"version":"1.0"}],"preview":[{"version":"2.0"}]}`
	if got := extractJSONRecordsFromResponses([]acquisition.Response{ambiguous}); len(got) != 0 {
		t.Fatal("generic acquisition picked an ambiguous JSON path", got)
	}
}

func TestAcquireSourcesToleratesFlexibleFieldTypes(t *testing.T) {
	raw := `{"urls":["https://go.dev/dl/"],"acquisition":{"goal":"查询版本与发布日期","fields":[{"name":"version","type":"string"},{"name":"release_date","type":"date"},{"name":"size_mb","type":"float"}]}}`
	if err := validateInformationToolArgs("acquire_sources", raw); err != nil {
		t.Fatalf("expected acquire_sources schema validation to allow normalized semantic field types, got: %v", err)
	}
	var args informationAcquireArgs
	if err := decodeInformationArgs(raw, &args); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if args.Acquisition == nil {
		t.Fatal("expected acquisition to be decoded")
	}
	if _, err := withLocalAcquisition(context.Background(), *args.Acquisition); err != nil {
		t.Fatalf("expected withLocalAcquisition to normalize date/float field types cleanly, got: %v", err)
	}
}

func TestSingleTierBrowserFallbackReasonPolicy(t *testing.T) {
	cases := []struct {
		name   string
		resp   acquisition.Response
		wantOK bool
	}{
		{"http-403", acquisition.Response{Receipt: acquisition.Receipt{URL: "https://example.org/a", Status: 403}}, false},
		{"http-429", acquisition.Response{Receipt: acquisition.Receipt{URL: "https://example.org/a", Status: 429}}, false},
		{"http-503", acquisition.Response{Receipt: acquisition.Receipt{URL: "https://example.org/a", Status: 503}}, false},
		{"challenge-page", acquisition.Response{Receipt: acquisition.Receipt{URL: "https://example.org/verify", Status: 403}, Body: `<html><body>Verify you are human</body></html>`}, true},
		{"js-empty-shell", acquisition.Response{Receipt: acquisition.Receipt{URL: "https://example.org/spa", Status: 200}, Body: `<html><body><div id="root"></div><script src="/app.js"></script></body></html>`}, true},
		{"spa-with-nav-footer", acquisition.Response{Receipt: acquisition.Receipt{URL: "https://example.org/jobs", Status: 200}, Body: `<html><body><nav>Home Jobs</nav><div id="app"></div><footer>Copyright</footer><script src="/app.js"></script></body></html>`}, true},
		{"http-404", acquisition.Response{Receipt: acquisition.Receipt{URL: "https://example.org/missing", Status: 404}}, false},
		{"normal-html-content", acquisition.Response{Receipt: acquisition.Receipt{URL: "https://example.org/docs", Status: 200}, Body: `<html><body><main><h1>Go Releases</h1><p>go1.27.4 released 2026-11-10 includes security fixes for net/http and crypto packages across platforms.</p></main><script src="/nav.js"></script></body></html>`}, false},
		{"json-payload", acquisition.Response{Receipt: acquisition.Receipt{URL: "https://example.org/api", Status: 200}, Body: `{"items":[]}`}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, ok := browserFallbackReason(tc.resp)
			if ok != tc.wantOK {
				t.Fatalf("browserFallbackReason=%v (reason=%q), want=%v", ok, reason, tc.wantOK)
			}
		})
	}
}

func TestStage1PlanAndAcquirePreconditionAndBatchOrder(t *testing.T) {
	ctx := context.Background()
	sampleBody := `[{"product":"AlphaDB","branch":"Mainline","version":"2.1.0","release_date":"2026-11-10"}]`

	newTestService := func(t *testing.T, gets *atomic.Int32) *Service {
		t.Helper()
		publicSource(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gets.Add(1)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, sampleBody)
		}))
		s, _ := retrievalService(t)
		s.Crawler.Fingerprint = "stage1-batch-order"
		return s
	}

	toolNames := func(defs []aiapp.ToolDefinition) map[string]bool {
		out := make(map[string]bool, len(defs))
		for _, d := range defs {
			out[d.Function.Name] = true
		}
		return out
	}

	t.Run("same_batch_plan_then_acquire", func(t *testing.T) {
		var gets atomic.Int32
		s := newTestService(t, &gets)
		r, err := s.Create(ctx, Input{
			RequestKey:  "same-batch-plan-acquire",
			Goal:        "查询 AlphaDB 最新版本与发布日期并对比",
			SourceScope: []string{"8.8.8.8"},
			EntryURLs:   []string{"http://8.8.8.8/release.json"},
		})
		if err != nil {
			t.Fatal(err)
		}
		model := &informationScriptModel{script: func(input aiapp.ChatRequest, n int) (aiapp.Message, error) {
			switch n {
			case 1:
				avail := toolNames(input.Tools)
				if !avail["update_plan"] || !avail["acquire_sources"] {
					return aiapp.Message{}, fmt.Errorf("expected both update_plan and acquire_sources in Stage 1 tools, got %+v", avail)
				}
				return aiapp.Message{
					Role: "assistant",
					ToolCalls: []aiapp.ToolCall{
						{
							ID:   "call-plan-1",
							Type: "function",
							Function: aiapp.FunctionCall{
								Name:      "update_plan",
								Arguments: string(mustMarshal(WorkPlan{Interpretation: "获取 AlphaDB 版本记录", Route: "direct", CoverageRationale: "读取入口 JSON"})),
							},
						},
						{
							ID:   "call-acq-1",
							Type: "function",
							Function: aiapp.FunctionCall{
								Name: "acquire_sources",
								Arguments: string(mustMarshal(informationAcquireArgs{
									URLs: []string{"http://8.8.8.8/release.json"},
									Acquisition: &AcquisitionRequest{
										Goal:   "读取版本与发布日期",
										Fields: []aiapp.FieldRequirement{{Name: "product", Type: "string"}, {Name: "version", Type: "string"}, {Name: "release_date", Type: "string"}},
									},
								})),
							},
						},
					},
				}, nil
			case 2:
				if gets.Load() != 1 {
					return aiapp.Message{}, fmt.Errorf("expected 1 HTTP fetch after same-batch plan+acquire, got %d", gets.Load())
				}
				page, err := s.QueryObservations(ctx, r.ID, ObservationQuery{Limit: 10})
				if err != nil || len(page.Items) != 1 {
					return aiapp.Message{}, fmt.Errorf("expected 1 observation, len=%d err=%v", len(page.Items), err)
				}
				obs := page.Items[0]
				return informationCall("call-sub-1", "submit_delivery", DeliveryDraft{
					RequirementsRevision: 1,
					Outcome:              "completed",
					Report:               "AlphaDB Mainline 最新版本为 2.1.0，发布于 2026-11-10。",
					Tables: []DeliveryTable{{
						Title:             "版本记录",
						Columns:           []string{"产品", "分支", "版本号", "发布日期", "官方来源链接"},
						RowObservationIDs: [][]string{{obs.ID}},
						Rows: []map[string]any{{
							"产品":     "AlphaDB",
							"分支":     "Mainline",
							"版本号":    "2.1.0",
							"发布日期":   "2026-11-10",
							"官方来源链接": "http://8.8.8.8/release.json",
						}},
					}},
				}), nil
			default:
				return aiapp.Message{}, fmt.Errorf("unexpected extra model call %d", n)
			}
		}}
		s.Model = model
		if err := s.Advance(ctx, r.ID); err != nil {
			t.Fatal(err)
		}
		got, err := s.Request(ctx, r.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != "completed" || model.calls != 2 || gets.Load() != 1 {
			t.Fatalf("expected 2-round completion with 1 fetch, got status=%s calls=%d gets=%d", got.Status, model.calls, gets.Load())
		}
		var steps []Step
		if err := s.DB.Where("request_id = ? AND kind IN ?", r.ID, []string{"update_plan", "acquire_sources", "submit_delivery"}).Order("id ASC").Find(&steps).Error; err != nil {
			t.Fatal(err)
		}
		if len(steps) != 3 || steps[0].Kind != "update_plan" || steps[1].Kind != "acquire_sources" || steps[2].Kind != "submit_delivery" {
			t.Fatalf("expected exact sequential order update_plan -> acquire_sources -> submit_delivery, got: %+v", steps)
		}
		for _, st := range steps {
			if st.Stage != "succeeded" {
				t.Fatalf("expected step %s to succeed, got stage=%s err=%s", st.Kind, st.Stage, st.Error)
			}
		}
	})

	t.Run("ask_user_pause_stops_subsequent_acquire", func(t *testing.T) {
		var gets atomic.Int32
		s := newTestService(t, &gets)
		r, err := s.Create(ctx, Input{
			RequestKey:  "ask-user-before-acquire",
			Goal:        "查询版本",
			SourceScope: []string{"8.8.8.8"},
		})
		if err != nil {
			t.Fatal(err)
		}
		model := &informationScriptModel{script: func(_ aiapp.ChatRequest, n int) (aiapp.Message, error) {
			if n > 1 {
				return aiapp.Message{}, fmt.Errorf("model called again while paused on ask_user")
			}
			return aiapp.Message{
				Role: "assistant",
				ToolCalls: []aiapp.ToolCall{
					{ID: "call-ask-plan", Type: "function", Function: aiapp.FunctionCall{Name: "update_plan", Arguments: string(mustMarshal(WorkPlan{Interpretation: "需确认目标版本口径", Route: "direct", CoverageRationale: "待澄清"}))}},
					{ID: "call-ask-user", Type: "function", Function: aiapp.FunctionCall{Name: "ask_user", Arguments: `{"question":"请确认需要查询哪一个分支？"}`}},
					{ID: "call-ask-acq", Type: "function", Function: aiapp.FunctionCall{Name: "acquire_sources", Arguments: string(mustMarshal(informationAcquireArgs{URLs: []string{"http://8.8.8.8/release.json"}}))}},
				},
			}, nil
		}}
		s.Model = model
		if err := s.Advance(ctx, r.ID); err != nil {
			t.Fatal(err)
		}
		got, _ := s.Request(ctx, r.ID)
		if got.Status != "needs_input" || gets.Load() != 0 {
			t.Fatalf("expected pause at needs_input with 0 fetches, got status=%s gets=%d", got.Status, gets.Load())
		}
		if !strings.Contains(got.EinoMessagesJSON, "BATCH_NOT_DISPATCHED") {
			t.Fatalf("expected BATCH_NOT_DISPATCHED receipt for acquire_sources after ask_user, got: %s", got.EinoMessagesJSON)
		}
	})

	t.Run("replay_and_idempotency_unaffected_without_plan", func(t *testing.T) {
		var gets atomic.Int32
		s := newTestService(t, &gets)
		s.NewFetcher = func([]string) (*acquisition.PublicFetcher, error) {
			t.Fatal("fetcher must not run during replay")
			return nil, nil
		}
		r, err := s.Create(ctx, Input{RequestKey: "replay-precond-check", Goal: "验证回放", SourceScope: []string{"8.8.8.8"}})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.DB.Model(&Request{}).Where("id = ?", r.ID).Updates(map[string]any{"status": "running", "lease_token": "lease-replay", "lease_until": time.Now().Add(5 * time.Minute)}).Error; err != nil {
			t.Fatal(err)
		}
		r, _ = s.Request(ctx, r.ID)
		host := &informationHost{service: s, owner: requestExecutionOwner{RequestID: r.ID, Token: "lease-replay"}, request: r, input: Input{Goal: r.GoalForTest(), SourceScope: []string{"8.8.8.8"}}}

		raw := string(mustMarshal(informationAcquireArgs{URLs: []string{"http://8.8.8.8/release.json"}}))
		normRaw := normalizeToolArgsDeterministic("acquire_sources", raw)
		args, err := informationBusinessArgs(normRaw, "acquire_sources")
		if err != nil {
			t.Fatal(err)
		}
		hash := informationToolInput("acquire_sources", args, "")
		savedReceipt := string(mustMarshal(informationReceipt{Name: "acquire_sources", CallID: "orig-acq-1", Status: "succeeded", Data: map[string]any{"replayed": true}}))
		if err := s.DB.Create(&Step{
			EinoEpoch: r.EinoEpoch, RequestID: r.ID, Key: "eino_tool:" + hash, Kind: "acquire_sources",
			CallID: "orig-acq-1", InputHash: hash, RawInputJSON: normRaw, InputJSON: normRaw,
			Stage: "succeeded", ResultJSON: savedReceipt,
		}).Error; err != nil {
			t.Fatal(err)
		}

		replayed, err := host.invoke(ctx, "orig-acq-1", "acquire_sources", raw)
		if err != nil || replayed != savedReceipt {
			t.Fatalf("expected original receipt replay without plan, got %q err=%v", replayed, err)
		}
		dupOut, err := host.invoke(ctx, "dup-acq-2", "acquire_sources", raw)
		if err != nil {
			t.Fatal(err)
		}
		var dupRec informationReceipt
		if err := json.Unmarshal([]byte(dupOut), &dupRec); err != nil || dupRec.Code != "ACTION_ALREADY_CALLED" {
			t.Fatalf("expected ACTION_ALREADY_CALLED, got: %s", dupOut)
		}
	})
}
