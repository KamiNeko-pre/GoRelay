package information

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/engine"
	"go-lua-crawler/internal/knowledge"
)

func TestDeliveryCalculationsSeparatePromptEvidenceAndFormula(t *testing.T) {
	sources := calculationSources{
		prompt: "5 TB; 1,000 万次 GET；100 万次 PUT",
		evidence: func(id string) (string, StoredReference, error) {
			if id != "ev_rate" {
				return "", StoredReference{}, errors.New("unknown evidence")
			}
			return "Standard Storage $0.023 per binary GB", StoredReference{DocID: "doc", Revision: "v1"}, nil
		},
	}
	old := DeliveryDraft{Calculations: []DeliveryCalculation{{ID: "aws_storage", Formula: "5000 * 0.023"}, {ID: "aws_egress", Formula: "2000 * 0.09"}}}
	if err := verifyDeliveryCalculationsWithSources(&old, nil, sources); err == nil || !strings.Contains(err.Error(), "5000") || !strings.Contains(err.Error(), "2000") {
		t.Fatalf("L3 step 211 must fail: %v", err)
	}
	rate := .023
	good := DeliveryDraft{Report: "{{storage}}", Calculations: []DeliveryCalculation{{ID: "storage", Formula: "round(tb*pow(10,12)/pow(2,30)*rate*100)/100", EvidenceInputs: map[string]EvidenceCalculationInput{"rate": {EvidenceID: "ev_rate", Value: &rate}}, PromptInputs: map[string]PromptCalculationInput{"tb": {Value: 5, Label: "stored TB"}, "gets": {Value: 1e7, Label: "GET"}, "puts": {Value: 1e6, Label: "PUT"}}}}}
	if err := verifyDeliveryCalculationsWithSources(&good, nil, sources); err != nil || good.Report != "107.1" || len(good.References) != 1 {
		t.Fatalf("verified inputs should render once: %s %v", good.Report, err)
	}
	good.Report = "公式\n{{calculations:details}}"
	if err := verifyDeliveryCalculationsWithSources(&good, nil, sources); err != nil || !strings.Contains(good.Report, "107.1") || !strings.Contains(good.Report, "rate=0.023") || strings.Contains(good.Report, "{{") {
		t.Fatalf("formula declaration did not render the original operands and result: %s %v", good.Report, err)
	}
}

func TestDeliverySharesVerifiedInputsAcrossFormulas(t *testing.T) {
	for _, example := range []struct {
		name, prompt, literal, evidence string
		valid                           bool
	}{
		{"storage", "Store 5 GB", "5", "Storage $0.25 per GB", true},
		{"shipping", "Ship 5 parcels", "5", "Shipping $0.25 per parcel", true},
		{"unbound literal", "Ship 5 parcels", "6", "Shipping $0.25 per parcel", false},
		{"missing evidence", "Ship 5 parcels", "5", "Contact us for shipping rates", false},
	} {
		t.Run(example.name, func(t *testing.T) {
			const input = `{
				"report":"{{total}}\n{{calculations:details}}",
				"calculation_inputs":{"prompt_indices":{"quantity":0},"evidence_inputs":{"rate":{"evidence_id":"ev_rate","number_index":0}}},
				"calculations":[{"id":"subtotal","formula":"LITERAL * rate"},{"id":"total","formula":"subtotal + quantity * rate"}]
			}`
			var draft DeliveryDraft
			if err := json.Unmarshal([]byte(strings.Replace(input, "LITERAL", example.literal, 1)), &draft); err != nil {
				t.Fatal(err)
			}
			calls := 0
			source := calculationSources{prompt: example.prompt, evidence: func(id string) (string, StoredReference, error) {
				calls++
				if id != "ev_rate" {
					return "", StoredReference{}, errors.New("wrong evidence ID")
				}
				return example.evidence, StoredReference{DocID: "document", Revision: "revision"}, nil
			}}
			err := verifyDeliveryCalculationsWithSources(&draft, nil, source)
			if !example.valid {
				if err == nil {
					t.Fatal("unsupported amount accepted")
				}
				return
			}
			if err != nil || calls != 1 || len(draft.References) != 1 || !strings.HasPrefix(draft.Report, "2.5\n") || !strings.Contains(draft.Report, "rate=0.25") {
				t.Fatalf("shared verified operands did not reach all formulas: %s, resolves=%d, refs=%d, error=%v", draft.Report, calls, len(draft.References), err)
			}
		})
	}
}

