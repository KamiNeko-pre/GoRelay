package information

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/knowledge"
	"go-lua-crawler/internal/mcpserver"
	"go-lua-crawler/internal/testdb"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The model transport is deterministic, while Eino, SQL, tool admission,
// reference errors and delivery commit all use their production implementations.
func TestBusinessCorrectionContinues(t *testing.T) {
	s, _ := retrievalService(t)
	s.Crawler.Fingerprint = "business-correction"
	ctx := context.Background()
	r, err := s.Create(ctx, Input{RequestKey: "correction-flow", Goal: "核对已保存的官方版本资料并说明依据"})
	if err != nil {
		t.Fatal(err)
	}
	receipt := Step{RequestID: r.ID, Key: "saved-response", Kind: "inspect_api", Stage: "succeeded"}
	if err := s.DB.Create(&receipt).Error; err != nil {
		t.Fatal(err)
	}
	doc, err := knowledge.NewDocument(knowledge.Input{Source: "official", SourceKey: "release", URL: "https://example.org/releases", Title: "官方发布说明", Body: "稳定版本为 2.0。", Format: "text", FetchedAt: time.Now().UTC().Truncate(time.Millisecond)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.IngestAcquisition(ctx, r.ID, receipt.ID, []AcquiredResponse{{Key: "response", Document: doc}}, nil); err != nil {
		t.Fatal(err)
	}
	model := &informationScriptModel{script: func(input aiapp.ChatRequest, n int) (aiapp.Message, error) {
		if n > 2 {
			return aiapp.Message{}, fmt.Errorf("corrected submission did not terminate")
		}
		ref := StoredReference{DocID: "missing-document", Revision: "missing-revision"}
		if n == 2 {
			feedback := false
			for _, message := range input.Messages {
				if message.Role == "tool" && strings.Contains(message.Content, "REFERENCE_NOT_LINKED") && strings.Contains(message.Content, "原文") {
					feedback = true
				}
			}
			if !feedback {
				return aiapp.Message{}, fmt.Errorf("specific reference correction never reached model")
			}
			ref = StoredReference{DocID: doc.ID, Revision: doc.Revision}
		}
		return informationCall(fmt.Sprintf("submit-%d", n), "submit_delivery", DeliveryDraft{RequirementsRevision: 1, Report: "官方记录中的稳定版本为 2.0。", References: []StoredReference{ref}, CompletionRationale: "已核对官方保存记录", Outcome: "completed"}), nil
	}}
	s.Model = model
	if err := s.Advance(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	current, err := s.Request(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != "completed" || current.CurrentDeliveryID == "" || model.calls != 2 {
		t.Fatalf("correction did not complete: %+v calls=%d", current, model.calls)
	}
	var failed Step
	if err := s.DB.Where("request_id = ? AND kind = ? AND stage = ?", r.ID, "submit_delivery", "failed").First(&failed).Error; err != nil {
		t.Fatal("failed submission receipt missing", err)
	}
}

// This is the view boundary of the eventual facts-to-delivery scenario. SQL
// ingestion, Eino feedback and persisted delivery remain separate work in progress.
func TestBusinessFactsToDelivery_View(t *testing.T) {
	facts := []Observation{
		{ID: "price", Fields: map[string]any{"plan": "a", "price": "99"}},
		{ID: "region", Fields: map[string]any{"plan": "a", "region": "香港"}},
		{ID: "other", Fields: map[string]any{"plan": "b", "price": 200}},
		{ID: "unknown", Fields: map[string]any{"plan": "c"}},
		{ID: "conflict", Fields: map[string]any{"plan": "a", "price": 120}},
	}
	before, _ := json.Marshal(facts)
	spec := ViewSpec{ObservationIDs: []string{"price", "region", "other", "unknown"}, Columns: []string{"plan", "price", "region"}, JoinFields: []string{"plan"}, Filters: []ViewFilter{{Field: "price", Op: "lt", Value: 100}}, Sort: []ViewSort{{Field: "price", Direction: "asc"}}}
	view, err := projectObservations(facts, spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Rows) != 2 || view.Rows[0]["region"] != "香港" || view.Rows[0]["price"] != "99" || view.Rows[1]["plan"] != "c" {
		t.Fatalf("merge/filter/unknown: %+v", view)
	}
	spec.ObservationIDs = append(spec.ObservationIDs, "conflict")
	view, err = projectObservations(facts, spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Rows) != 2 || view.Rows[0]["price"] != nil {
		t.Fatalf("conflict must remain unknown: %+v", view)
	}
	spec.Resolutions = []FieldResolution{{ObservationIDs: []string{"price", "region", "conflict"}, Field: "price", SelectedObservationID: "price"}}
	view, err = projectObservations(facts, spec)
	if err != nil {
		t.Fatal(err)
	}
	if view.Rows[0]["price"] != "99" {
		t.Fatalf("explicit resolution: %+v", view)
	}
	after, _ := json.Marshal(facts)
	if string(before) != string(after) {
		t.Fatal("view mutated facts")
	}
	old := DeliveryDraft{Outcome: "completed", Tables: []DeliveryTable{{Title: "版本", JoinFields: []string{"plan"}, Rows: []map[string]any{{"plan": "a", "version": "1"}, {"plan": "b", "version": "1"}}}}}
	current := DeliveryDraft{Outcome: "completed", Tables: []DeliveryTable{{Title: "版本", JoinFields: []string{"plan"}, Rows: []map[string]any{{"plan": "b", "version": "1"}, {"plan": "a", "version": "1"}}}}}
	if diff := compareDeliveryTables(old, current); len(diff.Changes) != 0 {
		t.Fatal("row order became a business change", diff)
	}
	current.Tables[0].Rows[1]["version"] = "2"
	if diff := compareDeliveryTables(old, current); len(diff.Changes) != 1 {
		t.Fatal("stable identity update lost", diff)
	}
	current.Outcome = "partial"
	current.Tables[0].JoinFields = nil
	old.Tables[0].JoinFields = nil
	current.Tables[0].Rows = append(current.Tables[0].Rows, current.Tables[0].Rows[0])
	if diff := compareDeliveryTables(old, current); len(diff.Changes) != 3 || !strings.Contains(diff.Notice, "未出现的记录不代表") {
		t.Fatal("fallback lost multiplicity or partial warning", diff)
	}
}

func TestBusinessFactsToDelivery(t *testing.T) {
	db := testdb.Open(t)
	s := &Service{DB: db, Crawler: &aiapp.Service{Fingerprint: "integration-test"}}
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := knowledge.NewStore(db).Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	requirements := TaskRequirements{Revision: 1, OriginalText: "比较候选，并注明原文"}
	payload, _ := json.Marshal(requirements)
	r := Request{ID: "business-flow", RequestKey: "business-flow", Status: "running", RequirementsRevision: 1, RequirementsJSON: string(payload), InputJSON: "{}"}
	if err := db.Create(&r).Error; err != nil {
		t.Fatal(err)
	}
	interpretationReceipt := Step{RequestID: r.ID, Key: "interpret", Kind: "revise_requirement_interpretation", Stage: "reserved"}
	if err := db.Create(&interpretationReceipt).Error; err != nil {
		t.Fatal(err)
	}
	input := RequirementInterpretation{RequirementsRevision: 1, RequirementID: "original.reference", SourceText: "注明原文", Reason: "提取交付物", Output: &OutputRequirement{Kind: "artifact", Target: "原文表"}}
	change, err := s.ReviseRequirementInterpretation(ctx, r.ID, interpretationReceipt.ID, input)
	if err != nil || change.Revision != 2 {
		t.Fatal("requirement extraction", change, err)
	}
	replayed, err := s.ReviseRequirementInterpretation(ctx, r.ID, interpretationReceipt.ID, input)
	if err != nil || replayed.Revision != 2 {
		t.Fatal("requirement replay changed revision", replayed, err)
	}
	correctionReceipt := Step{RequestID: r.ID, Key: "correct-interpretation", Kind: "revise_requirement_interpretation", Stage: "reserved"}
	if err := db.Create(&correctionReceipt).Error; err != nil {
		t.Fatal(err)
	}
	change, err = s.ReviseRequirementInterpretation(ctx, r.ID, correctionReceipt.ID, RequirementInterpretation{RequirementsRevision: 2, RequirementID: "original.reference", SourceText: "注明原文", Reason: "用户要求原文依据，未指定独立的原文表"})
	if err != nil || change.Revision != 3 || change.Previous == nil || change.Current != nil {
		t.Fatal("requirement correction", change, err)
	}
	step := Step{RequestID: r.ID, Key: "acquire", Kind: "acquire", Stage: "completed"}
	if err := db.Create(&step).Error; err != nil {
		t.Fatal(err)
	}
	doc, err := knowledge.NewDocument(knowledge.Input{Source: "official", SourceKey: "sample", URL: "https://example.org/plans", Title: "候选套餐", Body: "A 套餐价格 99，地区香港。", Format: "text", FetchedAt: time.Now().UTC().Truncate(time.Millisecond)})
	if err != nil {
		t.Fatal(err)
	}
	records := []AcquiredRecord{{Position: 0, ResponseKey: "page", Fields: map[string]any{"plan": "a", "price": 99}, FetchedAt: doc.FetchedAt}, {Position: 1, ResponseKey: "page", Fields: map[string]any{"plan": "a", "region": "香港"}, FetchedAt: doc.FetchedAt}}
	responses := []AcquiredResponse{{Key: "page", Document: doc}}
	facts, err := s.IngestAcquisition(ctx, r.ID, step.ID, responses, records)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.IngestAcquisition(ctx, r.ID, step.ID, responses, records)
	if err != nil || len(again) != 2 || again[0].ID != facts[0].ID {
		t.Fatalf("replay: %v %+v", err, again)
	}
	if err := s.UpdatePlan(ctx, r.ID, WorkPlan{Interpretation: "变更调查步骤"}); err != nil {
		t.Fatal(err)
	}
	// Only changes to the selected facts invalidate their cursor. Saving an
	// additional original, or collecting a different fact, does not change it.
	selected := ObservationQuery{ObservationIDs: []string{facts[0].ID, facts[1].ID}, Limit: 1}
	first, err := s.QueryObservations(ctx, r.ID, selected)
	if err != nil || first.NextCursor == "" {
		t.Fatal("fact pagination", first, err)
	}
	allFirst, err := s.QueryObservations(ctx, r.ID, ObservationQuery{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	documentsBefore, err := s.QueryTaskDocuments(ctx, r.ID, DocumentQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	nextReceipt := Step{RequestID: r.ID, Key: "additional-original", Kind: "inspect_api", Stage: "succeeded"}
	if err := db.Create(&nextReceipt).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := s.IngestAcquisition(ctx, r.ID, nextReceipt.ID, []AcquiredResponse{{Key: "page", Document: doc}}, nil); err != nil {
		t.Fatal(err)
	}
	selected.Cursor = first.NextCursor
	second, err := s.QueryObservations(ctx, r.ID, selected)
	if err != nil || second.CollectionVersion != first.CollectionVersion || len(second.Items) != 1 || second.Items[0].ID == first.Items[0].ID {
		t.Fatal("new original invalidated fact pagination", second, err)
	}
	documentsAfter, err := s.QueryTaskDocuments(ctx, r.ID, DocumentQuery{Limit: 10})
	if err != nil || documentsBefore.CollectionVersion == documentsAfter.CollectionVersion {
		t.Fatal("document collection did not change", documentsAfter, err)
	}
	newFactsReceipt := Step{RequestID: r.ID, Key: "other-fact", Kind: "acquire", Stage: "succeeded"}
	if err := db.Create(&newFactsReceipt).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := s.IngestAcquisition(ctx, r.ID, newFactsReceipt.ID, []AcquiredResponse{{Key: "page", Document: doc}}, []AcquiredRecord{{Position: 0, ResponseKey: "page", Fields: map[string]any{"plan": "a", "price": 99}, FetchedAt: doc.FetchedAt}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.QueryObservations(ctx, r.ID, selected); err != nil {
		t.Fatal("unselected fact invalidated selected cursor", err)
	}
	if _, err := s.QueryObservations(ctx, r.ID, ObservationQuery{Limit: 1, Cursor: allFirst.NextCursor}); err == nil {
		t.Fatal("changed full collection accepted old cursor")
	}
	store := knowledge.NewStore(db)
	version, err := store.EvidenceRevision(ctx, knowledge.EvidenceQuery{Query: "套餐"})
	if err != nil {
		t.Fatal(err)
	}
	search, err := store.SearchEvidence(ctx, knowledge.EvidenceQuery{Query: "套餐"})
	if err != nil || version != search.CorpusRevision {
		t.Fatal("read admission and retrieval use different corpus versions", version, search, err)
	}
	view, err := s.BuildView(ctx, r.ID, ViewSpec{ObservationIDs: []string{facts[0].ID, facts[1].ID}, Columns: []string{"plan", "price", "region"}, JoinFields: []string{"plan"}})
	if err != nil {
		t.Fatal(err)
	}
	view.Title = "候选"
	submit := Step{RequestID: r.ID, Key: "submit", Kind: "submit_delivery", Stage: "executing"}
	if err := db.Create(&submit).Error; err != nil {
		t.Fatal(err)
	}
	draft := DeliveryDraft{RequirementsRevision: 3, Report: "已整理候选", Tables: []DeliveryTable{view}, References: []StoredReference{{DocID: doc.ID, Revision: doc.Revision}}, CompletionRationale: "已获得所需资料", Outcome: "completed"}
	delivery, err := s.SubmitDelivery(ctx, r.ID, submit.ID, draft)
	if err != nil {
		t.Fatal(err)
	}
	// Historical successful submission is immutable even after user state changes.
	if err := db.Model(&Request{}).Where("id = ?", r.ID).Update("requirements_revision", 4).Error; err != nil {
		t.Fatal(err)
	}
	replay, err := s.SubmitDelivery(ctx, r.ID, submit.ID, DeliveryDraft{})
	if err != nil || replay.ID != delivery.ID {
		t.Fatalf("submission replay: %+v %v", replay, err)
	}
	restarted := &Service{DB: db}
	_, saved, err := restarted.SavedDelivery(ctx, delivery.ID)
	if err != nil || len(saved.Tables) != 1 || saved.Tables[0].Rows[0]["region"] != "香港" {
		t.Fatalf("restart: %+v %v", saved, err)
	}
}

func TestBusinessDeliveryPolicy(t *testing.T) {
	updateDraft := DeliveryDraft{RequirementsRevision: 1, Outcome: "completed", Report: "本轮未变化", CompletionRationale: "核对完毕", References: []StoredReference{{DocID: "doc", Revision: "rev"}}}
	if err := checkDelivery(TaskRequirements{Revision: 1}, updateDraft, DeliveryContext{BaselineDeliveryID: "before"}); err == nil {
		t.Fatal("old references passed as a new check")
	}
	if err := checkDelivery(TaskRequirements{Revision: 1}, updateDraft, DeliveryContext{BaselineDeliveryID: "before", NewCaptures: 1}); err != nil {
		t.Fatal("unchanged source with a new capture rejected", err)
	}
	requirements := TaskRequirements{Revision: 1}
	missingEvDraft := DeliveryDraft{RequirementsRevision: 1, Outcome: "completed", Report: "只有结论没有依据"}
	if checkDelivery(requirements, missingEvDraft, DeliveryContext{}) == nil {
		t.Fatal("completed delivery without captures or references passed")
	}
	if checkDelivery(requirements, missingEvDraft, DeliveryContext{NewCaptures: 1}) == nil {
		t.Fatal("unselected captures were accepted as proof of delivery")
	}
	withoutGap := DeliveryDraft{RequirementsRevision: 1, Outcome: "partial", Report: "只取得部分结果", CompletionRationale: "已交付"}
	if checkDelivery(requirements, withoutGap, DeliveryContext{}) == nil {
		t.Fatal("generic rationale passed as the missing requirement")
	}
	partial := DeliveryDraft{
		RequirementsRevision: 1,
		Outcome:              "partial",
		Report:               "只取得一家供应商",
		Gaps:                 []string{"只取得一家供应商"},
	}
	if err := checkDelivery(requirements, partial, DeliveryContext{}); err != nil {
		t.Fatalf("honest partial rejected: %v", err)
	}
}

func TestBusinessToolContracts(t *testing.T) {
	if err := validateInformationToolArgs("read_materials", `{"kind":"structure"}`); err == nil {
		t.Fatal("read_materials still exposes a second path for inferring selectors")
	}
	page := acquisitionResponseMetadata(acquisition.Response{Body: `<main class="pricing">private fixture body</main>`})
	if strings.Contains(string(mustMarshal(page)), "private fixture body") || page["json_keys"] != nil {
		t.Fatal("HTML content leaked into the capture receipt", page)
	}
	api := acquisitionResponseMetadata(acquisition.Response{Body: `[{"price":8,"sku":"a"}]`})
	if got, _ := api["json_keys"].([]string); strings.Join(got, ",") != "price,sku" {
		t.Fatal("API inspection did not expose the actual keys", api)
	}
	nested := acquisitionResponseMetadata(acquisition.Response{Body: `{"data":{"offers":[{"a/b~": {"price":8,"sku":"sensitive-value"}}]}}`})
	shape := string(mustMarshal(nested["json_paths"]))
	if !strings.Contains(shape, `"/data/offers"`) || !strings.Contains(shape, `"/a~1b~0/price"`) || strings.Contains(shape, "sensitive-value") {
		t.Fatalf("API inspection must return actual escaped paths and types without response values: %s", shape)
	}
	for _, state := range []Request{
		{Status: "failed", Stage: "waiting", StatusJSON: `{"message":"saved provider failure"}`},
		{Status: "queued", Stage: "planning"},
	} {
		var wire map[string]any
		if err := json.Unmarshal(mustMarshal(state), &wire); err != nil {
			t.Fatal(err)
		}
		if wire["status"] != state.Status {
			t.Fatal("request lifecycle hidden by diagnostic envelope", wire["status"], state.Status)
		}
		if state.StatusJSON != "" && wire["operational_status"] == nil {
			t.Fatal("saved failure diagnostic disappeared")
		}
	}
	inputs := map[string]any{
		"ask_user":                          userQuestion{Question: "要比较哪个地区？"},
		"discover_sources":                  map[string]any{"query": "Go official releases", "saved_only": true},
		"acquire_sources":                   informationAcquireArgs{URLs: []string{"https://example.org"}, Acquisition: &AcquisitionRequest{Goal: "读取当前来源版本", Fields: []aiapp.FieldRequirement{{Name: "version", Type: "string"}}}},
		"run_lua":                           luaRunArgs{Code: "return true, { items = { { version = '1.0' } } }", Mode: "sample", URL: "https://example.org"},
		"browser_extract":                   map[string]any{"url": "https://example.org"},
		"update_plan":                       WorkPlan{Interpretation: "查阅官网", Route: "direct", CoverageRationale: "分别核对两个官方来源"},
		"revise_requirement_interpretation": RequirementInterpretation{RequirementsRevision: 1, RequirementID: "versions", SourceText: "比较版本", Reason: "修正原话解释", Output: &OutputRequirement{Kind: "artifact", Target: "版本对比"}},
		"read_materials":                    MaterialReadQuery{Kind: "observations", Limit: 10},
		"submit_delivery":                   DeliveryDraft{RequirementsRevision: 1, Report: "未取得资料", Outcome: "partial", CompletionRationale: "来源当前不可用", Gaps: []string{"缺少版本依据"}},
	}
	defs := informationToolDefinitions()
	if len(defs) != 9 {
		t.Fatalf("expected 9 consolidated tools, got %d", len(defs))
	}
	advertised := map[string]bool{}
	for _, definition := range defs {
		advertised[definition.Function.Name] = true
	}
	for name, input := range inputs {
		if !advertised[name] {
			t.Fatalf("tool missing: %s", name)
		}
		if err := validateInformationToolArgs(name, string(mustMarshal(input))); err != nil {
			t.Fatalf("advertised input %s: %v", name, err)
		}
	}
	for _, removed := range []string{
		"load_evidence", "submit_answer", "propose_delivery", "submit_work_plan", "freeze_contract", "revise_output_schema", "lookup_facts", "understand_request",
		"recover_discovery", "lookup_sources", "inspect_api", "trial_api", "execute_asset", "request_browser", "inspect_browser", "trial_browser",
		"query_observations", "query_documents", "query_delivery", "list_assets", "link_materials", "build_view", "search_saved_materials", "read_saved_material",
	} {
		if advertised[removed] {
			t.Fatalf("obsolete tool is still offered: %s", removed)
		}
	}
	left, err := informationBusinessArgs(`{"asset_id":"asset","reason":"first attempt"}`, "acquire_sources")
	if err != nil {
		t.Fatal(err)
	}
	right, err := informationBusinessArgs(`{"asset_id":"asset","reason":"after compaction"}`, "acquire_sources")
	if err != nil {
		t.Fatal(err)
	}
	if informationToolInput("acquire_sources", left, "") != informationToolInput("acquire_sources", right, "") {
		t.Fatal("new explanation bypasses unchanged action idempotency")
	}
	if informationToolInput("read_materials", inputs["read_materials"], "version-1") == informationToolInput("read_materials", inputs["read_materials"], "version-2") {
		t.Fatal("new actual data version cannot be read")
	}
	for _, payload := range []string{`null`, `{"records":[{"fields":{"version":"2.0"}}]}`} {
		host := &informationHost{}
		// No database is available: restoring an already committed physical
		// failure must not look up a mutable asset or execute anything again.
		data, wait, err, known := host.restoreDomainAction(context.Background(), "acquire_sources", `{"asset_id":"removed-asset"}`, Step{ExecutionKind: "execute_asset", ExecutionResultJSON: payload, ExecutionError: "saved source failure"})
		if !known || err == nil || wait != nil || data.(map[string]any)["asset_id"] != "removed-asset" {
			t.Fatal("physical receipt not restored", data, wait, err, known)
		}
	}
	host := &informationHost{}
	for _, call := range []struct{ tool, execution, args string }{
		{"run_lua", "run_lua", `{"code":"return true, {}","url":"https://example.org"}`},
		{"acquire_sources", "capture_sources", `{"urls":["https://example.org"]}`},
	} {
		saved := Step{ExecutionKind: call.execution, ExecutionResultJSON: `{"observation_count":12}`}
		data, wait, err, known := host.restoreDomainAction(context.Background(), call.tool, call.args, saved)
		if !known || wait != nil || err != nil || string(data.(json.RawMessage)) != saved.ExecutionResultJSON {
			t.Fatalf("%s did not restore its physical receipt: %v %v %v", call.tool, data, wait, err)
		}
		saved.ExecutionResultJSON = ""
		if _, _, _, known := host.restoreDomainAction(context.Background(), call.tool, call.args, saved); known {
			t.Fatalf("%s treated an uncommitted external action as known", call.tool)
		}
	}
}

// SQL + application routes + an actual MCP client. Source content is a local
// fixture; this is persistence acceptance, not a live model task or network run.
func TestBusinessDeliveryPersistence(t *testing.T) {
	s, _ := retrievalService(t)
	ctx := context.Background()
	importInput := DocumentImport{Input: knowledge.Input{Source: "manual", SourceKey: "release-note", Title: "导入说明", Body: "保留的本地原文", Format: "text"}}
	imported, err := s.ImportDocument(ctx, importInput)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportDocument(ctx, importInput); err != nil {
		t.Fatal(err)
	}
	var importReceipts int64
	if err := s.DB.Model(&Step{}).Where("kind = ?", "import").Count(&importReceipts).Error; err != nil || importReceipts != 1 {
		t.Fatal("import receipt replay", importReceipts, err)
	}
	if _, err := knowledge.NewStore(s.DB).Document(ctx, imported.ID, imported.Revision); err != nil {
		t.Fatal("import original missing", err)
	}
	newCapture := func(request Request, version string) (Observation, StoredReference) {
		t.Helper()
		step := Step{RequestID: request.ID, Key: "capture-" + version, Kind: "inspect_api", Stage: "succeeded"}
		if err := s.DB.Create(&step).Error; err != nil {
			t.Fatal(err)
		}
		at := time.Now().UTC().Truncate(time.Microsecond)
		response := acquisition.Response{Receipt: acquisition.Receipt{URL: "https://example.org/releases", Status: 200, FetchedAt: at}, Body: fmt.Sprintf(`{"component":"Go","version":%q}`, version)}
		rows := []aiapp.Record{{Fields: map[string]any{"component": "Go", "version": version}, Provenance: aiapp.Provenance{URL: response.Receipt.URL, FetchedAt: at}}}
		if _, err := s.ingestAcquiredRecords(ctx, request.ID, step.ID, rows, []acquisition.Response{response}); err != nil {
			t.Fatal(err)
		}
		var row observationRow
		if err := s.DB.First(&row, "receipt_id = ?", step.ID).Error; err != nil {
			t.Fatal(err)
		}
		fact, err := decodeObservation(row)
		if err != nil {
			t.Fatal(err)
		}
		return fact, fact.References[0]
	}
	running := func(request Request) Request {
		t.Helper()
		if err := s.DB.Model(&Request{}).Where("id = ?", request.ID).Update("status", "running").Error; err != nil {
			t.Fatal(err)
		}
		request.Status = "running"
		return request
	}
	draftFor := func(fact Observation, ref StoredReference) DeliveryDraft {
		return DeliveryDraft{RequirementsRevision: 1, Outcome: "completed", Report: "已核对官方版本。", CompletionRationale: "已保存本轮实际响应", References: []StoredReference{ref}, Tables: []DeliveryTable{{Title: "版本", Columns: []string{"component", "version"}, JoinFields: []string{"component"}, Rows: []map[string]any{fact.Fields}, RowObservationIDs: [][]string{{fact.ID}}}}}
	}
	submit := func(request Request, draft DeliveryDraft, key string) (Delivery, error) {
		step := Step{RequestID: request.ID, Key: key, Kind: "submit_delivery", Stage: "reserved"}
		if err := s.DB.Create(&step).Error; err != nil {
			return Delivery{}, err
		}
		return s.SubmitDelivery(ctx, request.ID, step.ID, draft)
	}
	first, err := s.Create(ctx, Input{RequestKey: "persistent-report", Goal: "跟踪官方版本", SourceScope: []string{"example.org"}})
	if err != nil {
		t.Fatal(err)
	}
	first = running(first)
	fact, ref := newCapture(first, "1.0")
	before, err := submit(first, draftFor(fact, ref), "submit")
	if err != nil {
		t.Fatal(err)
	}
	update, err := s.CreateUpdate(ctx, before.ID, UpdateInput{RequestKey: "persistent-update", Guidance: "核对新版本和升级注意事项"})
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.CreateUpdate(ctx, before.ID, UpdateInput{RequestKey: "persistent-update", Guidance: "核对新版本和升级注意事项"})
	if err != nil || replay.ID != update.ID {
		t.Fatal("update replay created another task", replay, err)
	}
	page, err := s.QueryObservations(ctx, update.ID, ObservationQuery{Limit: 10})
	if err != nil || page.Total != 0 {
		t.Fatal("baseline copied into new acquisition", page, err)
	}
	if err := s.LinkDocuments(ctx, update.ID, []StoredReference{ref}); err != nil {
		t.Fatal(err)
	}
	update = running(update)
	_, err = submit(update, draftFor(fact, ref), "too-early")
	var correction *BusinessError
	if !errors.As(err, &correction) || correction.Code != "UPDATE_ACQUISITION_REQUIRED" {
		t.Fatal("historical reference passed as a fresh update", err)
	}
	newFact, newRef := newCapture(update, "2.0")
	after, err := submit(update, draftFor(newFact, newRef), "submit")
	if err != nil {
		t.Fatal("corrected update could not deliver", err)
	}
	rebuilt := NewService(s.Crawler)
	view, err := rebuilt.deliveryDisplay(ctx, after.ID)
	if err != nil || len(view["records"].([]map[string]any)) != 1 || len(view["baseline_evidence_documents"].([]map[string]any)) == 0 {
		t.Fatal("rebuilt result lost rows or baseline originals", view, err)
	}
	comparison := view["comparison"].(DeliveryComparison)
	if len(comparison.Changes) != 1 || comparison.Changes[0].Field != "version" || comparison.Changes[0].Before != "1.0" || comparison.Changes[0].After != "2.0" {
		t.Fatal("update comparison", comparison)
	}
	gin.SetMode(gin.TestMode)
	t.Setenv("GORELAY_ADMIN_TOKEN", "persistence-test-token")
	router := gin.New()
	RegisterRoutes(router, rebuilt)
	request := httptest.NewRequest("GET", "/api/v1/information/requests/"+update.ID, nil)
	request.Header.Set("Authorization", "Bearer persistence-test-token")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	var httpResult struct {
		Result struct {
			DeliveryID string `json:"delivery_id"`
			Report     string `json:"report"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &httpResult); err != nil || response.Code != 200 || httpResult.Result.DeliveryID != after.ID {
		t.Fatal("HTTP projection", response.Code, response.Body.String(), err)
	}
	var stepsBefore int64
	s.DB.Model(&Step{}).Count(&stepsBefore)
	server := httptest.NewServer(mcpserver.NewHandler(sourceBackend{rebuilt}, ""))
	defer server.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "delivery-persistence", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	listed, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "gorelay_find_deliveries", Arguments: map[string]any{"query": "官方版本", "page_size": 1}})
	if err != nil || listed.IsError {
		t.Fatal("MCP delivery discovery", listed, err)
	}
	encoded, err := json.Marshal(listed.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var catalog mcpserver.ToolReply[mcpserver.DeliveryPage]
	if err := json.Unmarshal(encoded, &catalog); err != nil || catalog.Data == nil || catalog.Data.Total != 2 || len(catalog.Data.Deliveries) != 1 || catalog.Data.NextCursor == "" {
		t.Fatal("MCP catalog paging", string(encoded), err)
	}
	read, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "gorelay_read_delivery", Arguments: map[string]any{"delivery_id": after.ID}})
	if err != nil || read.IsError {
		t.Fatal("MCP saved delivery", read, err)
	}
	encoded, err = json.Marshal(read.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var result mcpserver.ToolReply[map[string]any]
	if err := json.Unmarshal(encoded, &result); err != nil || result.Data == nil || (*result.Data)["report"] != httpResult.Result.Report || !strings.Contains(string(encoded), server.URL+"/evidence#id=") {
		t.Fatal("MCP and HTTP disagree or lose exact links", string(encoded), err)
	}
	var stepsAfter int64
	s.DB.Model(&Step{}).Count(&stepsAfter)
	if stepsBefore != stepsAfter {
		t.Fatal("reading delivery changed execution receipts", stepsBefore, stepsAfter)
	}
	monitor, err := rebuilt.CreateMonitor(ctx, MonitorConfig{RequestKey: "persist-monitor", DeliveryID: after.ID, Focus: "关注安全修复", IntervalSeconds: 3600})
	if err != nil {
		t.Fatal(err)
	}
	check, err := rebuilt.RefreshMonitor(ctx, monitor.ID, "first-check")
	if err != nil {
		t.Fatal(err)
	}
	same, err := rebuilt.RefreshMonitor(ctx, monitor.ID, "first-check")
	if err != nil || same.ID != check.ID {
		t.Fatal("monitor check replay", same, err)
	}
	check = running(check)
	checkedFact, checkedRef := newCapture(check, "3.0")
	checked, err := submit(check, draftFor(checkedFact, checkedRef), "submit")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := rebuilt.ProcessMonitorCheck(ctx, check.ID); err != nil {
			t.Fatal(err)
		}
	}
	var inboxCount int64
	s.DB.Model(&MonitorInbox{}).Where("monitor_id = ?", monitor.ID).Count(&inboxCount)
	if err := s.DB.First(&monitor, monitor.ID).Error; err != nil || monitor.ActiveRequestID != "" || monitor.BaselineDeliveryID != checked.ID || inboxCount != 1 {
		t.Fatal("monitor result duplicated or baseline lost", monitor, inboxCount, err)
	}
	cancelled, err := rebuilt.RefreshMonitor(ctx, monitor.ID, "cancelled-check")
	if err != nil {
		t.Fatal(err)
	}
	if err := rebuilt.Cancel(ctx, cancelled.ID); err != nil {
		t.Fatal(err)
	}
	if err := rebuilt.ProcessMonitorCheck(ctx, cancelled.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.First(&monitor, monitor.ID).Error; err != nil || monitor.ActiveRequestID != "" || monitor.BaselineDeliveryID != checked.ID {
		t.Fatal("cancelled check overwrote delivery baseline", monitor, err)
	}
}

func TestBusinessLocalAcquisition(t *testing.T) {
	t.Run("captured_responses_survive_document_import_assumptions", func(t *testing.T) {
		for _, body := range []string{"", strings.Repeat("x", (2<<20)+1)} {
			response := acquisition.Response{Body: body, Receipt: acquisition.Receipt{URL: "https://example.org/response", Status: 200, FetchedAt: time.Now()}}
			documents, facts, err := acquisitionEnvelope(11, nil, []acquisition.Response{response})
			if err != nil || len(documents) != 1 || len(facts) != 0 || documents[0].Document.Body != body {
				t.Fatalf("captured response bytes=%d lost: documents=%d facts=%d error=%v", len(body), len(documents), len(facts), err)
			}
		}
	})
	// Successful facts survive a later page failure or a missing sibling field.
	for _, scenario := range []string{"later_page_fails", "missing_field", "all_pages_succeed"} {
		t.Run(scenario, func(t *testing.T) {
			calls := 0
			fetch := func(_ context.Context, method, target string) (acquisition.Response, error) {
				calls++
				receipt := acquisition.Receipt{URL: target, Method: method, Status: 200, FetchedAt: time.Now()}
				body := `{"items":[{"version":"1.0","date":"2026-11-01"}],"next":"/second"}`
				if calls == 2 {
					if scenario == "later_page_fails" {
						return acquisition.Response{Receipt: receipt}, fmt.Errorf("second page unavailable")
					}
					body = `{"items":[{"version":"2.0","date":"2026-11-02"}],"next":null}`
					if scenario == "missing_field" {
						body = `{"items":[{"version":"2.0"},{"version":"3.0","date":"2026-11-03"}],"next":null}`
					}
				}
				return acquisition.Response{Body: body, Receipt: receipt}, nil
			}
			plan := APIPlan{URL: "https://example.org/releases", ItemsPointer: "/items", Fields: map[string]string{"version": "/version", "date": "/date"}, NextPointer: "/next"}
			runtime, cause := executeAPI(context.Background(), plan, fetch)
			rows, bindErr := aiapp.BindRecords(runtime, aiapp.RecordBinding{SourceID: "releases", DataVersion: "program-1", TaskID: 1, SchemaVersion: "v1"})
			want := map[string]int{"later_page_fails": 1, "missing_field": 3, "all_pages_succeed": 2}[scenario]
			if bindErr != nil || len(rows) != want || calls != 2 {
				t.Fatalf("lost acquired facts: rows=%d want=%d calls=%d bind=%v cause=%v", len(rows), want, calls, bindErr, cause)
			}
			if (cause == nil) == (scenario == "later_page_fails") {
				t.Fatalf("unexpected error state for scenario=%s: %v", scenario, cause)
			}
			if scenario == "missing_field" && (rows[1].Fields["date"] != nil || rows[2].Fields["date"] != "2026-11-03") {
				t.Fatal("missing field filled or sibling discarded", rows, cause)
			}
			if _, records, err := acquisitionEnvelope(1, rows, runtime.SourceEvidence); err != nil || len(records) != want {
				t.Fatal("successful raw responses cannot support partial records", err)
			}
		})
	}
	for _, tc := range []struct {
		body     string
		wantBody string
	}{
		{`{"version":"1.2"}`, `{"version":"1.2"}`},
		{`<h2>version 1.2</h2>`, `version 1.2`},
	} {
		url := "https://example.org/release"
		at := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
		source := acquisition.Response{Body: tc.body, Receipt: acquisition.Receipt{URL: url, Status: 200, FetchedAt: at}}
		row := aiapp.Record{Fields: map[string]any{"version": "1.2"}, Provenance: aiapp.Provenance{URL: url, FetchedAt: at}}
		responses, records, err := acquisitionEnvelope(7, []aiapp.Record{row}, []acquisition.Response{source})
		if err != nil || len(responses) != 1 || len(records) != 1 || responses[0].Document.Body != tc.wantBody || records[0].References[0].DocID != responses[0].Document.ID {
			t.Fatalf("raw response binding: wantBody=%q got=%q %+v %v", tc.wantBody, responses[0].Document.Body, records, err)
		}
		empty, rows, err := acquisitionEnvelope(8, nil, []acquisition.Response{source})
		if err != nil || len(empty) != 1 || len(rows) != 0 {
			t.Fatal("zero-record successful response discarded", err)
		}
		if _, _, err := acquisitionEnvelope(9, []aiapp.Record{row}, nil); err == nil {
			t.Fatal("record JSON substituted for missing original")
		}
	}

	// Parent inputs convert once, without collector validation or silent loss.
	for _, spec := range []aiapp.TaskSpec{
		{RequireSummary: true},
		{RequiredFields: []aiapp.FieldRequirement{{Name: "version", Type: "string"}}, MinRecords: 2},
	} {
		requirements, err := requirementsFromInput(Input{Goal: "比较资料", Spec: &spec})
		if err != nil || len(requirements.ExplicitOutput) == 0 {
			t.Fatalf("explicit input conversion: %+v %v", requirements, err)
		}
		for _, r := range requirements.ExplicitOutput {
			if len(r.CountBy) > 0 {
				t.Fatal("collector join keys became user entity count")
			}
		}
	}
	for _, spec := range []aiapp.TaskSpec{
		{Entities: []string{"x"}, EntityField: "name"},
		{RequiredCombinations: []aiapp.RecordCondition{{ID: "region", Equals: map[string]any{"region": "A"}, MinRecords: 1}}},
		{JoinKeys: []string{"id"}},
	} {
		_, err := requirementsFromInput(Input{Goal: "比较资料", Spec: &spec})
		e, ok := err.(*BusinessError)
		if !ok || e.Code != "UNSUPPORTED_REQUIREMENT" || e.Details["path"] == "" {
			t.Fatalf("unsupported explicit input silently lost: %v", err)
		}
	}
	if _, err := requirementsFromInput(Input{Goal: "只写报告"}); err != nil {
		t.Fatal(err)
	}

	for _, field := range []string{"price", "version"} {
		ctx, err := withLocalAcquisition(context.Background(), AcquisitionRequest{Goal: "读取来源字段", Fields: []aiapp.FieldRequirement{{Name: field, Type: "string"}}})
		if err != nil {
			t.Fatal(err)
		}
		spec, err := localAcquisition(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if spec.MinRecords != 0 || spec.RequireSummary || len(spec.RequiredCombinations) > 0 || len(spec.RequiredFields) != 1 {
			t.Fatal("parent delivery conditions leaked into acquisition")
		}
	}
	if _, err := localAcquisition(context.Background()); err == nil {
		t.Fatal("missing per-action input silently accepted")
	}
	if lifecycleAfterInvocation("wait_crawler", false, &informationWait{Dependencies: []informationDependency{{Kind: "crawler", ID: "actual-job"}}}) != "waiting_dependency" {
		t.Fatal("real dependency does not wait")
	}
	if lifecycleAfterInvocation("blocked_runtime", false, nil) != "failed" {
		t.Fatal("missing execution owner remained running")
	}
	if lifecycleAfterInvocation("needs_input", false, nil) != "needs_input" {
		t.Fatal("user clarification state lost")
	}
}

// One model tool receipt owns its physical execution and crash recovery.
func TestBusinessReceiptRecovery(t *testing.T) {
	s, _ := retrievalService(t)
	ctx := context.Background()
	t.Run("native_partial_facts_survive_reconstruction", func(t *testing.T) {
		calls := 0
		publicSource(t, http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			calls++
			w.Header().Set("Content-Type", "application/json")
			if calls == 1 {
				fmt.Fprint(w, `{"items":[{"version":"1.0","date":"2026-11-01"}]}`)
			} else {
				fmt.Fprint(w, `{"items":[{"version":"2.0"}]}`)
			}
		}))
		r, err := s.Create(ctx, Input{RequestKey: "native-partial", Goal: "Read available release facts", SourceScope: []string{"8.8.8.8"}})
		if err != nil {
			t.Fatal(err)
		}
		fields := []aiapp.FieldRequirement{{Name: "version", Type: "string"}, {Name: "date", Type: "string"}}
		local, err := withLocalAcquisition(ctx, AcquisitionRequest{Goal: "Read releases", Fields: fields, JoinKeys: []string{"date"}})
		if err != nil {
			t.Fatal(err)
		}
		trial, err := s.TrialAPI(local, r.ID, "Release API", APIPlan{URL: "http://8.8.8.8/releases", ItemsPointer: "/items", Fields: map[string]string{"version": "/version", "date": "/date"}})
		if err != nil || trial.CandidateID == "" {
			t.Fatal("working capability cannot be trialled", trial, err)
		}
		asset, err := s.Publish(ctx, r.ID, trial.CandidateID)
		if err != nil {
			t.Fatal(err)
		}
		// Declaring another supported host does not forbid an allowed invocation.
		if err := s.DB.Model(&Asset{}).Where("id = ?", asset.ID).Update("scope_json", `["8.8.8.8","9.9.9.9"]`).Error; err != nil {
			t.Fatal(err)
		}
		rows, err := s.ExecuteAsset(ctx, r.ID, asset.ID, assetInvocation{})
		if err == nil || len(rows) != 1 || rows[0].Fields["version"] != "2.0" || rows[0].Fields["date"] != nil {
			t.Fatal("partial execution was lost or falsely successful", rows, err)
		}
		rebuilt := NewService(s.Crawler)
		facts, err := rebuilt.QueryObservations(ctx, r.ID, ObservationQuery{Limit: 10})
		if err != nil || facts.Total != 2 {
			t.Fatal("reconstruction lost immutable facts", facts, err)
		}
		docs, err := rebuilt.QueryTaskDocuments(ctx, r.ID, DocumentQuery{Limit: 10})
		if err != nil || docs.Total != 2 {
			t.Fatal("reconstruction lost originals", docs, err)
		}
		if _, err := s.ExecuteAsset(ctx, r.ID, asset.ID, assetInvocation{}); err == nil || !strings.Contains(err.Error(), "ACTION_ALREADY_CALLED") || calls != 2 {
			t.Fatal("unchanged failed execution repeated source IO", calls, err)
		}
		// A worker restart restores a partial failure even if its asset is later
		// revoked. Reading saved facts must not depend on a fake crawler task.
		job := nativeSourceJob{AssetID: asset.ID, SourceID: asset.SourceID, Version: asset.Version}
		if err := s.DB.Model(&Request{}).Where("id = ?", r.ID).Updates(map[string]any{"purpose": nativeSourcePurpose, "native_job_json": string(mustMarshal(job)), "status": "queued"}).Error; err != nil {
			t.Fatal(err)
		}
		if err := s.DB.Model(&Asset{}).Where("id = ?", asset.ID).Update("active", false).Error; err != nil {
			t.Fatal(err)
		}
		r, err = rebuilt.Request(ctx, r.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := rebuilt.advanceNativeSourceJob(ctx, r); err == nil || calls != 2 {
			t.Fatal("partial failure was retried or hidden", calls, err)
		}
		state, err := (sourceBackend{rebuilt}).nativeJob(ctx, r.ID)
		if err != nil || state.RecordsSaved != 1 || state.Status != "failed" {
			t.Fatal("partial job lost its rows on reconstruction", state, err)
		}
		input := mcpserver.DataInput{JobID: nativeJobPrefix + r.ID, PageSize: 1}
		page, err := (sourceBackend{rebuilt}).ReadData(ctx, input)
		if err != nil || page.Total != 2 || len(page.Records) != 1 || len(page.Records[0].Documents) == 0 || page.NextCursor == "" {
			t.Fatal("public saved-data read lost native facts or originals", page, err)
		}
		input.Cursor = page.NextCursor
		next, err := (sourceBackend{rebuilt}).ReadData(ctx, input)
		if err != nil || len(next.Records) != 1 || next.Records[0].Fields["version"] != "2.0" || next.NextCursor != "" || calls != 2 {
			t.Fatal("native saved-data cursor repeated or fetched again", next, calls, err)
		}
	})
	r, err := s.Create(ctx, Input{RequestKey: "single-receipt", Goal: "核对官方信息"})
	if err != nil {
		t.Fatal(err)
	}
	step := Step{RequestID: r.ID, Key: "eino_tool:inspect", Kind: "inspect_api", CallID: "call-1", Stage: "reserved"}
	if err := s.DB.Create(&step).Error; err != nil {
		t.Fatal(err)
	}
	owned := context.WithValue(ctx, acquisitionReceiptKey{}, step.ID)
	input := map[string]any{"url": "https://example.org", "scope": []string{}}
	physical, fresh, err := s.reserveStep(owned, r, "inspect_api_candidate", input)
	if err != nil || !fresh || physical.ID != step.ID {
		t.Fatalf("duplicate admission: %+v %v %v", physical, fresh, err)
	}
	if err := s.finishStep(owned, physical, map[string]any{"structure": "saved-source"}, nil); err != nil {
		t.Fatal(err)
	}
	var saved Step
	if err := s.DB.First(&saved, step.ID).Error; err != nil {
		t.Fatal(err)
	}
	if saved.Stage != "reserved" || saved.ResultJSON != "" || saved.ExecutionResultJSON == "" {
		t.Fatalf("physical executor completed Eino's receipt: %+v", saved)
	}
	restored, fresh, err := s.reserveStep(owned, r, "inspect_api_candidate", input)
	if err != nil || fresh || restored.ID != step.ID || restored.Stage != "succeeded" || !strings.Contains(restored.ResultJSON, "saved-source") {
		t.Fatalf("physical recovery: %+v %v %v", restored, fresh, err)
	}
	var count int64
	if err := s.DB.Model(&Step{}).Where("request_id = ?", r.ID).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("expected one durable operation, got %d: %v", count, err)
	}
	changed := Step{RequestID: r.ID, Key: "eino_tool:corrected", Kind: "inspect_api", CallID: "call-2", Stage: "reserved"}
	if err := s.DB.Create(&changed).Error; err != nil {
		t.Fatal(err)
	}
	corrected := map[string]any{"url": "https://example.org/releases", "scope": []string{}}
	if operation, fresh, err := s.reserveStep(context.WithValue(ctx, acquisitionReceiptKey{}, changed.ID), r, "inspect_api_candidate", corrected); err != nil || !fresh || operation.ID != changed.ID {
		t.Fatalf("corrected action rejected: %+v %v %v", operation, fresh, err)
	}
	// Direct HTTP execution shares the same admission/result implementation.
	direct, fresh, err := s.reserveStep(ctx, r, "inspect_api_candidate", corrected)
	if err != nil || !fresh {
		t.Fatal("direct entry admission", err)
	}
	if err := s.finishStep(ctx, direct, nil, fmt.Errorf("connection unavailable")); err != nil {
		t.Fatal(err)
	}
	prior, fresh, err := s.reserveStep(ctx, r, "inspect_api_candidate", corrected)
	if err != nil || fresh || prior.ID != direct.ID || prior.Stage != "failed" || prior.Error == "" {
		t.Fatal("direct failure replay", err)
	}
}
