package information

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	mysqldsn "github.com/go-sql-driver/mysql"
	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/engine"
	"go-lua-crawler/internal/knowledge"
	"go-lua-crawler/internal/programrunner"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type savedL3ReplayMode int

const (
	savedL3Deliveries savedL3ReplayMode = iota
	savedL3Failures
	savedL3ExcerptCoverage
)

// Replays the saved L3 delivery in a disposable schema. Archive access is read-only.
func TestL3SavedDeliveryReplay(t *testing.T) {
	dir := os.Getenv("GORELAY_L3_REPLAY_DIR")
	if dir == "" {
		t.Skip("set GORELAY_L3_REPLAY_DIR to the saved L3 run directory")
	}
	replayL3Delivery(t, dir, savedL3Deliveries)
}

func TestL3SavedFailureReplay(t *testing.T) {
	dir := os.Getenv("GORELAY_L3_FAILED_REPLAY_DIR")
	if dir == "" {
		t.Skip("set GORELAY_L3_FAILED_REPLAY_DIR to the saved failed L3 run directory")
	}
	replayL3Delivery(t, dir, savedL3Failures)
}

func TestL3SavedExcerptCoverage(t *testing.T) {
	dir := os.Getenv("GORELAY_L3_EXCERPT_REPLAY_DIR")
	if dir == "" {
		t.Skip("set GORELAY_L3_EXCERPT_REPLAY_DIR to the saved Terra L3 run directory")
	}
	replayL3Delivery(t, dir, savedL3ExcerptCoverage)
}