func TestExcerptPackingReturnsNewQueriesBeforeDuplicates(t *testing.T) {
	matches := []excerptMatchEntry{
		{EvidenceID: "first", MatchedQueries: []string{"reservation"}, Excerpt: "reservation applies to on-demand requests"},
		{EvidenceID: "overlap", MatchedQueries: []string{"reservation"}, Excerpt: "reservation applies to on-demand requests"},
		{EvidenceID: "other", MatchedQueries: []string{"cancellation"}, Excerpt: "cancellation requires notice"},
	}
	budget := len(mustMarshal(matches[:2])) + 1
	packed, covered := packExcerptMatches(matches, budget)
	if len(packed) != 2 || !covered["reservation"] || !covered["cancellation"] {
		t.Fatalf("duplicate excerpts exhausted coverage: %+v, %+v", packed, covered)
	}
}

func TestDeliveryAuditKeepsRejectionReceipt(t *testing.T) {
	s, _ := retrievalService(t)
	ctx := context.Background()
	r, err := s.Create(ctx, Input{RequestKey: "delivery-semantic-audit", Goal: "Compare rates with applicable discounts"})
	if err != nil {
		t.Fatal(err)
	}
	step := Step{RequestID: r.ID, Key: "audit-test-submit", Kind: "submit_delivery", Stage: "reserved"}
	if err := s.DB.Create(&step).Error; err != nil {
		t.Fatal(err)
	}
	provider := &informationScriptModel{script: func(_ aiapp.ChatRequest, _ int) (aiapp.Message, error) {
		return aiapp.Message{Content: `{"verdict":"reject","issues":[{"category":"unit_or_scale_conflict","reason":"unit mismatch","required_action":"use the source's unit"}]}`}, nil
	}}
	s.DeliveryAuditModel = provider
	for _, goal := range []string{"Compare published storage rates", "Compare per-million and per-thousand token prices", "Compare per-million and per-thousand token prices"} {
		var business *BusinessError
		err := s.runDeliveryAudit(ctx, r.ID, step.ID, deliveryAuditInput{Goal: goal})
		if !errors.As(err, &business) || business.Code != "DELIVERY_AUDIT_REJECTED" || business.Details["issues"] == nil {
			t.Fatalf("no useful correction: %v", err)
		}
	}
	if err := s.runDeliveryAudit(ctx, r.ID, step.ID, deliveryAuditInput{Goal: "third review"}); err == nil || !strings.Contains(err.Error(), "DELIVERY_AUDIT_LIMIT") || provider.calls != 2 {
		t.Fatalf("bounded unique rejections expected: %v, calls %d", err, provider.calls)
	}
}

