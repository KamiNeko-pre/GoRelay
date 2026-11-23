package information

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"

	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/agentstatus"
	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/engine"
	"go-lua-crawler/internal/knowledge"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type AssetTrial struct {
	ID          string    `json:"id" gorm:"primaryKey;size:64"`
	RequestID   string    `json:"request_id" gorm:"size:32;index"`
	AssetJSON   string    `json:"-" gorm:"type:longtext"`
	RuntimeJSON string    `json:"-" gorm:"type:longtext"`
	CreatedAt   time.Time `json:"created_at"`
}

func (AssetTrial) TableName() string { return "information_asset_trials" }

const acquisitionSampleSize = 5

type calculationSampleEvidence struct {
	RowIndex int                                `json:"row_index"`
	Fields   map[string]JSONCalculationEvidence `json:"fields"`
}

type TrialResult struct {
	SampleEvidence []calculationSampleEvidence `json:"sample_evidence,omitempty"`
	References     []StoredReference           `json:"references,omitempty"`
	Observation    *sourceCheckReceipt         `json:"observation,omitempty"`
	CandidateID    string                      `json:"candidate_id"`
	Asset          Asset                       `json:"asset"`
	Coverage       aiapp.CoverageReport        `json:"coverage"`
	Records        []aiapp.Record              `json:"records"`
}

func acquisitionTrialSummary(trial TrialResult) map[string]any {
	summary := map[string]any{
		"status": "captured", "candidate_id": trial.CandidateID,
		"record_count": len(trial.Records), "sample_items": compactAPISample(trial.Records, acquisitionSampleSize),
		"source_check": trial.Observation, "references": trial.References,
	}
	if len(trial.SampleEvidence) > 0 {
		summary["sample_evidence"] = trial.SampleEvidence
	}
	return summary
}

func sampleEvidenceFor(rows []aiapp.Record, refs map[string]StoredReference, previewCount int) []calculationSampleEvidence {
	var out []calculationSampleEvidence
	for i, row := range rows[:min(len(rows), previewCount)] {
		if row.Provenance.ResponseIndex == 0 {
			continue
		}
		ref, ok := refs[acquisitionResponseKey(row.Provenance.ResponseIndex-1)]
		if !ok {
			continue
		}
		fields := map[string]JSONCalculationEvidence{}
		for field, support := range row.Provenance.FieldSupport {
			pointer, ok := support.ActualLocator.(string)
			if !ok {
				continue
			}
			if _, ok := deliveryJSONNumber(row.Fields[field]); !ok {
				continue
			}
			fields[field] = JSONCalculationEvidence{SourceRef: luaSourceRef{DocID: ref.DocID, Revision: ref.Revision}, JSONPointer: pointer}
		}
		if len(fields) > 0 {
			out = append(out, calculationSampleEvidence{RowIndex: i, Fields: fields})
		}
	}
	return out
}

type ActionError struct {
	Code        string         `json:"code"`
	Message     string         `json:"message"`
	Steps       []string       `json:"steps"`
	PriorStepID uint64         `json:"prior_step_id,omitempty"`
	Cause       error          `json:"-"`
	Details     map[string]any `json:"details,omitempty"`
}

