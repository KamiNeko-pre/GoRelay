package aiapp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/engine"
	"go-lua-crawler/internal/testdb"
)

type admissionCountingTransport struct {
	http.RoundTripper
	Calls atomic.Int64
}

func (t *admissionCountingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.Calls.Add(1)
	return t.RoundTripper.RoundTrip(r)
}

func countActualRunnerAdmission(t *testing.T, s *Service) *atomic.Int64 {
	t.Helper()
	runner := actualRunnerForChain(t)
	base := runner.client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	transport := &admissionCountingTransport{RoundTripper: base}
	runner.client.Transport = transport
	s.Runner = runner
	return &transport.Calls
}

func successfulRepairParent(t *testing.T, s *Service, run agentdata.Run, id, name string) {
	t.Helper()
	out, _ := toolData(ToolCall{ID: id, Function: FunctionCall{Name: name}}, map[string]any{"accepted": true})
	if err := s.Data.DB.Create(&agentdata.Operation{RunID: run.ID, Key: "tool:" + id, Kind: name, Stage: "succeeded", InputHash: "previous-different-business-input", ResultJSON: string(mustJSON(out))}).Error; err != nil {
		t.Fatal(err)
	}
}

func TestCapturedRepairMetadataCannotConsumeFirstProgramExecution(t *testing.T) {
	raw, err := os.ReadFile("testdata/repair-metadata-admission-20261016.json")
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Call ToolCall `json:"call"`
		Spec TaskSpec `json:"run_spec"`
	}
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	// Preserve the exact captured code/arguments. Controlled HTTP prevents its
	// source URLs from escaping this local admission reproduction.
	probeFeedbackProxy(t, `<main><h2>No region links in this local boundary check</h2></main>`)
	s, run, _ := storedHTMLReader(t, `<h2>source</h2>`)
	run.SpecJSON = string(mustJSON(saved.Spec))
	if err := s.Data.DB.Model(&agentdata.Run{}).Where("id = ?", run.ID).Update("spec_json", run.SpecJSON).Error; err != nil {
		t.Fatal(err)
	}
	run, err = s.Data.Claim(context.Background(), run.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	calls := countActualRunnerAdmission(t, s)
	var args map[string]any
	json.Unmarshal([]byte(saved.Call.Function.Arguments), &args)
	parent, _ := args["repair_of"].(string)
	if parent == "" {
		t.Fatal("captured reproduction lost its invalid metadata")
	}
	successfulRepairParent(t, s, run, parent, "validate_program")
	bad := saved.Call
	out, err := s.executeDurableTool(context.Background(), run, bad)
	if err == nil || out.Failure == nil || failureField(out.Failure) != "repair_of" || calls.Load() != 0 {
		t.Fatal("expected protocol rejection before runner admission", out, err, calls.Load())
	}
	if !strings.Contains(strings.ToLower(out.Failure.Recovery.Instruction), "omit") {
		t.Error("metadata diagnostic never says which argument can be omitted", out.Failure.Recovery)
	}
	bad.ID = "repeat-illegal-metadata"
	if repeated, e := s.executeDurableTool(context.Background(), run, bad); e == nil || repeated.Failure.Code != "DUPLICATE_TOOL_CALL" || calls.Load() != 0 {
		t.Fatal("identical invalid protocol proposal was executed", repeated, e)
	}
	delete(args, "repair_of")
	fixed := saved.Call
	fixed.ID = "correct-protocol-once"
	fixed.Function.Arguments = string(mustJSON(args))
	out, err = s.executeDurableTool(context.Background(), run, fixed)
	if calls.Load() != 1 || out.Failure != nil && out.Failure.Code == "DUPLICATE_TOOL_CALL" {
		t.Fatal("corrected optional metadata is blocked before its FIRST business execution", out, err, calls.Load())
	}
	// This fixture is only an admission replay, not a successful crawl. Its
	// exact source code must reach the real runner once, even if output fails.
	args["repair_of"] = saved.Call.ID
	fixed.ID = "metadata-cannot-authorize-second-execution"
	fixed.Function.Arguments = string(mustJSON(args))
	if duplicate, e := s.executeDurableTool(context.Background(), run, fixed); e == nil || duplicate.Failure.Code != "DUPLICATE_TOOL_CALL" || calls.Load() != 1 {
		t.Fatal("metadata retried an already admitted program", duplicate, e, calls.Load())
	}
	ops, _ := s.Data.Operations(context.Background(), run.ID)
	for _, failure := range unresolvedToolFailures(LoopState{Run: run, Operations: ops}) {
		if failure.Failure != nil && failureField(failure.Failure) == "repair_of" {
			t.Fatal("fixed protocol metadata remains the pending program diagnosis", failure)
		}
	}
}