func TestDeliveryAuditChecksTokenPriceUnitFromSavedSources(t *testing.T) {
	s, _ := retrievalService(t)
	ctx := context.Background()
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	input := Input{Goal: "Calculate the cost of processing 1,000,000 input tokens", RequestKey: "token-cost-audit"}
	r, err := s.Create(ctx, input)
	check(err)
	check(s.DB.Model(&r).Updates(map[string]any{"status": "running", "lease_token": "audit-test", "lease_until": time.Now().Add(time.Minute)}).Error)
	r, err = s.Request(ctx, r.ID)
	check(err)
	h := &informationHost{service: s, request: r, input: input, owner: requestExecutionOwner{RequestID: r.ID, Token: r.LeaseToken}}
	acquisition := Step{RequestID: r.ID, Key: "pricing-capture", Kind: "acquire_sources", Stage: "succeeded"}
	check(s.DB.Create(&acquisition).Error)
	var rateRef StoredReference
	for _, source := range []struct{ path, body string }{
		{"rates", `{"models":[{"operation":"input tokens","quoted_price":"0.04"},{"operation":"output tokens","quoted_price":"0.08"}]}`},
		{"terms", "The unit of measurement for the quoted rate is 1,000 input tokens."},
	} {
		doc, err := knowledge.NewDocument(knowledge.Input{Source: "information-response", SourceKey: source.path,
			URL: "https://example.com/" + source.path, Title: source.path, Body: source.body, Format: "text", FetchedAt: time.Now().UTC()})
		check(err)
		_, err = s.IngestAcquisition(ctx, r.ID, acquisition.ID, []AcquiredResponse{{Key: source.path, Document: doc}}, nil)
		check(err)
		if source.path == "rates" {
			rateRef = StoredReference{DocID: doc.ID, Revision: doc.Revision}
		}
	}
	provider := &informationScriptModel{script: func(req aiapp.ChatRequest, _ int) (aiapp.Message, error) {
		var packet deliveryAuditInput
		check(json.Unmarshal([]byte(req.Messages[1].Content), &packet))
		if !strings.Contains(req.Messages[1].Content, "The unit of measurement for the quoted rate is 1,000 input tokens.") {
			t.Fatal("audit packet omitted the saved terms page")
		}
		if packet.SharedInputs["tokens"] != 1e6 || packet.SharedInputs["price"] != .04 || len(packet.PromptQuantities) != 1 || packet.PromptQuantities[0].Index != 0 || !strings.Contains(req.Messages[1].Content, `/models/0/quoted_price`) {
			t.Fatalf("audit packet omitted the shared inputs or their sources: %+v", packet)
		}
		if packet.Values["cost"] == 40 {
			return aiapp.Message{Content: `{"verdict":"pass"}`}, nil
		}
		return aiapp.Message{Content: `{"verdict":"reject","issues":[{"category":"unit_or_scale_conflict","calculation_id":"cost","reason":"price is per thousand","required_action":"divide token count by 1000"}]}`}, nil
	}}
	s.DeliveryAuditModel = provider
	draft := func(ref StoredReference, path, formula string) map[string]any {
		return map[string]any{
			"outcome": "completed", "report": "Cost is ${{cost}}", "calculations": []DeliveryCalculation{{ID: "cost", Formula: formula}},
			"calculation_inputs": map[string]any{"prompt_indices": map[string]int{"tokens": 0}, "evidence_inputs": map[string]any{
				"price": map[string]any{"source_value": map[string]any{"source_ref": ref, "json_pointer": path}},
			}},
		}
	}
	for _, bad := range []struct {
		ref  StoredReference
		path string
	}{
		{rateRef, "/models/0/unknown"},
		{rateRef, "/models/0/operation"},
		{StoredReference{DocID: rateRef.DocID, Revision: "unlinked"}, "/models/0/quoted_price"},
	} {
		key := "invalid-location-" + bad.path + "-" + bad.ref.Revision
		result, err := h.invoke(ctx, key, "submit_delivery", string(mustMarshal(draft(bad.ref, bad.path, "tokens*price/1000"))))
		check(err)
		var out informationReceipt
		check(json.Unmarshal([]byte(result), &out))
		if out.Code != "DELIVERY_CALCULATION_INVALID" || provider.calls != 0 {
			t.Fatalf("unverified path reached audit or lacked correction: %s", result)
		}
	}
	for _, formula := range []string{"tokens*price/1e6", "tokens*price/1000"} {
		result, err := h.invoke(ctx, formula, "submit_delivery", string(mustMarshal(draft(rateRef, "/models/0/quoted_price", formula))))
		check(err)
		var out informationReceipt
		check(json.Unmarshal([]byte(result), &out))
		if formula == "tokens*price/1e6" && (out.Code != "DELIVERY_AUDIT_REJECTED" || !strings.Contains(result, "divide token count by 1000")) ||
			formula == "tokens*price/1000" && out.Status != "succeeded" {
			t.Fatalf("unexpected audit outcome for %s: %s", formula, result)
		}
	}
	if provider.calls != 2 {
		t.Fatalf("expected one audit per distinct completed draft, got %d", provider.calls)
	}
}