func (e *ActionError) Error() string { return e.Code + ": " + e.Message }
func (e *ActionError) ToolFeedback() (string, []string, map[string]any) {
	return e.Code, e.Steps, e.Details
}
func (e *ActionError) Unwrap() error { return e.Cause }
func (e *ActionError) OperationalStatus() agentstatus.Status {
	s := aiapp.OperationalStatus(e.Cause)
	switch e.Code {
	case "ACTION_ALREADY_CALLED", "ACTION_OUTCOME_UNKNOWN":
		s = agentstatus.Of(agentstatus.AgentDuplicate)
	case "WORK_INVESTIGATION_INCOMPLETE", "WORK_DELIVERY_UNSUPPORTED", "WORK_PLAN_INVALID", "WORK_SCOPE_UNSUPPORTED", "WORK_SOURCE_NOT_PENDING", "WORK_RULES_UNDECLARED", "WORK_CONSTRAINT_CHANGED", "WORK_COVERAGE_INCOMPLETE", "ROUTING_ARGUMENTS_INVALID", "ASSET_SCOPE_INVALID", "LOCAL_LOOKUP_REQUIRED", "LUA_FAILURE_REPORT_INVALID", "BROWSER_SOURCE_REFERENCE_INVALID", "READ_SNAPSHOT_INVALID", "SCHEMA_REVISION_INVALID", "DISCOVERY_BOOTSTRAP_DEPENDENCY", "DISCOVERY_CAPABILITY_FAILED", "DISCOVERY_RECOVERY_INVALID", "ANSWER_EVIDENCE_REQUIRED", "SOURCE_URLS_REQUIRED", "CHILD_NOT_CREATED", "CHILD_ALREADY_RUNNING", "ACQUISITION_FAILED", "ASSET_NOT_ELIGIBLE", "ASSET_CANDIDATE_INVALID", "UNKNOWN_TOOL", "DELIVERY_INCOMPLETE", "TABLE_VALUE_UNSUPPORTED", "OBSERVATION_NOT_LINKED", "REQUIREMENTS_CHANGED", "DELIVERY_EVIDENCE_MISSING", "INVESTIGATION_EVIDENCE_MISSING", "UPDATE_ACQUISITION_REQUIRED", "EXPLICIT_INPUT_IMMUTABLE", "REQUIREMENT_NOT_FOUND", "INVALID_OUTPUT_REQUIREMENT":
		s = agentstatus.Of(agentstatus.AgentCorrection)
	case "BROWSER_UNAVAILABLE", "ROUTING_MODEL_UNAVAILABLE":
		s = agentstatus.Of(agentstatus.SystemConfiguration)
	case "ROUTING_OUTPUT_INCOMPLETE":
		s = agentstatus.Of(agentstatus.SystemDependency)
	case "ASSET_TRIAL_FAILED", "ASSET_EXECUTION_FAILED":
		if s.Code == agentstatus.Unclassified {
			s = agentstatus.Of(agentstatus.AgentCorrection)
		}
	case "LUA_FAILURE_RECORDED":
		s = agentstatus.Of(agentstatus.AgentExhausted)
	default:
		if s.Code == agentstatus.Unclassified && len(e.Steps) > 0 {
			s = agentstatus.Of(agentstatus.AgentCorrection)
		}
	}
	s.Recovery = append([]string{}, e.Steps...)
	return s
}
func actionFailure(code string, cause error, steps ...string) *ActionError {
	return &ActionError{Code: code, Message: cause.Error(), Steps: steps, Cause: cause}
}

func (s *Service) trialRequest(ctx context.Context, id string) (Request, Input, aiapp.TaskSpec, error) {
	r, err := s.Request(ctx, id)
	if err != nil {
		return r, Input{}, aiapp.TaskSpec{}, err
	}
	var in Input
	json.Unmarshal([]byte(r.InputJSON), &in)
	if !r.Deadline.IsZero() && time.Now().After(r.Deadline) {
		return r, in, aiapp.TaskSpec{}, fmt.Errorf("information request deadline reached")
	}
	spec, err := localAcquisition(ctx)
	if err != nil && r.Purpose == nativeSourcePurpose {
		var job nativeSourceJob
		if e := json.Unmarshal([]byte(r.NativeJobJSON), &job); e != nil {
			return r, in, spec, e
		}
		ctx, err = withLocalAcquisition(ctx, job.Acquisition)
		if err == nil {
			spec, err = localAcquisition(ctx)
		}
	}
	if err != nil {
		return r, in, spec, err
	}
	return r, in, spec, nil
}