func replayL3Delivery(t *testing.T, dir string, mode savedL3ReplayMode) {
	t.Helper()
	s, _ := retrievalService(t)
	ctx := context.Background()
	decodeFile := func(name string, result any) {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, result); err != nil {
			t.Fatal(err)
		}
	}
	var summary struct {
		Database string `json:"database"`
		TaskID   string `json:"task_id"`
	}
	var trace struct {
		Steps []Step `json:"steps"`
	}
	decodeFile("summary.json", &summary)
	decodeFile("trace.json", &trace)
	if !strings.HasPrefix(summary.Database, "gorelay_e2e_l3_") || summary.TaskID == "" {
		t.Fatal("expected an archived isolated L3 task")
	}
	dsn, err := mysqldsn.ParseDSN(os.Getenv("MYSQL_INTEGRATION_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	dsn.DBName, dsn.ParseTime = summary.Database, true
	archive, err := gorm.Open(mysql.Open(dsn.FormatDSN()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	archiveSQL, err := archive.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = archiveSQL.Close() })
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	var request Request
	var input Input
	var captures []taskDocument
	check(archive.Transaction(func(src *gorm.DB) error {
		if err := src.First(&request, "id = ?", summary.TaskID).Error; err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(request.InputJSON), &input); err != nil {
			return err
		}
		leaseUntil := time.Now().Add(time.Hour)
		request.Status, request.CurrentDeliveryID, request.LeaseToken, request.LeaseUntil = "running", "", "archived-replay", &leaseUntil
		if err := s.DB.Create(&request).Error; err != nil {
			return err
		}
		var sourceSteps []Step
		if err := src.Where("request_id = ? AND kind IN ?", request.ID, []string{"acquire_sources", "run_lua", "inspect_api_candidate", "capture_sources", "trial_api"}).Order("id").Find(&sourceSteps).Error; err != nil {
			return err
		}
		for _, step := range sourceSteps {
			if err := s.DB.Create(&step).Error; err != nil {
				return err
			}
		}
		if err := src.Where("request_id = ?", request.ID).Order("receipt_id, response_key").Find(&captures).Error; err != nil {
			return err
		}
		heads := map[string]bool{}
		versions := map[[2]string]bool{}
		for _, capture := range captures {
			key := [2]string{capture.DocID, capture.Revision}
			if !heads[capture.DocID] && isValidDocumentForCompleted(capture) {
				var head knowledge.Head
				if err := src.First(&head, "id = ?", capture.DocID).Error; err != nil {
					return err
				}
				if err := s.DB.Create(&head).Error; err != nil {
					return err
				}
				heads[capture.DocID] = true
			}
			if !versions[key] && isValidDocumentForCompleted(capture) {
				var version knowledge.DocumentVersion
				if err := src.First(&version, "doc_id = ? AND revision = ?", key[0], key[1]).Error; err != nil {
					return err
				}
				if err := s.DB.Create(&version).Error; err != nil {
					return err
				}
				versions[key] = true
			}
		}
		var links []taskObservation
		if err := src.Where("request_id = ?", request.ID).Find(&links).Error; err != nil {
			return err
		}
		for _, link := range links {
			var row observationRow
			if err := src.First(&row, "id = ?", link.ObservationID).Error; err != nil {
				return err
			}
			if err := s.DB.Create(&row).Error; err != nil {
				return err
			}
		}
		if len(links) > 0 {
			return s.DB.Create(&links).Error
		}
		return nil
	}, &sql.TxOptions{ReadOnly: true}))
	h := &informationHost{service: s, request: request, owner: requestExecutionOwner{RequestID: request.ID, Token: request.LeaseToken}, input: input}
	added := 0
	var lastArchivedProbe, extraction uint64
	var history, withoutTitle int
	var compared int
	for _, step := range trace.Steps {
		isRead := mode != savedL3Deliveries && step.Kind == "read_materials"
		if !isRead && (mode == savedL3ExcerptCoverage || step.Kind != "submit_delivery") {
			continue
		}
		for added < len(captures) && captures[added].ReceiptID < step.ID {
			cap := captures[added]
			check(s.DB.Create(&cap).Error)
			if cap.RecordCount > 0 {
				extraction = cap.ReceiptID
			} else if cap.ReceiptID != extraction {
				lastArchivedProbe = cap.ReceiptID
			}
			added++
		}
		data, err := h.invoke(ctx, fmt.Sprintf("archive-%d", step.ID), step.Kind, step.InputJSON)
		check(err)
		var rec informationReceipt
		check(json.Unmarshal([]byte(data), &rec))
		if mode == savedL3ExcerptCoverage {
			compareSavedExcerptCoverage(t, step, rec)
			compared++
			continue
		}
		if isRead {
			var out struct {
				Quantities []promptQuantityValue `json:"prompt_quantities"`
			}
			check(json.Unmarshal(mustMarshal(rec.Data), &out))
			available := map[float64]bool{}
			for _, q := range out.Quantities {
				available[q.Value] = true
			}
			if rec.Status != "succeeded" || !available[5] || !available[2] || !available[1e6] || !available[1e7] || available[5000] || available[2000] {
				t.Fatalf("step %d did not provide the original task amounts: %s %+v", step.ID, rec.Status, out)
			}
			continue
		}
		if rec.Status != "failed" {
			t.Fatalf("historical step %d (%s) was accepted", step.ID, step.Stage)
		}
		if step.Stage == "succeeded" && (rec.Code != "DELIVERY_CALCULATION_INVALID" || !strings.Contains(rec.Error, "字面量")) {
			t.Fatalf("old accepted submission failed for wrong reason: %+v", rec)
		}
		if mode == savedL3Failures {
			var submitted DeliveryDraft
			check(json.Unmarshal([]byte(step.InputJSON), &submitted))
			var fields map[string]json.RawMessage
			check(json.Unmarshal([]byte(step.InputJSON), &fields))
			if _, hasMisplacedTitle := fields["title"]; hasMisplacedTitle {
				if rec.Code != "ROUTING_ARGUMENTS_INVALID" || !strings.Contains(rec.Error, "unexpected parameter submit_delivery.title") {
					t.Fatalf("step %d should point to its misplaced top-level title: %s", step.ID, data)
				}
				delete(fields, "title")
				data, err = h.invoke(ctx, fmt.Sprintf("archive-%d-remove-invalid-property", step.ID), step.Kind, string(mustMarshal(fields)))
				check(err)
				rec = informationReceipt{}
				check(json.Unmarshal([]byte(data), &rec))
				t.Logf("step %d: removing the reported invalid property reaches actual calculation validation", step.ID)
			}
			for _, table := range submitted.Tables {
				if table.Title == "" {
					withoutTitle++
					break
				}
			}
			if rec.Code != "DELIVERY_CALCULATION_INVALID" || rec.Details["prompt_quantities"] == nil {
				t.Fatalf("step %d should reach calculation validation with actionable feedback: %s", step.ID, data)
			}
		}
		t.Logf("old step %d (%s) -> %s", step.ID, step.Stage, rec.Code)
		history++
	}
	if mode == savedL3ExcerptCoverage {
		if compared == 0 {
			t.Fatal("archive did not contain any excerpt reads to compare")
		}
		return
	}
	if mode == savedL3Failures {
		if history != 9 || withoutTitle != 4 || added != len(captures) {
			t.Fatalf("incomplete failed replay: %d submissions (%d without titles), %d/%d captures", history, withoutTitle, added, len(captures))
		}
		return
	}
	if history != 11 || added != len(captures) || extraction == 0 {
		t.Fatalf("incomplete replay: %d submissions, %d/%d captures", history, added, len(captures))
	}
	snapshots, err := s.taskAcquisitionSnapshots(ctx, request.ID, input.SourceScope)
	check(err)
	var pricing acquisitionSnapshotFixture
	for key, resp := range snapshots.responses {
		if strings.HasSuffix(resp.Receipt.URL, "/datatransfer.json") {
			pricing = acquisitionSnapshotFixture{key, snapshots.documents[key], resp.Body}
			break
		}
	}
	if len(pricing.body) < 14_000_000 || pricing.doc.RawBody != "" {
		t.Fatalf("expected actual archived large JSON with elided raw body, got %d", len(pricing.body))
	}
	t.Logf("original JSON reusable: %d bytes", len(pricing.body))

	read := func(path, query string, want float64) EvidenceCalculationInput {
		t.Helper()
		for _, cap := range captures {
			if !strings.HasSuffix(cap.SourceURL, path) {
				continue
			}
			q := MaterialReadQuery{Kind: "excerpts", Query: query, DocumentIDs: []string{cap.DocID}}
			out, err := s.QueryTaskDocumentExcerpts(ctx, request.ID, input.SourceScope, q)
			check(err)
			for _, match := range out["matches"].([]excerptMatchEntry) {
				if containsDeliveryNumber(match.Excerpt, want) {
					return EvidenceCalculationInput{EvidenceID: match.EvidenceID, Value: &want, Label: query}
				}
			}
		}
		t.Fatalf("archive has no number %g for %q in %s", want, query, path)
		return EvidenceCalculationInput{}
	}
	quantities := parsedPromptQuantities(input.Goal)
	operand := func(value float64) int {
		t.Helper()
		for _, qty := range quantities {
			if qty.Value == value {
				return qty.Index
			}
		}
		t.Fatalf("original task did not expose %g", value)
		return -1
	}
	structuredRate := func(path string) EvidenceCalculationInput {
		t.Helper()
		for _, capture := range captures {
			if strings.HasSuffix(capture.SourceURL, "/s3-standard.json") {
				return EvidenceCalculationInput{SourceValue: &JSONCalculationEvidence{
					SourceRef:   luaSourceRef{DocID: capture.DocID, Revision: capture.Revision},
					JSONPointer: path,
				}}
			}
		}
		t.Fatal("archive is missing the saved S3 JSON")
		return EvidenceCalculationInput{}
	}
	bindings := &DeliveryCalculationInputs{
		PromptIndices: map[string]int{"stored_tb": operand(5), "egress_tb": operand(2), "gets": operand(1e7), "puts": operand(1e6)},
		EvidenceInputs: map[string]EvidenceCalculationInput{
			"r2_free":      read("/r2/pricing/", "10 GB-month / month", 10),
			"r2_rate":      read("/r2/pricing/", "0.015 / GB-month", .015),
			"gib_exp":      read("/s3/pricing/", "binary gigabytes", 30),
			"s3_storage":   read("/s3-standard.json", "0.0230000000", .023),
			"s3_egress":    read("/datatransfer.json", "0.0900000000", .09),
			"s3_allowance": read("/s3/pricing/", "100GB per month", 100),
			"s3_read":      structuredRate("/regions/US East (N. Virginia)/GET and all other requests per Requests/price"),
			"s3_write":     structuredRate("/regions/US East (N. Virginia)/PUT COPY~1POST or LIST requests per Requests/price"),
			"b2_free":      read("/cloud-storage/pricing", "First 10GB", 10),
			"b2_rate":      read("/cloud-storage/pricing", "6.95", 6.95),
		},
	}
	rates := []DeliveryCalculation{
		{ID: "r2", Formula: "(stored_tb*1000-r2_free)*r2_rate"},
		{ID: "aws_storage", Formula: "stored_tb*pow(10,12)/pow(2,gib_exp)*s3_storage"},
		{ID: "aws_egress", Formula: "max(0,egress_tb*pow(10,12)/pow(2,30)-s3_allowance)*s3_egress"},
		{ID: "aws_get", Formula: "gets*s3_read"},
		{ID: "aws_put", Formula: "puts*s3_write"},
		{ID: "aws", Formula: "round((aws_storage+aws_egress+aws_get+aws_put)*100)/100"},
		{ID: "b2", Formula: "(stored_tb-b2_free/1000)*b2_rate"},
	}
	provider := &informationScriptModel{script: func(req aiapp.ChatRequest, _ int) (aiapp.Message, error) {
		var pack deliveryAuditInput
		if err := json.Unmarshal([]byte(req.Messages[1].Content), &pack); err != nil {
			return aiapp.Message{}, err
		}
		if !strings.Contains(req.Messages[1].Content, "100GB per month") || !strings.Contains(req.Messages[1].Content, "binary gigabytes") {
			return aiapp.Message{}, fmt.Errorf("audit packet dropped the actual archived rules")
		}
		if pack.SharedInputs["stored_tb"] != 5 || pack.SharedInputs["s3_read"] != .0000004 || pack.SharedInputs["s3_write"] != .000005 || !strings.Contains(req.Messages[1].Content, "PUT COPY~1POST or LIST requests") {
			return aiapp.Message{}, fmt.Errorf("audit packet lost the verified shared operands")
		}
		if pack.Values["aws"] != 274.74 {
			return aiapp.Message{Content: `{"verdict":"reject","issues":[{"category":"omitted_condition","calculation_id":"aws_egress","reason":"100 GB rule omitted","required_action":"deduct the saved 100 GB allowance"}]}`}, nil
		}
		return aiapp.Message{Content: `{"verdict":"pass","issues":[]}`}, nil
	}}
	s.DeliveryAuditModel = provider
	draft := DeliveryDraft{Outcome: "completed", Report: "R2 {{r2}}; S3 {{aws}}; B2 {{b2}}. The 30-day storage estimate and the assumed egress billing unit are stated.", AcquisitionReceiptIDs: []uint64{lastArchivedProbe, extraction}, CalculationInputs: bindings, Calculations: rates, Tables: []DeliveryTable{{Rows: []map[string]any{{"R2": "{{r2}}", "S3": "{{aws}}", "B2": "{{b2}}"}}}}}
	wrong := draft
	wrong.Calculations = append([]DeliveryCalculation(nil), rates...)
	wrong.Calculations[2].Formula = "egress_tb*pow(10,12)/pow(2,30)*s3_egress"
	invoke := func(key string, arg DeliveryDraft) informationReceipt {
		t.Helper()
		data, err := h.invoke(ctx, key, "submit_delivery", string(mustMarshal(arg)))
		check(err)
		var out informationReceipt
		check(json.Unmarshal([]byte(data), &out))
		return out
	}
	if rejected := invoke("semantic-wrong", wrong); rejected.Code != "DELIVERY_AUDIT_REJECTED" {
		t.Fatalf("semantic rejection expected: %+v", rejected)
	}
	if accepted := invoke("evidence-bound-first", draft); accepted.Status != "succeeded" {
		t.Fatalf("corrected draft did not pass on its first submission: %+v", accepted)
	}
	updated, err := s.Request(ctx, request.ID)
	check(err)
	_, saved, err := s.SavedDelivery(ctx, updated.CurrentDeliveryID)
	check(err)
	if !strings.Contains(saved.Report, "274.74") || !strings.Contains(saved.Report, "74.85") || !strings.Contains(saved.Report, "34.6805") || provider.calls != 2 {
		t.Fatalf("bad final result: %s (audits=%d)", saved.Report, provider.calls)
	}
	if len(saved.Tables) != 1 || saved.Tables[0].Title != defaultDeliveryTableTitle || len(saved.Tables[0].Columns) != 3 || saved.Tables[0].Rows[0]["S3"] != 274.74 {
		t.Fatalf("summary table should infer its display metadata and retain the verified amount: %+v", saved.Tables)
	}
	t.Logf("first corrected submission passed; %s", saved.Report)

	// Run the acquisition tools against the real archived response, with the
	// HTTP transport replaced by the persisted response. Keep the receipts real.
	probeReq, err := s.Create(ctx, Input{RequestKey: "archived-json-corpus", Goal: "保存和检查结构化响应", SourceScope: input.SourceScope})
	check(err)
	leaseUntil := time.Now().Add(time.Hour)
	check(s.DB.Model(&Request{}).Where("id = ?", probeReq.ID).Updates(map[string]any{"status": "running", "lease_token": "archived-json", "lease_until": leaseUntil}).Error)
	probeReq, err = s.Request(ctx, probeReq.ID)
	check(err)
	probeInput := Input{Goal: "保存和检查结构化响应", SourceScope: input.SourceScope}
	probeHost := &informationHost{service: s, request: probeReq, owner: requestExecutionOwner{RequestID: probeReq.ID, Token: probeReq.LeaseToken}, input: probeInput}
	capture := snapshots.responses[pricing.key]
	s.LuaRunner = func(_ context.Context, _ programrunner.Request) (engine.ProgramResult, error) {
		return engine.ProgramResult{SourceEvidence: []acquisition.Response{capture}}, nil
	}
	var lastRef StoredReference
	for _, label := range []string{"first", "second", "third"} {
		capture.Receipt.FetchedAt = time.Now().UTC()
		args := luaRunArgs{Mode: "probe", URL: capture.Receipt.URL, Code: "return true, {} -- " + label}
		data, err := probeHost.invoke(ctx, "archived-probe-"+label, "run_lua", string(mustMarshal(args)))
		check(err)
		var receipt informationReceipt
		check(json.Unmarshal([]byte(data), &receipt))
		if receipt.Status != "succeeded" {
			t.Fatalf("probe failed: %+v", receipt)
		}
		var result struct {
			Deliverable bool                       `json:"deliverable_receipt"`
			Outline     map[string]json.RawMessage `json:"json_outline"`
			References  []StoredReference          `json:"references"`
		}
		check(json.Unmarshal(mustMarshal(receipt.Data), &result))
		if result.Deliverable || len(result.Outline[capture.Receipt.URL]) == 0 || len(result.References) != 1 {
			t.Fatalf("probe response lacks an outline or wrongly claims a batch: %+v", result)
		}
		ref := result.References[0]
		if lastRef.DocID != "" && (lastRef.DocID != ref.DocID || lastRef.Revision != ref.Revision) {
			t.Fatal("identical archived JSON was written as a different document/version")
		}
		lastRef = ref
	}
	var storedVersions int64
	check(s.DB.Model(&knowledge.DocumentVersion{}).Where("doc_id = ?", lastRef.DocID).Count(&storedVersions).Error)
	if storedVersions != 1 {
		t.Fatalf("expected 1 document version after 3 captures, got %d", storedVersions)
	}
	plain := DeliveryDraft{Outcome: "completed", Report: "The archived source was saved.", References: []StoredReference{lastRef}}
	data, err := probeHost.invoke(ctx, "archived-no-calculations", "submit_delivery", string(mustMarshal(plain)))
	check(err)
	var ordinary informationReceipt
	check(json.Unmarshal([]byte(data), &ordinary))
	if ordinary.Status != "succeeded" || provider.calls != 2 {
		t.Fatalf("ordinary delivery changed or invoked audit: %+v (%d audits)", ordinary, provider.calls)
	}
	t.Log("3 captures kept one original document version and emitted JSON outlines; regular delivery used zero audit calls")
}