func TestDeliveryCalculationsResolveSnapshotAndRenderOneValue(t *testing.T) {
	ref := StoredReference{DocID: "doc", Revision: "v1"}
	resolve := func(StoredReference) (string, error) {
		return "Storage uses 2 to the power of 30 bytes per unit. Rate: $0.023 per storage unit. First 100 units of egress are free.\nEgress: $0.09 per unit.", nil
	}
	makeDraft := func(rate float64, quote string) DeliveryDraft {
		return DeliveryDraft{Report: "Total {{total}} USD", Tables: []DeliveryTable{{Rows: []map[string]any{{"total": "{{total}}"}}}}, Calculations: []DeliveryCalculation{
			{ID: "storage", Formula: "5e12/pow(2,exponent)*rate", Inputs: map[string]CalculationInput{
				"exponent": {Value: 30, Reference: ref, Quote: "Storage uses 2 to the power of 30 bytes per unit."},
				"rate":     {Value: rate, Reference: ref, Quote: quote},
			}},
			{ID: "total", Formula: "round((storage + 9 + (2e12/pow(2,30)-free)*out)*100)/100", Inputs: map[string]CalculationInput{
				"free": {Value: 100, Reference: ref, Quote: "First 100 units of egress are free."},
				"out":  {Value: .09, Reference: ref, Quote: "Egress: $0.09 per unit."},
			}},
		}}
	}
	correct := makeDraft(.023, "Rate: $0.023 per storage unit.")
	if err := verifyDeliveryCalculations(&correct, resolve); err != nil || correct.Report != "Total 274.74 USD" || correct.Tables[0].Rows[0]["total"] != 274.74 {
		t.Fatalf("expected one verified number across report and table: %+v, %v", correct, err)
	}
	for _, wrong := range []DeliveryDraft{makeDraft(.02, "Rate: $0.023 per storage unit."), makeDraft(.025, "Rate: $0.025 per storage unit.")} {
		if err := verifyDeliveryCalculations(&wrong, resolve); err == nil || !strings.Contains(err.Error(), "rate") {
			t.Fatalf("ungrounded operand accepted or no recovery location: %v", err)
		}
	}
	plain := DeliveryDraft{Report: "A regular observation"}
	if err := verifyDeliveryCalculations(&plain, nil); err != nil || plain.Report != "A regular observation" {
		t.Fatalf("regular delivery changed: %+v, %v", plain, err)
	}
}

func TestCompletedDeliveryEnforcesExplicitOutput(t *testing.T) {
	minimum := 2
	requirements := TaskRequirements{Revision: 1, ExplicitOutput: []OutputRequirement{
		{ID: "prices", Kind: "fields", Target: "方案", Fields: []string{"price", "available"}},
		{ID: "providers", Kind: "count", Target: "方案", MinCount: &minimum, CountBy: []string{"provider"}},
	}}
	draft := DeliveryDraft{Outcome: "completed", References: []StoredReference{{DocID: "doc", Revision: "rev"}},
		Tables: []DeliveryTable{{Title: "方案", Rows: []map[string]any{
			{"provider": "A", "price": 0, "available": false},
			{"provider": "A", "price": 10, "available": true},
			{"provider": "B", "price": " ", "available": true},
		}}, {Title: "其他资料", Rows: []map[string]any{{"provider": "C", "price": 9, "available": true}}}}}
	var rejected *BusinessError
	if err := checkDelivery(requirements, draft, DeliveryContext{}); !errors.As(err, &rejected) ||
		!strings.Contains(err.Error(), "price") || !strings.Contains(err.Error(), "实有 1") {
		t.Fatalf("expected missing field and distinct count feedback, got %v", err)
	}
	draft.Outcome, draft.Gaps = "partial", []string{"缺少 B 的报价"}
	if err := checkDelivery(requirements, draft, DeliveryContext{}); err != nil {
		t.Fatalf("partial result was rejected: %v", err)
	}
	draft.Outcome, draft.Gaps = "completed", nil
	draft.Tables[0].Rows[2]["price"] = 30
	if err := checkDelivery(requirements, draft, DeliveryContext{}); err != nil {
		t.Fatalf("complete result was rejected: %v", err)
	}
}

