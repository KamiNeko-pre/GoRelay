package information

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/knowledge"
)

func TestRepairPlanV3_PointerNormalizationAndReplayBinding(t *testing.T) {
	respBody := `[{"version":"go1.27.4"},{"version":"go1.27.3"}]`
	plan := APIPlan{
		URL:          "https://example.org/dl.json",
		ItemsPointer: "",
		Fields:       map[string]string{"version": "version"},
	}
	normPlan, err := normalizeAPIPlan(plan)
	if err != nil || normPlan.Fields["version"] != "/version" {
		t.Fatalf("expected normalized /version, got %+v err=%v", normPlan, err)
	}
	runtime, err := executeAPI(context.Background(), plan, func(_ context.Context, method, target string) (acquisition.Response, error) {
		return acquisition.Response{Receipt: acquisition.Receipt{URL: target, Method: method, Status: 200, FetchedAt: time.Now()}, Body: respBody}, nil
	})
	if err != nil {
		t.Fatalf("executeAPI failed: %v", err)
	}
	items := runtime.Collection["items"].([]any)
	item0 := items[0].(map[string]any)
	ev := item0["_evidence"].(apiItemEvidence)
	if ev.Pointers["version"] != "/0/version" || ev.ResponseIndex != 1 {
		t.Fatalf("incorrect source location for the first API record: %+v", ev)
	}

	leftArgs, _ := informationBusinessArgs(string(mustMarshal(informationAcquireArgs{API: &APIPlan{URL: "https://example.org/dl.json", ItemsPointer: "", Fields: map[string]string{"version": "version"}}})), "acquire_sources")
	rightArgs, _ := informationBusinessArgs(string(mustMarshal(informationAcquireArgs{API: &APIPlan{URL: "https://example.org/dl.json", ItemsPointer: "", Fields: map[string]string{"version": "/version"}}})), "acquire_sources")
	if informationToolInput("acquire_sources", leftArgs, "") != informationToolInput("acquire_sources", rightArgs, "") {
		t.Fatal("expected normalized 'version' and '/version' to yield identical idempotency key")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/dl-manifest.json", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[{"version":"go1.27.4"}]`)
	})
	_ = publicSource(t, mux)

	s, _ := retrievalService(t)
	s.Crawler.Fingerprint = "bind-replay-test"
	ctx := context.Background()
	rTrial, err := s.Create(ctx, Input{RequestKey: "trial-manifest-req", Goal: "核对官方版本"})
	if err != nil {
		t.Fatal(err)
	}
	trialCtx, _ := withLocalAcquisition(ctx, AcquisitionRequest{Goal: "版本", Fields: []aiapp.FieldRequirement{{Name: "version", Type: "string"}}})
	trialRes, err := s.TrialAPI(trialCtx, rTrial.ID, "manifest check", APIPlan{URL: "http://8.8.8.8/dl-manifest.json", ItemsPointer: "", Fields: map[string]string{"version": "version"}})
	if err != nil {
		t.Fatalf("TrialAPI with relative field pointer failed: %v", err)
	}
	var savedPlan APIPlan
	if err := json.Unmarshal([]byte(trialRes.Asset.ManifestJSON), &savedPlan); err != nil || savedPlan.Fields["version"] != "/version" {
		t.Fatalf("expected Asset.ManifestJSON to store normalized '/version', got: %s err=%v", trialRes.Asset.ManifestJSON, err)
	}

	r, err := s.Create(ctx, Input{RequestKey: "bind-replay-req", Goal: "核对官方版本并提交"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Model(&Request{}).Where("id = ?", r.ID).Updates(map[string]any{"status": "running", "lease_token": "lease-1", "lease_until": time.Now().Add(5 * time.Minute)}).Error; err != nil {
		t.Fatal(err)
	}
	doc1, _ := knowledge.NewDocument(knowledge.Input{Source: "s1", SourceKey: "d1", URL: "https://example.org/1", Title: "D1", Body: "v1.0", Format: "text", FetchedAt: time.Now().UTC().Truncate(time.Millisecond)})
	seedStep := Step{RequestID: r.ID, Key: "seed-1", Kind: "acquire_sources", Stage: "succeeded"}
	if err := s.DB.Create(&seedStep).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := s.IngestAcquisition(ctx, r.ID, seedStep.ID, []AcquiredResponse{{Key: "r1", Document: doc1}}, nil); err != nil {
		t.Fatal(err)
	}
	r, _ = s.Request(ctx, r.ID)
	host := &informationHost{service: s, owner: requestExecutionOwner{RequestID: r.ID, Token: "lease-1"}, request: r, input: Input{Goal: r.GoalForTest()}}

	rawModelSubmit := `{"outcome":"complete","report":"版本为 1.0。"}`
	normRaw := normalizeToolArgsDeterministic("submit_delivery", rawModelSubmit)
	boundRaw, err := host.bindCurrentRead(ctx, "submit_delivery", normRaw)
	if err != nil {
		t.Fatal(err)
	}
	crashedStep := Step{
		EinoEpoch: r.EinoEpoch, RequestID: r.ID, Key: "eino_tool:" + informationToolInput("submit_delivery", mustDecodeMap(boundRaw), ""),
		Kind: "submit_delivery", CallID: "crash-sub-1", InputHash: "h1", RawInputJSON: normRaw, InputJSON: boundRaw, Stage: "reserved",
	}
	if err := s.DB.Create(&crashedStep).Error; err != nil {
		t.Fatal(err)
	}

	doc2, _ := knowledge.NewDocument(knowledge.Input{Source: "s2", SourceKey: "d2", URL: "https://example.org/2", Title: "D2", Body: "v2.0", Format: "text", FetchedAt: time.Now().UTC().Truncate(time.Millisecond)})
	if _, err := s.IngestAcquisition(ctx, r.ID, seedStep.ID, []AcquiredResponse{{Key: "r2", Document: doc2}}, nil); err != nil {
		t.Fatal(err)
	}
	reqs2 := TaskRequirements{Revision: 2, OriginalText: "核对官方版本并提交"}
	if err := s.DB.Model(&Request{}).Where("id = ?", r.ID).Updates(map[string]any{"requirements_revision": 2, "requirements_json": string(mustMarshal(reqs2))}).Error; err != nil {
		t.Fatal(err)
	}

	outStr, err := host.invoke(ctx, "crash-sub-1", "submit_delivery", rawModelSubmit)
	if err != nil {
		t.Fatalf("crash replay of submit_delivery failed: %v", err)
	}
	var receipt informationReceipt
	if err := json.Unmarshal([]byte(outStr), &receipt); err != nil {
		t.Fatal(err)
	}
	var persistedStep Step
	_ = s.DB.First(&persistedStep, crashedStep.ID).Error
	var replayedDraft DeliveryDraft
	_ = json.Unmarshal([]byte(persistedStep.InputJSON), &replayedDraft)
	if replayedDraft.RequirementsRevision != 1 || len(replayedDraft.References) != 1 || replayedDraft.References[0].DocID != doc1.ID {
		t.Fatalf("replay mutated original bound draft: %+v", replayedDraft)
	}
	if receipt.Code != "REQUIREMENTS_CHANGED" {
		t.Fatalf("expected REQUIREMENTS_CHANGED proving rev=1 exact bound object was replayed instead of re-binding rev=2, got: %s", outStr)
	}
	freshOut, err := host.invoke(ctx, "fresh-sub-2", "submit_delivery", rawModelSubmit)
	if err != nil {
		t.Fatalf("fresh submit_delivery failed: %v", err)
	}
	var freshReceipt informationReceipt
	_ = json.Unmarshal([]byte(freshOut), &freshReceipt)
	if freshReceipt.Status != "succeeded" {
		t.Fatalf("expected fresh call with updated bindings to succeed, got: %s", freshOut)
	}
}

func mustDecodeMap(raw string) map[string]any {
	var m map[string]any
	_ = json.Unmarshal([]byte(raw), &m)
	return m
}

func TestRepairPlanV3_FailedTrialValidResponseAndErrorRejection(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/partial-fields.json", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `[{"version":"go1.27.4"},{"version":"go1.27.3"}]`)
	})
	mux.HandleFunc("/broken-404.json", func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	})
	_ = publicSource(t, mux)

	s, _ := retrievalService(t)
	s.Crawler.Fingerprint = "trial-partial-test"
	ctx := context.Background()

	// TrialAPI fails Step (wrong ItemsPointer) yet HTTP 200 saved valid body capture -> usable for completed.
	rPos, err := s.Create(ctx, Input{RequestKey: "trial-valid-in-failed-step", Goal: "获取版本"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Model(&Request{}).Where("id = ?", rPos.ID).Updates(map[string]any{"status": "running", "lease_token": "lease-p", "lease_until": time.Now().Add(5 * time.Minute)}).Error; err != nil {
		t.Fatal(err)
	}
	planFailField := APIPlan{URL: "http://8.8.8.8/partial-fields.json", ItemsPointer: "/missing_list", Fields: map[string]string{"version": "/version"}}
	acqCtx, _ := withLocalAcquisition(ctx, AcquisitionRequest{Goal: "版本", Fields: []aiapp.FieldRequirement{{Name: "version", Type: "string"}}})
	_, trialErr := s.TrialAPI(acqCtx, rPos.ID, "partial trial", planFailField)
	if trialErr == nil {
		t.Fatal("expected TrialAPI to fail because ItemsPointer points to missing key")
	}
	var trialStep Step
	if err := s.DB.First(&trialStep, "request_id = ? AND kind = ?", rPos.ID, "trial_api").Error; err != nil || trialStep.Stage != "failed" {
		t.Fatalf("expected TrialAPI step to have Stage='failed', got stage=%q err=%v", trialStep.Stage, err)
	}
	hostPos := &informationHost{service: s, owner: requestExecutionOwner{RequestID: rPos.ID, Token: "lease-p"}, request: rPos, input: Input{Goal: rPos.GoalForTest()}}
	_ = hostPos.refresh(ctx)
	factsPos, err := hostPos.facts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sf := factsPos["saved_facts"].(map[string]any)
	if sf["valid_documents"].(int64) != 1 || sf["succeeded_documents"].(int64) != 1 {
		t.Fatalf("expected valid HTTP 200 doc from failed TrialAPI to count as valid document, got: %+v", sf)
	}
	_, directive := hostPos.stateMachineTools(factsPos)
	if strings.Contains(directive, "Structured facts ready") || strings.Contains(directive, "submit_delivery NOW") {
		t.Fatalf("directive must not prematurely declare completion or force submit_delivery NOW: %s", directive)
	}
	outStr, err := hostPos.invoke(ctx, "c-pos", "submit_delivery", string(mustMarshal(DeliveryDraft{Outcome: "completed", Report: "已核实版本 go1.27.4 与 go1.27.3。", AcquisitionReceiptIDs: []uint64{trialStep.ID}})))
	if err != nil {
		t.Fatalf("completed delivery failed on valid failed-step capture: %v", err)
	}
	var rec informationReceipt
	_ = json.Unmarshal([]byte(outStr), &rec)
	if rec.Status != "succeeded" {
		t.Fatalf("expected completed delivery to succeed using valid HTTP 200 capture even though TrialAPI step failed, got: %s", outStr)
	}

	// HTTP 404 capture only -> completed rejects with DELIVERY_EVIDENCE_MISSING; partial passes.
	rNeg, err := s.Create(ctx, Input{RequestKey: "trial-only-404", Goal: "获取不可达页面"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Model(&Request{}).Where("id = ?", rNeg.ID).Updates(map[string]any{"status": "running", "lease_token": "lease-n", "lease_until": time.Now().Add(5 * time.Minute)}).Error; err != nil {
		t.Fatal(err)
	}
	failStep := Step{RequestID: rNeg.ID, Key: "fail-404", Kind: "acquire_sources", Stage: "failed"}
	if err := s.DB.Create(&failStep).Error; err != nil {
		t.Fatal(err)
	}
	resp404 := acquisition.Response{Receipt: acquisition.Receipt{URL: "http://8.8.8.8/broken-404.json", Status: 404, FetchedAt: time.Now().UTC().Truncate(time.Microsecond)}, Body: ""}
	if _, err := s.ingestAcquiredRecords(ctx, rNeg.ID, failStep.ID, nil, []acquisition.Response{resp404}); err != nil {
		t.Fatal(err)
	}
	rNeg, _ = s.Request(ctx, rNeg.ID)
	hostNeg := &informationHost{service: s, owner: requestExecutionOwner{RequestID: rNeg.ID, Token: "lease-n"}, request: rNeg, input: Input{Goal: rNeg.GoalForTest()}}
	outNegComp, err := hostNeg.invoke(ctx, "c-neg-comp", "submit_delivery", `{"outcome":"completed","report":"假装完成了"}`)
	if err != nil {
		t.Fatal(err)
	}
	var recNegComp informationReceipt
	_ = json.Unmarshal([]byte(outNegComp), &recNegComp)
	if recNegComp.Code != "DELIVERY_EVIDENCE_MISSING" {
		t.Fatalf("expected completed with only HTTP 404 capture to be rejected with DELIVERY_EVIDENCE_MISSING, got: %s", outNegComp)
	}
	outNegPart, err := hostNeg.invoke(ctx, "c-neg-part", "submit_delivery", `{"outcome":"partial","report":"接口返回 404。","gaps":["目标页面 HTTP 404 不可达"]}`)
	if err != nil {
		t.Fatal(err)
	}
	var recNegPart informationReceipt
	_ = json.Unmarshal([]byte(outNegPart), &recNegPart)
	if recNegPart.Status != "succeeded" {
		t.Fatalf("expected honest partial referencing 404 investigation capture to succeed, got: %s", outNegPart)
	}
}