func compareSavedExcerptCoverage(t *testing.T, step Step, current informationReceipt) {
	t.Helper()
	type excerptData struct {
		Matches  []excerptMatchEntry `json:"matches"`
		Matched  []string            `json:"matched_queries"`
		Missing  []string            `json:"missing_queries"`
		Deferred []string            `json:"deferred_queries"`
	}
	var archive struct {
		Data excerptData `json:"data"`
	}
	if err := json.Unmarshal([]byte(step.ResultJSON), &archive); err != nil {
		t.Fatal(err)
	}
	var updated excerptData
	if err := json.Unmarshal(mustMarshal(current.Data), &updated); err != nil || current.Status != "succeeded" {
		t.Fatalf("step %d query replay failed: %v %s", step.ID, err, current.Error)
	}
	actualCoverage := func(matches []excerptMatchEntry) []string {
		var result []string
		for _, match := range matches {
			for _, query := range match.MatchedQueries {
				if !slices.Contains(result, query) {
					result = append(result, query)
				}
			}
		}
		return result
	}
	before, after := actualCoverage(archive.Data.Matches), actualCoverage(updated.Matches)
	var previouslyMissing, recovered []string
	for _, query := range archive.Data.Matched {
		if !slices.Contains(before, query) {
			previouslyMissing = append(previouslyMissing, query)
			if slices.Contains(after, query) {
				recovered = append(recovered, query)
			}
		}
	}
	t.Logf("step %d: %d -> %d queries with excerpts, previously claimed without excerpts=%v, recovered=%v, deferred=%v", step.ID, len(before), len(after), previouslyMissing, recovered, updated.Deferred)
	slices.Sort(after)
	slices.Sort(updated.Matched)
	if !slices.Equal(after, updated.Matched) {
		t.Fatalf("step %d claimed queries not present in the actual matches", step.ID)
	}
	if len(previouslyMissing) > 0 && len(recovered) == 0 {
		t.Fatalf("step %d did not recover any omitted matches", step.ID)
	}
}

type acquisitionSnapshotFixture struct {
	key  string
	doc  knowledge.Document
	body string
}