func TestDeliveryPolicyStreamlinedValidationAndRAGIndexing(t *testing.T) {
	t.Run("OutcomeAndEvidenceGating", func(t *testing.T) {
		reqs := TaskRequirements{Revision: 1}
		if err := checkDelivery(reqs, DeliveryDraft{
			RequirementsRevision: 1,
			Outcome:              "completed",
			Report:               "截至2026年11月10日，Go最新版本为1.27.4。",
			References:           []StoredReference{{DocID: "doc1", Revision: "rev1"}},
		}, DeliveryContext{}); err != nil {
			t.Fatalf("expected grounded completed report to pass: %v", err)
		}

		if err := checkDelivery(reqs, DeliveryDraft{
			RequirementsRevision: 1,
			Outcome:              "partial",
		}, DeliveryContext{}); err == nil {
			t.Fatal("expected partial without report or gaps to fail")
		}

		if err := checkDelivery(reqs, DeliveryDraft{
			RequirementsRevision: 2,
			Outcome:              "completed",
			Report:               "report",
			References:           []StoredReference{{DocID: "doc1", Revision: "rev1"}},
		}, DeliveryContext{}); err == nil {
			t.Fatal("expected mismatched requirements_revision to fail")
		}
	})

	t.Run("SubmitDeliveryReusesIndexedSource", func(t *testing.T) {
		s, _ := retrievalService(t)
		ctx := context.Background()
		r, err := s.Create(ctx, Input{RequestKey: "delivery-rag-idx", Goal: "整理最新版本交付并可 RAG 检索"})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.DB.Model(&Request{}).Where("id = ?", r.ID).Updates(map[string]any{"status": "running", "lease_token": "lease-deliv", "lease_until": time.Now().Add(5 * time.Minute)}).Error; err != nil {
			t.Fatal(err)
		}
		doc, err := knowledge.NewDocument(knowledge.Input{
			Source:    "information-response",
			SourceKey: "go-dl",
			URL:       "https://go.dev/dl/",
			Title:     "Go Downloads",
			Body:      "go1.27.4 released 2026-11-10",
			Format:    "text",
			FetchedAt: time.Now().UTC().Truncate(time.Millisecond),
		})
		if err != nil {
			t.Fatal(err)
		}
		acqStep := Step{RequestID: r.ID, Key: "acq-1", Kind: "acquire_sources", Stage: "succeeded"}
		if err := s.DB.Create(&acqStep).Error; err != nil {
			t.Fatal(err)
		}
		if _, err := s.IngestAcquisition(ctx, r.ID, acqStep.ID, []AcquiredResponse{{Key: "resp", Document: doc}}, nil); err != nil {
			t.Fatal(err)
		}
		subStep := Step{RequestID: r.ID, Key: "sub-1", Kind: "submit_delivery", Stage: "executing"}
		if err := s.DB.Create(&subStep).Error; err != nil {
			t.Fatal(err)
		}
		draft := DeliveryDraft{
			RequirementsRevision: 1,
			Outcome:              "completed",
			Report:               "已完成 Go 官方版本采集，最新稳定版为 go1.27.4（发布日 2026-11-10）。",
			Tables: []DeliveryTable{{
				Title:   "版本汇总",
				Columns: []string{"版本", "发布日"},
				Rows:    []map[string]any{{"版本": "go1.27.4", "发布日": "2026-11-10"}},
			}},
			References: []StoredReference{{DocID: doc.ID, Revision: doc.Revision}},
		}
		if _, err := s.SubmitDelivery(ctx, r.ID, subStep.ID, draft); err != nil {
			t.Fatalf("SubmitDelivery failed: %v", err)
		}

		store := knowledge.NewStore(s.DB)
		version, err := store.EvidenceRevision(ctx, knowledge.EvidenceQuery{Query: "go1.27.4"})
		if err != nil {
			t.Fatal(err)
		}
		search, err := store.SearchEvidence(ctx, knowledge.EvidenceQuery{Query: "go1.27.4"})
		if err != nil {
			t.Fatal(err)
		}
		if version == "" || len(search.Entries) == 0 {
			t.Fatalf("expected indexed delivery evidence searchable via knowledge store, got version=%q entries=%d", version, len(search.Entries))
		}
		foundSource := false
		for _, p := range search.Entries {
			if strings.Contains(p.Text, "go1.27.4") && p.SourceID == "information-response" {
				foundSource = true
			}
		}
		if !foundSource {
			t.Fatalf("expected the persisted source in knowledge search, got: %+v", search.Entries)
		}
		_ = agentdata.Hash(version)
	})
}