func duplicateStep(step Step) *ActionError {
	code := "ACTION_ALREADY_CALLED"
	if step.Stage == "reserved" {
		code = "ACTION_OUTCOME_UNKNOWN"
	}
	return &ActionError{Code: code, Message: "same action and effective business parameters were already admitted; changing call metadata cannot repeat execution", PriorStepID: step.ID, Details: map[string]any{"previous_status": step.Stage, "previous_error": step.Error, "previous_result": domainReceiptObservation(step.ResultJSON)}, Steps: []string{"Read details.previous_result and details.previous_error; they are the saved receipt, with no second execution.", "Change a real URL, field pointer or acquisition plan only when supported by new source evidence; otherwise use a different applicable asset."}}
}

func domainReceiptObservation(raw string) map[string]any {
	var saved map[string]any
	if json.Unmarshal([]byte(raw), &saved) != nil {
		return nil
	}
	out := map[string]any{}
	for _, key := range []string{"candidate_id", "asset", "records", "coverage", "observation", "structure"} {
		if v, ok := saved[key]; ok {
			out[key] = v
		}
	}
	if runtime, ok := saved["runtime"].(map[string]any); ok {
		view := map[string]any{}
		for _, key := range []string{"requests", "stats", "execution_diagnostics", "execution_observations"} {
			if v, ok := runtime[key]; ok {
				view[key] = v
			}
		}
		out["runtime"] = view
	}
	return out
}

func (s *Service) TrialAPI(ctx context.Context, id, description string, plan APIPlan) (TrialResult, error) {
	normalizedPlan, normErr := normalizeAPIPlan(plan)
	if normErr != nil {
		return TrialResult{}, actionFailure("ROUTING_ARGUMENTS_INVALID", normErr, "Correct API JSON pointers (e.g. '/version' or '/items') using JSON keys from inspect_url or the API response.")
	}
	plan = normalizedPlan
	r, in, spec, err := s.trialRequest(ctx, id)
	if err != nil {
		return TrialResult{}, err
	}
	if !urlInScope(plan.URL, in.SourceScope) {
		return TrialResult{}, actionFailure("ASSET_SCOPE_INVALID", fmt.Errorf("API URL is outside the original scope or malformed"), "Choose a public HTTP(S) endpoint inside the granted source scope.")
	}
	input := map[string]any{"api": plan, "scope": in.SourceScope, "spec": spec}
	step, fresh, err := s.reserveStep(ctx, r, "trial_api", input)
	if err != nil {
		return TrialResult{}, err
	}
	if !fresh {
		return TrialResult{}, duplicateStep(step)
	}
	ctx, cancel := r.RequestContext(ctx)
	defer cancel()
	fetcher, err := s.NewFetcher(in.SourceScope)
	if err != nil {
		err = actionFailure("ACQUISITION_FETCHER_UNAVAILABLE", err, "Verify source_scope domain rules or inspect URL configuration.")
		s.finishStep(context.WithoutCancel(ctx), step, nil, err)
		return TrialResult{}, err
	}
	defer fetcher.Close()
	snapshots := acquisitionSnapshots{responses: map[string]acquisition.Response{}, documents: map[string]knowledge.Document{}}
	checkedKeys := map[string]bool{}
	runtime, cause := executeAPIWithRequester(ctx, plan, func(ctx context.Context, opts acquisition.RequestOptions) (acquisition.Response, error) {
		var missing []string
		for _, key := range fetcher.SnapshotCandidates(ctx, opts) {
			if !checkedKeys[key] {
				missing = append(missing, key)
			}
		}
		if len(missing) > 0 {
			loaded, err := s.loadAcquisitionSnapshots(ctx, r.ID, in.SourceScope, missing)
			if err != nil {
				return acquisition.Response{}, err
			}
			for _, key := range missing {
				checkedKeys[key] = true
			}
			for k, v := range loaded.responses {
				snapshots.responses[k] = v
			}
			for k, d := range loaded.documents {
				snapshots.documents[k] = d
			}
		}
		return fetcher.FetchWithSnapshots(ctx, opts, snapshots.responses)
	})
	runtime.Stats = fetcher.Stats()
	asset := Asset{Kind: "api", SourceID: "api-" + agentdata.Hash(plan)[:24], Version: agentdata.Hash(input), Description: description, EntryURL: plan.URL, SpecJSON: string(mustMarshal(spec)), ScopeJSON: string(mustMarshal(in.SourceScope)), ManifestJSON: string(mustMarshal(plan)), VerifiedAt: time.Now()}
	asset.ID = agentdata.Hash([]string{asset.Kind, asset.SourceID, asset.Version})
	saved := map[int]knowledge.Document{}
	for i, resp := range runtime.SourceEvidence {
		snapshots.bind(resp, saved, i)
	}
	out, err := s.saveTrial(ctx, r, step, asset, runtime, cause, saved)
	return out, err
}