// Replay the actual native repair, without editing its Lua or supplying any
// selectors. Unlike an admission-only empty source, this fixture must yield all
// twelve heterogeneous details and a publishable adapter. It is saved-proposal
// replay, not proof that a new model autonomously completes the task.
func TestCapturedProtocolCorrectionRestoresCompleteAcquisition(t *testing.T) {
	raw, err := os.ReadFile("testdata/repair-metadata-admission-20261016.json")
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Call ToolCall `json:"call"`
	}
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	fixture := harderCrawlerCases()[0]
	probeFeedbackSource(t, func(w http.ResponseWriter, r *http.Request) {
		body, ok := fixture.Pages[r.URL.RequestURI()]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, body)
	})
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	fields := []FieldRequirement{}
	for _, field := range fixture.Fields {
		fields = append(fields, FieldRequirement{Name: field, Type: "string"})
	}
	spec := TaskSpec{Goal: fixture.Goal, ContractVersion: "v2", SchemaVersion: "v1", RequiredFields: fields, JoinKeys: []string{fixture.Fields[0]}, MinRecords: len(fixture.Gold), RequireFieldSupport: true, RequirePublishedCapability: true, SourceScope: []string{"8.8.8.8"}}
	run, err := s.Create(context.Background(), CreateRunRequest{RequestKey: "captured-native-correction", Goal: spec.Goal, Spec: &spec, SourceScope: spec.SourceScope})
	if err != nil {
		t.Fatal(err)
	}
	run, err = s.Data.Claim(context.Background(), run.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	calls := countActualRunnerAdmission(t, s)
	var args map[string]any
	json.Unmarshal([]byte(saved.Call.Function.Arguments), &args)
	parent, _ := args["repair_of"].(string)
	successfulRepairParent(t, s, run, parent, saved.Call.Function.Name)
	bad, err := s.executeDurableTool(context.Background(), run, saved.Call)
	if err == nil || bad.Failure == nil || failureField(bad.Failure) != "repair_of" || calls.Load() != 0 {
		t.Fatal("captured invalid metadata was not rejected before business admission", bad, err)
	}
	delete(args, "repair_of")
	fixed := saved.Call
	fixed.ID = "corrected-native-program"
	fixed.Function.Arguments = string(mustJSON(args))
	out, err := s.executeDurableTool(context.Background(), run, fixed)
	if err != nil || calls.Load() != 1 {
		t.Fatal("following the metadata diagnosis cannot advance the actual repaired program", out, err, calls.Load())
	}
	var validation struct {
		CandidateID string `json:"candidate_id"`
		Next        struct {
			Tool      string          `json:"tool"`
			Arguments json.RawMessage `json:"arguments"`
			Submit    json.RawMessage `json:"then_submit"`
		} `json:"next_action"`
	}
	if err := json.Unmarshal(out.Data, &validation); err != nil || validation.Next.Tool != "register_source" {
		t.Fatal("complete acquisition has no executable publication continuation", string(out.Data), err)
	}
	var candidate Candidate
	if err := s.Data.DB.First(&candidate, "id = ?", validation.CandidateID).Error; err != nil {
		t.Fatal(err)
	}
	var result engine.ProgramResult
	if err := json.Unmarshal([]byte(candidate.ResultJSON), &result); err != nil {
		t.Fatal(err)
	}
	records, err := BindRecords(result, RecordBinding{SourceID: candidate.SourceID, DataVersion: candidate.ArtifactHash, TaskID: 1, SchemaVersion: spec.SchemaVersion, KeyFields: spec.JoinKeys})
	if err != nil || len(records) != len(fixture.Gold) {
		t.Fatal("native repair still fails the twelve-detail task", len(records), err)
	}
	seen := map[string]bool{}
	for _, record := range records {
		url, _ := record.Fields["source_url"].(string)
		want := fixture.Gold[url]
		if want == nil || seen[url] {
			t.Fatal("unknown or duplicated native record", url)
		}
		seen[url] = true
		for field, value := range want {
			if record.Fields[field] != value {
				t.Fatalf("native repair field=%s got=%v want=%s", field, record.Fields[field], value)
			}
		}
	}
	publication, err := s.executeDurableTool(context.Background(), run, ToolCall{ID: "publish-corrected-native-program", Type: "function", Function: FunctionCall{Name: validation.Next.Tool, Arguments: string(validation.Next.Arguments)}})
	if err != nil {
		t.Fatal("corrected native acquisition cannot publish", publication, err)
	}
	var published struct {
		Next struct {
			Tool      string          `json:"tool"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"next_action"`
	}
	json.Unmarshal(publication.Data, &published)
	if published.Next.Tool != "submit_acquisition" || agentdata.Hash(published.Next.Arguments) != agentdata.Hash(validation.Next.Submit) {
		t.Fatal("publication lost the validated submission continuation", string(publication.Data))
	}
	auditSave(t, "captured-protocol-correction-to-output", map[string]any{"new_model_calls": 0, "unchanged_native_code": true, "records": len(records), "fields_per_record": len(fixture.Fields), "runner_executions": calls.Load(), "publication": publication, "records_and_field_values_checked": true, "task_completion_claimed": false})
}

func TestRepairMetadataDoesNotPreventBusinessReferenceNormalization(t *testing.T) {
	page := inspectResponse(acquisition.Response{Body: `<h2 id="heading">Observed title</h2>`, Receipt: acquisition.Receipt{Method: "GET", URL: "https://fixture.example/docs", Status: 200, FetchedAt: time.Now()}}, acquisition.Stats{})
	out, _ := toolData(ToolCall{ID: "source", Function: FunctionCall{Name: "inspect_source"}}, page)
	ops := []agentdata.Operation{{ID: 1, Kind: "inspect_source", Stage: "succeeded", Key: "tool:source", ResultJSON: string(mustJSON(out))}}
	a := FunctionCall{Name: "read_data", Arguments: `{"handle":"source","view":"dom","selector":"#heading"}`}
	b := FunctionCall{Name: "read_data", Arguments: `{"handle":"source","view":"dom","selector":"#heading","repair_of":"not-a-business-parameter"}`}
	if businessToolInput(a, ops) != businessToolInput(b, ops) {
		t.Fatal("protocol metadata changed canonical source/default identities before argument decoding")
	}
}

func TestProtocolRejectionAndFirstValidReadHaveSeparateAdmission(t *testing.T) {
	for _, legacy := range []string{"none", "valid-failed-reference", "business-key", "raw-canonical-key"} {
		t.Run(legacy, func(t *testing.T) {
			s, run, _ := storedHTMLReader(t, `<main><h2 id="heading">Observed title</h2></main>`)
			run, err := s.Data.Claim(context.Background(), run.ID, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			bad := ToolCall{ID: "bad-read-metadata", Type: "function", Function: FunctionCall{Name: "read_data", Arguments: `{"handle":"source","view":"dom","selector":"#heading","repair_of":"source"}`}}
			model := modelRound("original-protocol-proposal", bad)
			model.RunID = run.ID
			if err := s.Data.DB.Create(&model).Error; err != nil {
				t.Fatal(err)
			}
			out, err := s.executeDurableTool(context.Background(), run, bad)
			if err == nil || out.Failure == nil || failureField(out.Failure) != "repair_of" {
				t.Fatal("wrong-tool/successful reference was not rejected", out, err)
			}
			valid := bad
			valid.ID = "read-without-optional-metadata"
			valid.Function.Arguments = `{"handle":"source","view":"dom","selector":"#heading"}`
			if legacy == "valid-failed-reference" {
				valid.Function.Arguments = `{"handle":"source","view":"dom","selector":"#heading","repair_of":"bad-read-metadata"}`
			}
			if legacy != "none" && legacy != "valid-failed-reference" {
				// An older persisted protocol rejection used the business input hash.
				ops, _ := s.Data.Operations(context.Background(), run.ID)
				input := businessToolInput(valid.Function, ops)
				if legacy == "raw-canonical-key" {
					input = canonicalToolInput(bad.Function)
				}
				if err := s.Data.DB.Model(&agentdata.Operation{}).Where("run_id = ? AND `key` = ?", run.ID, "tool:"+bad.ID).Update("input_hash", input).Error; err != nil {
					t.Fatal(err)
				}
			}
			out, err = s.executeDurableTool(context.Background(), run, valid)
			if err != nil || !strings.Contains(string(out.Data), "Observed title") {
				t.Fatal("metadata removal could not perform the first valid read", out, err)
			}
			valid.ID = "renamed-read"
			if duplicate, e := s.executeDurableTool(context.Background(), run, valid); e == nil || duplicate.Failure.Code != "DUPLICATE_TOOL_CALL" {
				t.Fatal("a valid read repeated through another call ID", duplicate, e)
			}
			bad.ID = "invalid-metadata-after-business-admission"
			if duplicate, e := s.executeDurableTool(context.Background(), run, bad); e == nil || duplicate.Failure.Code != "DUPLICATE_TOOL_CALL" {
				t.Fatal("invalid protocol metadata hid an already admitted business action", duplicate, e)
			}
			ops, _ := s.Data.Operations(context.Background(), run.ID)
			for _, failure := range unresolvedToolFailures(LoopState{Run: run, Operations: ops}) {
				if failure.Failure != nil && failureField(failure.Failure) == "repair_of" {
					t.Fatal("resolved metadata failure kept guiding the next model", failure)
				}
			}
		})
	}
}