func TestDeliveryCalculationsHyphenNumbersAndGeneralLiterals(t *testing.T) {
	qs := parsedPromptQuantities("Compare us-east-1 Standard for 5 TB, 2 TB egress, 10 million GET, 100 万次 PUT, 30-days, and -5 offset")
	vals := map[float64]bool{}
	for _, q := range qs {
		vals[q.Value] = true
	}
	if vals[-1] || vals[-30] || !vals[5] || !vals[2] || !vals[1e7] || !vals[1e6] || !vals[-5] {
		t.Fatalf("unexpected prompt quantities parsed: %+v", qs)
	}
	if nums := extractDeliveryNumbers("AWS us-east-1 30-days rate is -0.5"); len(nums) != 3 || nums[0] != "1" || nums[1] != "30" || nums[2] != "-0.5" {
		t.Fatalf("unexpected extracted numbers: %v", nums)
	}

	sources := calculationSources{
		prompt: "10 million GET requests; 5 TB storage",
		evidence: func(id string) (string, StoredReference, error) {
			return "Price $0.004 per 10,000 requests; $0.023 per binary GiB", StoredReference{DocID: "doc", Revision: "v1"}, nil
		},
	}
	draft := DeliveryDraft{
		Report: "Total {{cost}}",
		Calculations: []DeliveryCalculation{
			{
				ID:             "cost",
				Formula:        "tb*pow(10,12)/pow(2,30)*rate + gets/1000000*100 + pow(10,6)/pow(10,6)",
				EvidenceInputs: map[string]EvidenceCalculationInput{"rate": {EvidenceID: "ev_1", Value: new(float64(0.023))}},
				PromptInputs:   map[string]PromptCalculationInput{"tb": {Value: 5, Label: "TB"}, "gets": {Value: 1e7, Label: "GET"}},
			},
		},
	}
	if err := verifyDeliveryCalculationsWithSources(&draft, nil, sources); err != nil {
		t.Fatalf("expected clean integer powers of 10 and pow exponents to pass, got %v", err)
	}
	bad := DeliveryDraft{
		Calculations: []DeliveryCalculation{
			{ID: "bad", Formula: "gets * 0.023", PromptInputs: map[string]PromptCalculationInput{"gets": {Value: 1e7, Label: "GET"}}},
		},
	}
	if err := verifyDeliveryCalculationsWithSources(&bad, nil, sources); err == nil || !strings.Contains(err.Error(), "0.023") {
		t.Fatalf("expected decimal business literal to be rejected, got %v", err)
	}
}