func enrichAcquisitionFailureDetails(failure *ActionError, coverage *aiapp.CoverageReport, runtime engine.ProgramResult, savedObs []Observation) {
	if failure.Details == nil {
		failure.Details = map[string]any{}
	}
	if coverage != nil {
		failure.Details["coverage"] = *coverage
	}
	failure.Details["runtime"] = map[string]any{
		"requests":     runtime.Requests,
		"diagnostics":  runtime.ExecutionDiagnostics,
		"observations": runtime.ExecutionObservations,
		"stats":        runtime.Stats,
	}
	if len(runtime.SourceEvidence) > 0 {
		sources := make([]map[string]any, 0, len(runtime.SourceEvidence))
		for _, source := range runtime.SourceEvidence {
			sources = append(sources, acquisitionResponseMetadata(source))
		}
		failure.Details["sources"] = sources
	}
	if len(savedObs) > 0 {
		failure.Details["saved_observation_count"] = len(savedObs)
		failure.Details["saved_observations"] = compactObservationPreviews(savedObs, 1200)
	}
}

func (s *Service) saveTrial(ctx context.Context, r Request, step Step, a Asset, runtime engine.ProgramResult, cause error, saved ...map[int]knowledge.Document) (TrialResult, error) {
	out := TrialResult{Asset: a}
	if cause == nil {
		out.Observation = emptyAssetObservation(a, sourceCheckID(step), runtime)
	}
	if runtime.Collection != nil || cause == nil {
		var bindErr error
		out.Records, bindErr = nativeAcquisitionRecords(a, step, runtime)
		cause = errors.Join(cause, bindErr)
	}
	var outcomeError error
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := requireNativeCommitOwner(tx, ctx, r, step); err != nil {
			return err
		}
		scoped := *s
		scoped.DB = tx
		savedObs, err := scoped.ingestAcquiredRecords(ctx, r.ID, step.ID, out.Records, runtime.SourceEvidence, saved...)
		if err != nil {
			return err
		}
		out.References = make([]StoredReference, 0, len(runtime.SourceEvidence))
		var documents []taskDocument
		if err := tx.Where("request_id = ? AND receipt_id = ?", r.ID, step.ID).Order("response_key").Find(&documents).Error; err != nil {
			return err
		}
		refs := map[string]StoredReference{}
		for _, doc := range documents {
			if isValidDocumentForCompleted(doc) {
				ref := StoredReference{DocID: doc.DocID, Revision: doc.Revision}
				out.References = append(out.References, ref)
				refs[doc.ResponseKey] = ref
			}
		}
		if a.Kind == "api" {
			out.SampleEvidence = sampleEvidenceFor(out.Records, refs, acquisitionSampleSize)
		}
		if cause == nil && out.Observation != nil {
			return scoped.finishStep(ctx, step, out, nil)
		}
		if cause == nil {
			// Lightweight sampling check: verify non-empty records with valid scalar content
			// instead of enforcing full provenance contracts on every item upfront.
			validCount := 0
			for _, rec := range out.Records {
				for k, v := range rec.Fields {
					if k != "source_url" && v != nil && fmt.Sprint(v) != "" {
						validCount++
						break
					}
				}
			}
			if validCount == 0 {
				cause = fmt.Errorf("acquisition yielded 0 records with non-empty extracted fields")
			} else {
				out.Coverage = aiapp.CoverageReport{ValidRecords: validCount, Satisfied: true}
			}
		}
		if cause != nil {
			failure := actionFailure("ASSET_TRIAL_FAILED", cause, "Inspect details.sources and runtime receipts, then fix field pointers or selectors without repeating unchanged parameters.")
			failure.PriorStepID = step.ID
			enrichAcquisitionFailureDetails(failure, &out.Coverage, runtime, savedObs)
			if a.Kind == "browser" {
				failure.Steps = []string{
					"Read the renderer error and details.runtime HTTP receipts for the failed page.",
					"If the extraction locator is wrong, correct the specific rows_selector or field named in the error. Use an observed empty-state marker only when it applies.",
					"Keep source scope and Browser authorization; if details.saved_observations already satisfy your task, submit_delivery directly.",
				}
			}
			if a.Kind == "api" {
				failure.Details["execution_contract"] = nativeAPIExecutionContract
				if len(savedObs) > 0 {
					failure.Steps = []string{
						"Note: valid scalar records from this response were already saved (see details.saved_observations). If they satisfy the task, call submit_delivery directly with their observation_ids.",
						"Otherwise use details.sources and the reported JSON Pointer error to fix api.items_pointer or api.fields. Use Lua if the response is HTML.",
					}
				} else {
					failure.Steps = []string{
						"Read the actual root or item keys in details.sources together with the failing JSON pointer.",
						"Correct api.items_pointer and api.fields pointers (or use acquire_sources(urls=[...]) when reusable asset publication is unnecessary).",
					}
				}
				var approved []string
				var scope []string
				_ = json.Unmarshal([]byte(a.ScopeJSON), &scope)
				for _, capture := range runtime.SourceEvidence {
					if url, ok := scoped.maybeApproveBrowserFromFetch(ctx, &r, step, scope, capture); ok {
						approved = append(approved, url)
					}
				}
				if len(approved) > 0 {
					failure.Details["browser_approved_urls"] = approved
					failure.Steps = []string{"Use browser_extract for the approved source, or continue with another applicable source."}
				}
			}
			outcomeError = failure
			return scoped.finishStep(ctx, step, map[string]any{"records": compactObservationPreviews(savedObs, 1200), "references": out.References}, failure)
		}
		if step.CallID != "" {
			return scoped.finishStep(ctx, step, out, nil)
		}
		out.CandidateID = agentdata.Hash([]any{r.ID, a.ID, step.InputHash})
		proof := map[string]any{"candidate_id": out.CandidateID, "step_id": step.ID, "coverage": out.Coverage, "runtime_hash": agentdata.Hash(runtime)}
		if a.Kind == "browser" {
			proof["browser_approval_request"] = r.ID
		}
		a.ProofJSON = string(mustMarshal(proof))
		out.Asset = a
		trial := AssetTrial{ID: out.CandidateID, RequestID: r.ID, AssetJSON: string(mustMarshal(a)), RuntimeJSON: string(mustMarshal(runtime))}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&trial).Error; err != nil {
			return err
		}
		return scoped.finishStep(ctx, step, out, nil)
	})
	if err != nil {
		return out, err
	}
	return out, outcomeError
}

