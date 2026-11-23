package information

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/aiapp"
)

func TestRework_FactsAntiSilentLossAndDirectoryCompaction(t *testing.T) {
	s, _ := retrievalService(t)
	s.Crawler.Fingerprint = "rework-facts-index"
	ctx := context.Background()

	histReq, err := s.Create(ctx, Input{RequestKey: "hist-dir-seed", Goal: "历史来源种子"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 18; i++ {
		ds := DiscoveredSource{
			ID:                 fmt.Sprintf("hist-src-%02d", i),
			URL:                fmt.Sprintf("https://hist.example.org/source-%02d", i),
			Title:              fmt.Sprintf("Historical Source %02d", i),
			DiscoveryRequestID: histReq.ID,
			LastSeenAt:         time.Now().UTC(),
		}
		if err := s.DB.Create(&ds).Error; err != nil {
			t.Fatal(err)
		}
	}

	r, err := s.Create(ctx, Input{RequestKey: "rework-facts-req", Goal: "检查 facts 不静默丢弃", SourceScope: []string{"hist.example.org", "example.org"}})
	if err != nil {
		t.Fatal(err)
	}
	step := Step{RequestID: r.ID, Key: "acq-many", Kind: "acquire_sources", Stage: "succeeded"}
	if err := s.DB.Create(&step).Error; err != nil {
		t.Fatal(err)
	}
	var captures []acquisition.Response
	var records []aiapp.Record
	now := time.Now().UTC().Truncate(time.Microsecond)
	for i := 0; i < 20; i++ {
		u := fmt.Sprintf("https://example.org/item-%02d.json", i)
		captures = append(captures, acquisition.Response{
			Receipt: acquisition.Receipt{URL: u, Status: 200, FetchedAt: now},
			Body:    fmt.Sprintf(`{"version":"1.%d.0","description":%q}`, i, strings.Repeat("d", 140)),
		})
		records = append(records, aiapp.Record{
			Key:           u,
			SchemaVersion: "v1",
			Fields: map[string]any{
				"source_url":  u,
				"version":     fmt.Sprintf("1.%d.0", i),
				"description": strings.Repeat("d", 140),
			},
			Provenance: aiapp.Provenance{URL: u, FetchedAt: now},
		})
	}
	obs, err := s.ingestAcquiredRecords(ctx, r.ID, step.ID, records, captures)
	if err != nil || len(obs) != 20 {
		t.Fatalf("expected 20 obs, got %d err=%v", len(obs), err)
	}

	host := &informationHost{service: s, request: r, input: Input{Goal: r.GoalForTest()}}
	facts, err := host.facts(ctx)
	if err != nil {
		t.Fatal(err)
	}

	dir, ok := facts["saved_source_directory"].(map[string]any)
	if !ok || dir["total"].(int) < 18 {
		t.Fatalf("expected count-only saved_source_directory >= 18, got: %+v", facts["saved_source_directory"])
	}
	if _, hasItems := dir["items"]; hasItems {
		t.Fatalf("saved_source_directory must not dump raw items into facts context: %+v", dir)
	}

	if facts["source_coverage_total"].(int) != 20 || facts["source_coverage_omitted"].(int) != 4 {
		t.Fatalf("expected source_coverage_total=20 & omitted=4, got total=%v omitted=%v", facts["source_coverage_total"], facts["source_coverage_omitted"])
	}
	firstCov := facts["source_coverage"].([]map[string]any)[0]
	if firstCov["extraction_status"] != "extracted" {
		t.Fatalf("expected extracted status on covered doc, got: %+v", firstCov)
	}

	sf := facts["saved_facts"].(map[string]any)
	shown := sf["observation_previews_shown"].(int)
	omitted := sf["observation_previews_omitted"].(int64)
	if shown <= 0 || omitted <= 0 || int64(shown)+omitted != 20 {
		t.Fatalf("expected exact shown+omitted == 20, got shown=%d omitted=%d", shown, omitted)
	}
	omittedIdx := sf["omitted_observation_index"].([]map[string]any)
	if int64(len(omittedIdx)) != omitted {
		t.Fatalf("expected omitted_observation_index to list all %d omitted obs, got %d", omitted, len(omittedIdx))
	}
	lastOmitted := omittedIdx[len(omittedIdx)-1]
	if lastOmitted["observation_id"] == "" || lastOmitted["source_url"] == "" || len(lastOmitted["keys"].([]string)) == 0 {
		t.Fatalf("expected observation_id, source_url, and keys in omitted_observation_index: %+v", lastOmitted)
	}
}

func TestImplementationS_ResumeCursorIndexAndExplain(t *testing.T) {
	s, _ := retrievalService(t)
	s.Crawler.Fingerprint = "impl-s-resume-idx"
	ctx := context.Background()

	type indexRow struct {
		KeyName    string `gorm:"column:Key_name"`
		SeqInIndex int    `gorm:"column:Seq_in_index"`
		ColumnName string `gorm:"column:Column_name"`
	}
	var idxRows []indexRow
	if err := s.DB.Raw("SHOW INDEX FROM `information_steps` WHERE Key_name = 'idx_information_steps_req_epoch_id'").Scan(&idxRows).Error; err != nil {
		t.Fatal(err)
	}
	if len(idxRows) != 3 || idxRows[0].ColumnName != "request_id" || idxRows[1].ColumnName != "eino_epoch" || idxRows[2].ColumnName != "id" {
		t.Fatalf("expected composite index (request_id, eino_epoch, id), got: %+v", idxRows)
	}

	bigJSON := `{"payload":"` + strings.Repeat("x", 4096) + `"}`
	otherReq, err := s.Create(ctx, Input{RequestKey: "idx-other-req", Goal: "other"})
	if err != nil {
		t.Fatal(err)
	}
	targetReq, err := s.Create(ctx, Input{RequestKey: "idx-target-req", Goal: "target"})
	if err != nil {
		t.Fatal(err)
	}
	batch := make([]Step, 0, 260)
	for i := 0; i < 240; i++ {
		batch = append(batch, Step{
			RequestID:  otherReq.ID,
			EinoEpoch:  0,
			Key:        fmt.Sprintf("other-step-%d", i),
			Kind:       "eino_model",
			Stage:      "succeeded",
			InputJSON:  bigJSON,
			ResultJSON: bigJSON,
		})
	}
	for i := 0; i < 15; i++ {
		batch = append(batch, Step{
			RequestID:  targetReq.ID,
			EinoEpoch:  0,
			Key:        fmt.Sprintf("target-step-%d", i),
			Kind:       "eino_model",
			Stage:      "succeeded",
			InputJSON:  bigJSON,
			ResultJSON: bigJSON,
		})
	}
	batch = append(batch, Step{
		RequestID:  targetReq.ID,
		EinoEpoch:  1,
		Key:        "target-epoch1-step-0",
		Kind:       "eino_model",
		Stage:      "succeeded",
		InputJSON:  bigJSON,
		ResultJSON: bigJSON,
	})
	if err := s.DB.CreateInBatches(batch, 100).Error; err != nil {
		t.Fatal(err)
	}

	if err := s.DB.Create([]Step{
		{RequestID: otherReq.ID, EinoEpoch: 0, Key: "eino_resume:other-before", Kind: "eino_resume", Stage: "succeeded", ResultJSON: `{}`},
		{RequestID: targetReq.ID, EinoEpoch: 1, Key: "eino_resume:epoch1-before", Kind: "eino_resume", Stage: "succeeded", ResultJSON: `{}`},
	}).Error; err != nil {
		t.Fatal(err)
	}
	queryResumeID := func(reqID string, epoch uint64) uint64 {
		var id uint64
		if err := s.DB.WithContext(ctx).Model(&Step{}).Select("COALESCE(MAX(id), 0)").Where("request_id = ? AND eino_epoch = ? AND kind = ?", reqID, epoch, "eino_resume").Scan(&id).Error; err != nil {
			t.Fatal(err)
		}
		return id
	}
	if zeroID := queryResumeID(targetReq.ID, 0); zeroID != 0 {
		t.Fatalf("expected 0 when target request/epoch has no eino_resume, got %d", zeroID)
	}
	if err := s.Resume(ctx, targetReq.ID); err != nil {
		t.Fatal(err)
	}
	resumeID1 := queryResumeID(targetReq.ID, 0)
	if resumeID1 == 0 {
		t.Fatalf("expected non-zero eino_resume step ID after first Resume")
	}
	if err := s.Resume(ctx, targetReq.ID); err != nil {
		t.Fatal(err)
	}
	resumeID2 := queryResumeID(targetReq.ID, 0)
	if resumeID2 <= resumeID1 {
		t.Fatalf("expected second Resume to return newer ID > %d, got %d", resumeID1, resumeID2)
	}
	if err := s.DB.Create([]Step{
		{RequestID: otherReq.ID, EinoEpoch: 0, Key: "eino_resume:other-after", Kind: "eino_resume", Stage: "succeeded", ResultJSON: `{}`},
		{RequestID: targetReq.ID, EinoEpoch: 1, Key: "eino_resume:epoch1-after", Kind: "eino_resume", Stage: "succeeded", ResultJSON: `{}`},
	}).Error; err != nil {
		t.Fatal(err)
	}
	if resumeIDAfter := queryResumeID(targetReq.ID, 0); resumeIDAfter != resumeID2 {
		t.Fatalf("expected other request/epoch resumes not to affect query; want %d got %d", resumeID2, resumeIDAfter)
	}

	replayReq, err := s.Create(ctx, Input{RequestKey: "idx-replay-req", Goal: "replay"})
	if err != nil {
		t.Fatal(err)
	}
	savedChat := aiapp.ChatResponse{Message: aiapp.Message{Role: "assistant", ToolCalls: []aiapp.ToolCall{{ID: "tc-replay-1", Type: "function", Function: aiapp.FunctionCall{Name: "update_plan", Arguments: `{}`}}}}}
	if err := s.DB.Create(&Step{
		RequestID:  replayReq.ID,
		EinoEpoch:  0,
		Key:        "replay-model-1",
		Kind:       "eino_model",
		Stage:      "succeeded",
		ResultJSON: string(mustMarshal(savedChat)),
	}).Error; err != nil {
		t.Fatal(err)
	}
	h := &informationHost{service: s, owner: requestExecutionOwner{RequestID: replayReq.ID}, request: replayReq}
	var steps []Step
	err = h.modelSteps(ctx, "eino_model").Select("id, result_json").Where("stage = ?", "succeeded").Order("id").Find(&steps).Error
	if err != nil || len(steps) != 1 || !strings.Contains(steps[0].ResultJSON, "tc-replay-1") {
		t.Fatalf("expected model steps to preserve ResultJSON for replay, got len=%d err=%v", len(steps), err)
	}
}