func TestDeliveryExcerptsRoundRobinMultiDocAndAuditBypassGuard(t *testing.T) {
	s, _ := retrievalService(t)
	ctx := context.Background()
	input := Input{Goal: "Calculate monthly cost for 5 TB storage and 2 TB egress", RequestKey: "audit-bypass-guard"}
	r, err := s.Create(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Model(&r).Updates(map[string]any{"status": "running", "lease_token": "lease-rr", "lease_until": time.Now().Add(time.Minute)}).Error; err != nil {
		t.Fatal(err)
	}
	acq := Step{RequestID: r.ID, Key: "acq-rr", Kind: "acquire_sources", Stage: "succeeded"}
	if err := s.DB.Create(&acq).Error; err != nil {
		t.Fatal(err)
	}
	largePadding := strings.Repeat(" padding rate token info line.", 90)
	doc1Body := strings.Join([]string{"vendorA price $1.0" + largePadding, "vendorA price $2.0" + largePadding, "vendorA price $3.0" + largePadding}, "\n")
	doc2Body := "vendorB price $9.0\nFirst 100 free\n"
	d1, _ := knowledge.NewDocument(knowledge.Input{Source: "information-response", SourceKey: "a", URL: "https://a.example/p", Title: "A", Body: doc1Body, Format: "text", FetchedAt: time.Now().UTC()})
	d2, _ := knowledge.NewDocument(knowledge.Input{Source: "information-response", SourceKey: "b", URL: "https://b.example/p", Title: "B", Body: doc2Body, Format: "text", FetchedAt: time.Now().UTC()})
	if _, err := s.IngestAcquisition(ctx, r.ID, acq.ID, []AcquiredResponse{{Key: "a", Document: d1}, {Key: "b", Document: d2}}, nil); err != nil {
		t.Fatal(err)
	}
	ex, err := s.QueryTaskDocumentExcerpts(ctx, r.ID, nil, MaterialReadQuery{Kind: "excerpts", Queries: []string{"price"}})
	if err != nil {
		t.Fatal(err)
	}
	matches := ex["matches"].([]excerptMatchEntry)
	seenB := false
	for _, m := range matches {
		if m.URL == "https://b.example/p" {
			seenB = true
		}
	}
	if !seenB {
		t.Fatalf("expected round-robin excerpt budget to include doc B alongside large doc A, got %d matches", len(matches))
	}
	// Even the first submit on a calculation goal cannot bypass DELIVERY_CALCULATION_REQUIRED by leaving WorkPlan.NeedsCalculation false.
	firstBypass := Step{RequestID: r.ID, Key: "sub-first-bypass", Kind: "submit_delivery", Stage: "executing"}
	if err := s.DB.Create(&firstBypass).Error; err != nil {
		t.Fatal(err)
	}
	ctxOwned := context.WithValue(ctx, requestExecutionOwnerKey{}, requestExecutionOwner{RequestID: r.ID, Token: "lease-rr"})
	if _, err := s.SubmitDelivery(ctxOwned, r.ID, firstBypass.ID, DeliveryDraft{Outcome: "completed", Report: "Estimated total $99", References: []StoredReference{{DocID: d2.ID, Revision: d2.Revision}}}); err == nil || !strings.Contains(err.Error(), "DELIVERY_CALCULATION_REQUIRED") {
		t.Fatalf("expected calculation omission on first submit to be blocked, got %v", err)
	}
	// Dropping calculations after a failed calculation step must be blocked even on retry.
	failedCalcStep := Step{RequestID: r.ID, Key: "sub-fail-calc", Kind: "submit_delivery", Stage: "failed", InputJSON: `{"outcome":"completed","calculations":[{"id":"x","formula":"1"}]}`}
	if err := s.DB.Create(&failedCalcStep).Error; err != nil {
		t.Fatal(err)
	}
	sub := Step{RequestID: r.ID, Key: "sub-bypass", Kind: "submit_delivery", Stage: "executing"}
	if err := s.DB.Create(&sub).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := s.SubmitDelivery(ctxOwned, r.ID, sub.ID, DeliveryDraft{Outcome: "completed", Report: "Estimated total $99", References: []StoredReference{{DocID: d2.ID, Revision: d2.Revision}}}); err == nil || !strings.Contains(err.Error(), "DELIVERY_CALCULATION_REQUIRED") {
		t.Fatalf("expected calculation omission bypass to be blocked, got %v", err)
	}
	if obj := extractJSONObject("Sure, here is the verdict:\n```json\n{\"verdict\":\"pass\"}\n```\nDone."); obj != `{"verdict":"pass"}` {
		t.Fatalf("unexpected extracted JSON object: %q", obj)
	}
}

func TestTaskRequiresCalculationDetectsQuantitativeIntentWithoutFalsePositives(t *testing.T) {
	positive := []TaskRequirements{
		{OriginalText: "Calculate monthly cost for 5 TB storage and 2 TB egress"},
		{OriginalText: "估算处理 1,000,000 个输入 tokens 的费用"},
		{OriginalText: "5 TB 存储和 2 TB 出网在三家云厂商选哪家更便宜、各自花费多少钱"},
		{OriginalText: "对比云存储方案", Additions: []string{"按 100 万次 PUT 与 1000 万次 GET 核算月均费用"}},
	}
	for _, req := range positive {
		if !taskRequiresCalculation(req) {
			t.Fatalf("expected taskRequiresCalculation=true for %+v", req)
		}
	}

	negative := []TaskRequirements{
		{OriginalText: "Compare pricing quotes"},
		{OriginalText: "Compare rates with applicable discounts"},
		{OriginalText: "保存和检查结构化响应"},
		{OriginalText: "查找 20 个北京 Go 后端岗位并对比薪资与发布时间"},
		{OriginalText: "查询 AlphaDB 最新版本与发布日期并对比"},
	}
	for _, req := range negative {
		if taskRequiresCalculation(req) {
			t.Fatalf("expected taskRequiresCalculation=false for plain task %+v", req)
		}
	}
}

func TestBoundedCaveatScanChunksAndNativeRecordsDefenses(t *testing.T) {
	body := strings.Repeat("中", 120000) + "\nFree tier includes 10 GB-month.\n"
	chunks := boundedCaveatScanChunks(body, 256*1024, 64*1024, 32*1024)
	if len(chunks) != 2 {
		t.Fatalf("expected head+tail caveat chunks for oversized UTF-8 doc, got %d", len(chunks))
	}
	for i, c := range chunks {
		if !utf8.ValidString(c) {
			t.Fatalf("chunk %d cut inside a UTF-8 rune", i)
		}
	}
	if !strings.Contains(chunks[1], "Free tier includes 10 GB-month.") {
		t.Fatalf("expected tail caveats to be preserved in tail chunk")
	}

	asset := Asset{Kind: "api", SourceID: "api-1", Version: "v1", SpecJSON: `{"schema_version":"v1"}`}
	step := Step{ID: 7}
	rows, err := nativeAcquisitionRecords(asset, step, engine.ProgramResult{
		Collection: map[string]any{"items": "not-an-array"},
	})
	if err == nil || len(rows) != 0 {
		t.Fatalf("expected clean error without panic on malformed runtime items, rows=%v err=%v", rows, err)
	}
}