func (s *Service) Publish(ctx context.Context, requestID, candidateID string) (Asset, error) {
	var trial AssetTrial
	if err := s.DB.WithContext(ctx).First(&trial, "id = ? AND request_id = ?", candidateID, requestID).Error; err != nil {
		return Asset{}, actionFailure("ASSET_CANDIDATE_INVALID", err, "Copy candidate_id from a successful acquisition trial in this request.")
	}
	var a Asset
	if err := json.Unmarshal([]byte(trial.AssetJSON), &a); err != nil {
		return a, err
	}
	a.Active = true
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&a).Error; err != nil {
			return err
		}
		return s.indexAsset(tx, a, requestID)
	})
	return a, err
}

func (s *Service) indexAsset(tx *gorm.DB, a Asset, requestIDs ...string) error {
	indexErr := tx.Transaction(func(indexTx *gorm.DB) error {
		body := fmt.Sprintf("Acquisition asset %s\nKind: %s\n%s\nOutput contract: %s\nScope: %s", a.SourceID, a.Kind, a.Description, a.SpecJSON, a.ScopeJSON)
		doc, err := knowledge.NewDocument(knowledge.Input{Source: "information-capability", SourceKey: a.ID, Title: a.SourceID, URL: a.EntryURL, Body: body, Format: "text", FetchedAt: a.VerifiedAt, DataVersion: a.Version, RecordKey: a.ID})
		if err != nil {
			return err
		}
		_, err = knowledge.PutDocument(indexTx, doc, s.Crawler.Fingerprint, requestIDs...)
		return err
	})
	if indexErr != nil {
		for _, requestID := range requestIDs {
			if err := agentdata.NewStore(tx).AddTrace(tx.Statement.Context, requestID, "knowledge_index_deferred", map[string]any{"asset_id": a.ID, "error": indexErr.Error(), "asset_saved": true}); err != nil {
				return err
			}
		}
	}
	return nil
}

