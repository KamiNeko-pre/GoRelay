package information

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/mcpserver"
	"go-lua-crawler/internal/testdb"
)

func TestMCPNativeSourceJobRunsWithoutModelAndRestoresReceipt(t *testing.T) {
	crawler := aiapp.NewService(testdb.Open(t))
	crawler.Fingerprint = "mcp-native-local-check"
	if err := crawler.Migrate(); err != nil {
		t.Fatal(err)
	}
	service := NewService(crawler)
	if err := service.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	backend := sourceBackend{service: service}
	for _, query := range []string{"", "unrelated topic"} {
		page, err := backend.FindSources(ctx, mcpserver.SourceQuery{Query: query})
		if err != nil || page.Total != 0 {
			t.Fatalf("empty source catalog: %+v %v", page, err)
		}
	}
	var gets atomic.Int32
	publicSource(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gets.Add(1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"name": "Observed API value"})
	}))
	goal := "Read a public name"
	fields := []aiapp.FieldRequirement{{Name: "name", Type: "string"}, {Name: "source_url", Type: "string"}}
	build, err := service.Create(ctx, Input{RequestKey: "prepare-native-source", Goal: goal, SourceScope: []string{"8.8.8.8"}})
	if err != nil {
		t.Fatal(err)
	}
	local, err := withLocalAcquisition(ctx, AcquisitionRequest{Goal: goal, Fields: fields, JoinKeys: []string{"name"}})
	if err != nil {
		t.Fatal(err)
	}
	trial, err := service.TrialAPI(local, build.ID, "Reusable public name", APIPlan{URL: "http://8.8.8.8/name", Fields: map[string]string{"name": "/name"}})
	if err != nil {
		t.Fatal(err)
	}
	asset, err := service.Publish(ctx, build.ID, trial.CandidateID)
	if err != nil {
		t.Fatal(err)
	}
	page, err := backend.FindSources(ctx, mcpserver.SourceQuery{Query: asset.SourceID})
	if err != nil || len(page.Sources) != 1 || !page.Sources[0].CanExecute {
		t.Fatalf("published source not discoverable: %+v %v", page, err)
	}
	input := mcpserver.CollectInput{Goal: goal, SourceID: asset.SourceID, Version: asset.Version, URL: "http://8.8.8.8/name"}
	job, err := backend.Collect(ctx, input)
	if err != nil || !strings.HasPrefix(job.JobID, nativeJobPrefix) || job.Terminal {
		t.Fatalf("native submission: %+v %v", job, err)
	}
	before := gets.Load()
	if _, err := backend.GetJob(ctx, mcpserver.JobInput{JobID: job.JobID}); err != nil || gets.Load() != before {
		t.Fatalf("status read executed acquisition: %v", err)
	}
	if err := service.Advance(ctx, strings.TrimPrefix(job.JobID, nativeJobPrefix)); err != nil {
		t.Fatal(err)
	}
	settled, err := backend.GetJob(ctx, mcpserver.JobInput{JobID: job.JobID})
	if err != nil || !settled.Terminal || settled.Status != "succeeded" || settled.RecordsSaved != 1 || gets.Load() != before+1 {
		t.Fatalf("native result: %+v %v", settled, err)
	}
	data, err := backend.ReadData(ctx, mcpserver.DataInput{JobID: job.JobID})
	if err != nil || len(data.Records) != 1 || data.Records[0].Fields["name"] != "Observed API value" || len(data.Records[0].Documents) == 0 {
		t.Fatalf("native saved data or capture references lost: %+v %v", data, err)
	}
	var cited bool
	for _, doc := range data.Records[0].Documents {
		if doc.Origin != "source_response" {
			continue
		}
		evidence, err := backend.ReadEvidence(ctx, mcpserver.EvidenceIDs{DocumentID: doc.ID, Revision: doc.Revision})
		if err != nil || len(evidence.Entries) == 0 || !strings.Contains(evidence.Entries[0].Text, "Observed API value") {
			t.Fatalf("native capture cannot support a citation: %+v %v", evidence, err)
		}
		cited = true
	}
	if !cited {
		t.Fatal("only generated record text was available as evidence")
	}
	duplicate, err := backend.Collect(ctx, input)
	var failure *mcpserver.ToolFailure
	if !errors.As(err, &failure) || failure.Code != "ACTION_ALREADY_CALLED" || duplicate.JobID != job.JobID || gets.Load() != before+1 {
		t.Fatalf("duplicate native submission: %+v %v", duplicate, err)
	}
	id := strings.TrimPrefix(job.JobID, nativeJobPrefix)
	if err := service.DB.Model(&Request{}).Where("id = ?", id).Updates(map[string]any{"stage": "mcp_queued", "result_json": ""}).Error; err != nil {
		t.Fatal(err)
	}
	restarted := NewService(crawler)
	if err := restarted.Advance(ctx, id); err != nil || gets.Load() != before+1 {
		t.Fatalf("receipt recovery repeated the original HTTP action: %v", err)
	}
	input.Params = map[string]any{"page": 2}
	changed, err := backend.Collect(ctx, input)
	if err != nil || changed.JobID == job.JobID {
		t.Fatalf("real parameter change was rejected: %+v %v", changed, err)
	}
	cancelled, err := backend.CancelJob(ctx, mcpserver.JobInput{JobID: changed.JobID})
	if err != nil || cancelled.Status != "cancelled" || !cancelled.Terminal {
		t.Fatalf("native cancellation: %+v %v", cancelled, err)
	}
	if err := restarted.Advance(ctx, strings.TrimPrefix(changed.JobID, nativeJobPrefix)); err != nil || gets.Load() != before+1 {
		t.Fatalf("cancelled work fetched again: %v", err)
	}
	if crawler.Model != nil || service.Model != nil {
		t.Fatal("native MCP execution configured an internal model")
	}
	// A real user update is a new observation; retries and read-only requests
	// inside that update must not repeat the external acquisition.
	countBeforeRefresh := gets.Load()
	refreshInput := sourceRefreshInput{PreviousJobID: settled.JobID, RequestKey: "explicit-update-1"}
	refresh, err := backend.refreshSource(ctx, refreshInput)
	if err != nil || refresh.Job.JobID == settled.JobID || refresh.Refresh.ID == "" {
		t.Fatalf("explicit update did not create a new job: %+v %v", refresh, err)
	}
	retry, err := backend.refreshSource(ctx, refreshInput)
	if err != nil || retry.Job.JobID != refresh.Job.JobID {
		t.Fatalf("same user update was submitted twice: %+v %v", retry, err)
	}
	inFlight, err := backend.refreshSource(ctx, sourceRefreshInput{PreviousJobID: settled.JobID, RequestKey: "double-click-while-running"})
	if err != nil || inFlight.Job.JobID != refresh.Job.JobID || gets.Load() != countBeforeRefresh {
		t.Fatalf("in-flight update was repeated: %+v %v", inFlight, err)
	}
	if err := service.Advance(ctx, strings.TrimPrefix(refresh.Job.JobID, nativeJobPrefix)); err != nil {
		t.Fatal(err)
	}
	diff, err := backend.sourceDifference(ctx, settled.JobID, refresh.Job.JobID)
	if err != nil || diff.Notice != "" || len(diff.Changes) != 0 || gets.Load() != countBeforeRefresh+1 {
		t.Fatalf("read-only comparison repeated acquisition or lost unchanged observation: %+v %v", diff, err)
	}
	for cursor, count := "", 0; ; {
		page, err := backend.listSourceJobs(ctx, asset.SourceID, cursor, 1)
		if err != nil || len(page.Jobs) != 1 {
			t.Fatalf("source history page: %+v %v", page, err)
		}
		count++
		if page.NextCursor == "" {
			if count != page.Total || count < 3 {
				t.Fatalf("source history truncated: %d / %d", count, page.Total)
			}
			break
		}
		cursor = page.NextCursor
	}
	next, err := backend.refreshSource(ctx, sourceRefreshInput{PreviousJobID: refresh.Job.JobID, RequestKey: "explicit-update-2"})
	if err != nil || next.Job.JobID == refresh.Job.JobID || next.Refresh.ID == refresh.Refresh.ID {
		t.Fatalf("next explicit update did not create a new observation: %+v %v", next, err)
	}
	if _, err := backend.CancelJob(ctx, mcpserver.JobInput{JobID: next.Job.JobID}); err != nil {
		t.Fatal(err)
	}
}