// A native asset's actual execution uses the same immutable declarative plan
// that passed trial. It commits standard typed acquisition snapshots, allowing
// the existing knowledge indexer/retriever to read them without crawler tools.
func (s *Service) ExecuteAPI(ctx context.Context, id, assetID string) ([]aiapp.Record, error) {
	var a Asset
	if err := s.DB.WithContext(ctx).First(&a, "id = ? AND kind = ?", assetID, "api").Error; err != nil {
		return nil, err
	}
	return s.ExecuteAsset(ctx, id, assetID, assetInvocation{})
}

func (s *Service) ExecuteAsset(ctx context.Context, id, assetID string, selected assetInvocation) ([]aiapp.Record, error) {
	var a Asset
	if err := s.DB.WithContext(ctx).First(&a, "id = ? AND active = ?", assetID, true).Error; err != nil {
		return nil, err
	}
	ctx, err := withAssetAcquisition(ctx, a)
	if err != nil {
		return nil, err
	}
	r, in, spec, err := s.trialRequest(ctx, id)
	if err != nil {
		return nil, err
	}
	if a.Kind != "api" && a.Kind != "browser" {
		return nil, fmt.Errorf("Lua assets execute only through the independent crawler entry")
	}
	a, err = nativeAssetForExecution(in, a, selected)
	if err != nil {
		return nil, err
	}
	input := map[string]any{"asset": a.ID, "version": a.Version, "manifest": a.ManifestJSON, "spec": spec}
	var step Step
	var fresh bool
	if a.Kind == "browser" {
		if r.Purpose == nativeSourcePurpose {
			if err := s.grantBrowserAsset(ctx, r, a); err != nil {
				return nil, err
			}
		}
		step, fresh, err = s.reserveBrowserStep(ctx, r, "execute_browser", a.EntryURL, input)
	} else {
		step, fresh, err = s.reserveStep(ctx, r, "execute_asset", input)
	}
	if err != nil {
		return nil, err
	}
	if !fresh {
		return nil, duplicateStep(step)
	}
	ctx, cancel := r.RequestContext(ctx)
	defer cancel()
	fetcher, err := s.NewFetcher(in.SourceScope)
	if err != nil {
		s.finishStep(context.WithoutCancel(ctx), step, nil, err)
		return nil, err
	}
	defer fetcher.Close()
	var runtime engine.ProgramResult
	var cause error
	if a.Kind == "api" {
		var plan APIPlan
		cause = json.Unmarshal([]byte(a.ManifestJSON), &plan)
		if cause == nil {
			runtime, cause = executeAPIWithRequester(ctx, plan, fetcher.FetchRequest)
		}
	} else {
		var plan BrowserPlan
		cause = json.Unmarshal([]byte(a.ManifestJSON), &plan)
		if cause == nil && s.Browser == nil {
			cause = fmt.Errorf("browser runtime is not configured; configure its executable, Node and Playwright module")
		}
		if cause == nil {
			runtime, cause = s.Browser.Render(ctx, plan, in.SourceScope, fetcher)
		}
	}
	runtime.Stats = fetcher.Stats()
	var rows []aiapp.Record
	if runtime.Collection != nil || cause == nil {
		var bindErr error
		rows, bindErr = nativeAcquisitionRecords(a, step, runtime)
		cause = errors.Join(cause, bindErr)
	}
	var observation *sourceCheckReceipt
	if cause == nil {
		observation = emptyAssetObservation(a, sourceCheckID(step), runtime)
	}
	var failure *ActionError
	if cause != nil {
		failure = actionFailure("ASSET_EXECUTION_FAILED", cause,
			"Read details.sources, saved runtime receipts, and any saved_observations.",
			"Keep the published version immutable. Trial a corrected mapping as a new version, or obtain missing facts from another source.")
		failure.PriorStepID = step.ID
		cause = failure
	}
	payload := map[string]any{"records": rows, "observation": observation, "runtime": runtime}
	if err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := requireNativeCommitOwner(tx, ctx, r, step); err != nil {
			return err
		}
		scoped := *s
		scoped.DB = tx
		savedObs, err := scoped.ingestAcquiredRecords(ctx, r.ID, step.ID, rows, runtime.SourceEvidence)
		if err != nil {
			return err
		}
		if failure != nil {
			enrichAcquisitionFailureDetails(failure, nil, runtime, savedObs)
			payload["failure"] = failure
		}
		return scoped.finishStep(ctx, step, payload, cause)
	}); err != nil {
		return rows, err
	}
	return rows, cause
}

// Acquisition identity is the admitted receipt and stable record position.
// A missing business join key is a fact/view diagnostic, not an intake veto.
func nativeAcquisitionRecords(a Asset, step Step, runtime engine.ProgramResult) ([]aiapp.Record, error) {
	var spec aiapp.TaskSpec
	if err := json.Unmarshal([]byte(a.SpecJSON), &spec); err != nil {
		return nil, err
	}
	rows, err := aiapp.BindRecords(runtime, aiapp.RecordBinding{SourceID: a.SourceID, DataVersion: a.Version, TaskID: step.ID, SchemaVersion: spec.SchemaVersion})
	if a.Kind != "api" || len(rows) == 0 {
		return rows, err
	}
	items, ok := runtime.Collection["items"].([]any)
	if !ok {
		return rows, err
	}
	for i := range rows {
		if i >= len(items) {
			break
		}
		item, ok := items[i].(map[string]any)
		if !ok {
			continue
		}
		ev, ok := item["_evidence"].(apiItemEvidence)
		if !ok || ev.ResponseIndex < 1 || ev.ResponseIndex > len(runtime.SourceEvidence) {
			continue
		}
		response := runtime.SourceEvidence[ev.ResponseIndex-1]
		provenance := &rows[i].Provenance
		provenance.URL, provenance.FetchedAt = response.Receipt.URL, response.Receipt.FetchedAt
		provenance.ResponseIndex = ev.ResponseIndex
		for field := range provenance.FieldSupport {
			support := aiapp.FieldSupport{Status: "supported"}
			if path, found := ev.Pointers[field]; found {
				support.ActualLocator = path
			}
			provenance.FieldSupport[field] = support
		}
	}
	return rows, err
}

func requireNativeCommitOwner(tx *gorm.DB, ctx context.Context, r Request, step Step) error {
	if step.RequestID != r.ID {
		return agentdata.ErrConflict
	}
	var current Request
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&current, "id = ?", r.ID).Error; err != nil {
		return err
	}
	expected := r.LeaseToken
	if owner, ok := ctx.Value(requestExecutionOwnerKey{}).(requestExecutionOwner); ok {
		if owner.RequestID != r.ID {
			return agentdata.ErrConflict
		}
		expected = owner.Token
	}
	now := time.Now()
	if terminal(current.Status) || !current.Deadline.IsZero() && !current.Deadline.After(now) || current.LeaseToken != expected {
		return agentdata.ErrConflict
	}
	if expected != "" && (current.LeaseUntil == nil || !current.LeaseUntil.After(now)) {
		return agentdata.ErrConflict
	}
	return nil
}

func (s *Service) Revoke(ctx context.Context, id string) error {
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var a Asset
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&a, "id = ?", id).Error; err != nil {
			return err
		}
		if err := tx.Model(&a).Update("active", false).Error; err != nil {
			return err
		}
		var heads []knowledge.Head
		if err := tx.Where("(source = ? AND record_key = ?) OR (source = ? AND data_version = ?)", "information-capability", a.ID, a.SourceID, a.Version).Find(&heads).Error; err != nil {
			return err
		}
		for _, h := range heads {
			if err := knowledge.NewStore(tx).Invalidate(ctx, h.ID); err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
		}
		return nil
	})
}

const nativeAPIExecutionContract = "An API plan uses the declared method, headers, body and JSON pointers. next_pointer follows observed URLs using that request configuration. Use run_lua for pagination requiring changed request bodies, other control flow, or field transformations. The parent task combines results from independent sources."

// The execution target belongs to the current request, not the tool parameter schema.
func publishedAssetTarget(in Input, asset Asset, selected assetInvocation) (string, error) {
	target := selected.URL
	if target == "" && len(in.EntryURLs) == 1 {
		target = in.EntryURLs[0]
	}
	if target == "" && len(in.EntryURLs) == 0 {
		target = asset.EntryURL
	}
	if target == "" || !urlInScope(target, in.SourceScope) {
		failure := actionFailure("ROUTING_ARGUMENTS_INVALID", fmt.Errorf("acquire_sources parameter url: an unambiguous absolute target within source_scope is required"), "Choose an exact asset ID from read_materials(kind='assets') and provide only its declared parameters; use a separate root url for the execution target.", "Set root url to the current intended entry from goal_input.entry_urls (or another observed URL within source_scope), keep the exact catalog asset_id, and supply only schema-declared tool params. Do not add url or version to params unless its input_schema declares them.")
		failure.Details = map[string]any{"field": "url", "actual": target, "entry_urls": in.EntryURLs, "expected": "one explicit or unambiguous current target within source_scope"}
		return "", failure
	}
	return target, nil
}

// Resolve native calls once for execution, durable identity and recovery.
func nativeAssetForExecution(in Input, a Asset, selected assetInvocation) (Asset, error) {
	if selected.URL == "" && len(selected.Params) == 0 {
		return a, nil
	}
	var manifest map[string]json.RawMessage
	if err := json.Unmarshal([]byte(a.ManifestJSON), &manifest); err != nil {
		return a, fmt.Errorf("decode native execution plan: %w", err)
	}
	if manifest == nil {
		return a, fmt.Errorf("native execution plan must be an object")
	}
	var original string
	if err := json.Unmarshal(manifest["url"], &original); err != nil {
		return a, fmt.Errorf("decode native execution URL: %w", err)
	}
	target := selected.URL
	if target == "" {
		target = original
	}
	if len(selected.Params) > 0 {
		parsed, err := url.Parse(target)
		if err != nil {
			return a, actionFailure("ROUTING_ARGUMENTS_INVALID", err, "Choose an exact asset ID from read_materials(kind='assets') and provide only its declared parameters; use a separate root url for the execution target.")
		}
		query := parsed.Query()
		for key, value := range selected.Params {
			query.Del(key)
			if value == nil {
				continue
			}
			values, sequence := value.([]any)
			if !sequence {
				values = []any{value}
			}
			for _, item := range values {
				text, ok := item.(string)
				if !ok {
					raw, err := json.Marshal(item)
					if err != nil {
						return a, actionFailure("ROUTING_ARGUMENTS_INVALID", fmt.Errorf("params.%s: %w", key, err), "Use JSON values for observed query parameters, or supply their complete encoded URL in root url.")
					}
					text = string(raw)
				}
				query.Add(key, text)
			}
		}
		parsed.RawQuery = query.Encode()
		target = parsed.String()
	}
	selected.URL = target
	target, err := publishedAssetTarget(in, a, selected)
	if err != nil {
		return a, err
	}
	if target != original {
		manifest["url"] = mustMarshal(target)
		a.ManifestJSON = string(mustMarshal(manifest))
	}
	a.EntryURL = target
	return a, nil
}
