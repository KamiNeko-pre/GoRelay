package aiapp

import (
	"context"
	"encoding/json"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/knowledge"
	"sort"
	"strings"

	"github.com/PuerkitoBio/goquery"
)

// Child completion and index publication happen outside the parent loop.
// Observe and persist their progress before admission, under the parent row
// lock, so a stale budget cannot reject the next necessary model/tool turn.
func (s *Service) executionState(ctx context.Context, run agentdata.Run) (state LoopState, err error) {
	var budget BudgetSnapshot
	if err = json.Unmarshal([]byte(run.BudgetJSON), &budget); err != nil {
		return state, err
	}
	if budget.LoopVersion < 4 {
		return s.state(ctx, run)
	}
	err = s.Data.WithOwnedRun(ctx, run.ID, run.Token, func(store *agentdata.Store) error {
		current, e := store.Run(ctx, run.ID)
		if e != nil {
			return e
		}
		scoped := *s
		scoped.Data = store
		scoped.Knowledge = knowledge.NewStore(store.DB)
		state, e = scoped.state(ctx, current)
		if e != nil {
			return e
		}
		before := state.HostProgress
		state.HostProgress = scoped.advanceHostProgress(ctx, before, state)
		if before.Signature != "" && before.Signature != state.HostProgress.Signature {
			state.Budget.ProgressBase = state.Budget.Spent
		}
		hostJSON, _ := json.Marshal(state.HostProgress)
		budgetJSON, _ := json.Marshal(state.Budget)
		state.Run.HostProgressJSON = string(hostJSON)
		state.Run.BudgetJSON = string(budgetJSON)
		return store.DB.WithContext(ctx).Model(&agentdata.Run{}).Where("id = ?", run.ID).Updates(map[string]any{"host_progress_json": string(hostJSON), "budget_json": string(budgetJSON)}).Error
	})
	return state, err
}

// This is host-owned execution state. Checkpoints, query spelling, page offsets
// and model claims cannot reset it. It commits with the scheduling transition.
type HostProgress struct {
	Recovery              DeliveryRecovery    `json:"delivery_recovery,omitempty"`
	Signature             string              `json:"signature"`
	LastModelOperation    uint64              `json:"last_model_operation"`
	NoProgressModelRounds int64               `json:"no_progress_model_rounds"`
	DeliverySignature     string              `json:"delivery_signature,omitempty"`
	ReadSignature         string              `json:"read_signature,omitempty"`
	ExplorationRounds     int64               `json:"exploration_rounds,omitempty"`
	LastReadingModel      uint64              `json:"last_reading_model,omitempty"`
	SemanticCheckedModel  uint64              `json:"semantic_checked_model,omitempty"`
	SemanticMatchedRound  string              `json:"semantic_matched_round,omitempty"`
	SemanticCheck         *SemanticAssessment `json:"semantic_check,omitempty"`
	// ProbeSignature tracks distinct successful exploration probes. A probe that
	// returns a structure the model had not seen is real progress toward a
	// working flow, so it renews the no-progress review the same way a new
	// fetched response does. It never satisfies delivery coverage.
	ProbeSignature string `json:"probe_signature,omitempty"`
}

// probeSignature digests the distinct successful probe outcomes. Repeated
// identical probes do not renew progress, so this cannot be used to idle.
func probeSignature(state LoopState) string {
	outcomes := []string{}
	seen := map[string]bool{}
	for _, op := range state.Operations {
		if op.Kind != "validate_program" || op.Stage != "succeeded" {
			continue
		}
		var receipt ToolResult
		if json.Unmarshal([]byte(op.ResultJSON), &receipt) != nil || receipt.Error != "" {
			continue
		}
		var data struct {
			Mode   string   `json:"mode"`
			Rows   int      `json:"rows"`
			Fields []string `json:"produced_fields"`
			Items  any      `json:"items"`
		}
		if json.Unmarshal(receipt.Data, &data) != nil || data.Mode != "probe" {
			continue
		}
		sort.Strings(data.Fields)
		key := agentdata.Hash([]any{data.Fields, data.Rows, data.Items})
		if !seen[key] {
			outcomes = append(outcomes, key)
			seen[key] = true
		}
	}
	sort.Strings(outcomes)
	return agentdata.Hash(outcomes)
}

// V5 delivery progress is accepted fields/records, useful validated or published
// capability and current evidence. Source fetching, read ranges, call IDs and
// diagnostic spelling cannot renew that signature. Legacy runs retain their
// original preparation accounting; V5 bounds necessary structural reads apart.
func hostDeliverySignature(state LoopState) string {
	if state.Budget.LoopVersion >= 5 {
		return hostProgressSignature(state, false)
	}
	return hostProgressSignature(state, true)
}

// hostProgressSignature builds the durable progress signature.
//
// preparation=true additionally carries fetched material, evidence and consumed
// read ranges, which describe what the run has looked at rather than what it
// achieved.
func hostProgressSignature(state LoopState, preparation bool) string {
	capabilities := []string{}
	seen := map[string]bool{}
	for _, op := range state.Operations {
		if op.Stage != "succeeded" || (op.Kind != "validate_program" && op.Kind != "register_source") {
			continue
		}
		var result ToolResult
		var data struct {
			Hash string `json:"artifact_hash"`
		}
		if json.Unmarshal([]byte(op.ResultJSON), &result) == nil && json.Unmarshal(result.Data, &data) == nil && data.Hash != "" && !seen[data.Hash] {
			capabilities = append(capabilities, data.Hash)
			seen[data.Hash] = true
		}
	}
	if state.Budget.LoopVersion >= 4 {
		capabilities = capabilityWorkKeys(state)
	}
	sort.Strings(capabilities)
	// Only host-accepted records count, not model-written requirement statuses.
	records := []string{}
	if state.Result != nil {
		for _, r := range state.Result.Records {
			if state.Budget.LoopVersion >= 4 {
				records = append(records, agentdata.Hash([]any{r.SchemaVersion, r.Fields}))
			} else {
				records = append(records, agentdata.Hash(r))
			}
		}
	}
	sort.Strings(records)
	material := map[string]any{"spec": state.Spec, "capabilities": capabilities, "records": records}
	if state.Budget.LoopVersion >= 5 {
		current := []string{}
		seen := map[string]bool{}
		for _, e := range state.EvidenceCatalog {
			doc, _ := e["doc_id"].(string)
			revision, _ := e["revision"].(string)
			generation, _ := e["generation"].(string)
			chunk, _ := e["chunk_id"].(string)
			if doc == "" || revision == "" || generation == "" || chunk == "" {
				continue
			}
			key := agentdata.Hash([]string{doc, revision, generation, chunk})
			if !seen[key] {
				seen[key] = true
				current = append(current, key)
			}
		}
		sort.Strings(current)
		material["current_evidence"] = current
	}
	if !preparation {
		return agentdata.Hash(material)
	}
	evidence := []string{}
	for _, e := range state.EvidenceCatalog {
		if state.Budget.LoopVersion >= 4 {
			evidence = append(evidence, agentdata.Hash([]any{e["doc_id"], e["revision"], e["generation"], e["chunk_id"]}))
		} else {
			evidence = append(evidence, agentdata.Hash(e))
		}
	}
	sort.Strings(evidence)
	// New host-fetched material is acquisition progress before an adapter can
	// be generated. It never satisfies delivery coverage or promotes facts.
	// Deduplicate content, not call IDs/URLs/timestamps, so repeated reads and
	// changed URL spelling cannot keep the loop alive. Distinct preparation is
	// separately bounded by the delivery review; it cannot renew that review.
	acquired := []string{}
	seenAcquired := map[string]bool{}
	for _, op := range state.Operations {
		if op.Kind != "inspect_source" || op.Stage != "succeeded" {
			continue
		}
		var result ToolResult
		var page sourceInspection
		if json.Unmarshal([]byte(op.ResultJSON), &result) != nil || result.Error != "" || json.Unmarshal(result.Data, &page) != nil {
			continue
		}
		r := page.Source.Receipt
		body := strings.Join(strings.Fields(page.Source.Body), " ")
		if r.Method != "GET" || r.Status < 200 || r.Status >= 300 || r.URL == "" || r.FetchedAt.IsZero() || body == "" {
			continue
		}
		key := agentdata.Hash(body)
		if !seenAcquired[key] {
			seenAcquired[key] = true
			acquired = append(acquired, key)
		}
	}
	sort.Strings(acquired)
	material["evidence"], material["acquired"] = evidence, acquired
	if state.Budget.LoopVersion >= 4 {
		material["consumed_ranges"] = consumedReadRanges(state)
	}
	return agentdata.Hash(material)
}

// Validation is useful once for a given observed output, and publication is a
// separate milestone. Merely changing code/opaque artifact identity while
// producing the same required facts cannot keep a stalled task alive.
func capabilityWorkKeys(state LoopState) []string {
	byHash := map[string]string{}
	keys := map[string]bool{}
	for _, op := range state.Operations {
		if op.Kind != "validate_program" || op.Stage != "succeeded" {
			continue
		}
		var out ToolResult
		var candidate struct {
			Hash   string `json:"artifact_hash"`
			Result struct {
				Collection struct {
					Items []map[string]any `json:"items"`
				} `json:"collection"`
			} `json:"result"`
		}
		if json.Unmarshal([]byte(op.ResultJSON), &out) != nil || out.Error != "" || json.Unmarshal(out.Data, &candidate) != nil || candidate.Hash == "" || len(candidate.Result.Collection.Items) == 0 {
			continue
		}
		facts := []string{}
		for _, item := range candidate.Result.Collection.Items {
			fields := map[string]any{"source_url": item["source_url"]}
			if state.Spec != nil {
				for _, f := range state.Spec.RequiredFields {
					fields[f.Name] = item[f.Name]
				}
			} else {
				for k, v := range item {
					if k != "_evidence" {
						fields[k] = v
					}
				}
			}
			facts = append(facts, agentdata.Hash(fields))
		}
		sort.Strings(facts)
		identity := agentdata.Hash(facts)
		byHash[candidate.Hash] = identity
		keys["validated:"+identity] = true
	}
	for _, op := range state.Operations {
		if op.Kind != "register_source" || op.Stage != "succeeded" {
			continue
		}
		var out ToolResult
		var registered struct {
			Hash string `json:"artifact_hash"`
		}
		if json.Unmarshal([]byte(op.ResultJSON), &out) == nil && out.Error == "" && json.Unmarshal(out.Data, &registered) == nil {
			if identity, ok := byHash[registered.Hash]; ok {
				keys["published:"+identity] = true
			}
		}
	}
	result := []string{}
	for key := range keys {
		result = append(result, key)
	}
	return result
}

// Successful readers have already checked ownership and bounds. Count newly
// consumed ranges of the same immutable resource, rather than call IDs or text
// hashes: identical paragraphs at different offsets still require reading.
func consumedReadRanges(state LoopState) map[string][][2]int {
	parents := map[string]agentdata.Operation{}
	for _, op := range state.Operations {
		parents[strings.TrimPrefix(op.Key, "tool:")] = op
	}
	evidence := evidenceFromOperations(state.Run.ID, state.Operations)
	ranges := map[string][][2]int{}
	refinements := map[string][][2]int{}
	for _, op := range state.Operations {
		if op.Kind != "read_data" || op.Stage != "succeeded" {
			continue
		}
		var receipt ToolResult
		var data struct {
			Handle   string          `json:"handle"`
			Pointer  string          `json:"pointer"`
			Version  string          `json:"view_version"`
			Selector string          `json:"selector"`
			Offset   int             `json:"offset"`
			Text     string          `json:"text"`
			Value    json.RawMessage `json:"value"`
			Nodes    json.RawMessage `json:"nodes"`
			Catalog  json.RawMessage `json:"selector_catalog"`
		}
		if json.Unmarshal([]byte(op.ResultJSON), &receipt) != nil || receipt.Error != "" || receipt.Name != "read_data" || json.Unmarshal(receipt.Data, &data) != nil || data.Handle == "" || data.Version == "" || data.Offset < 0 {
			continue
		}
		width := len([]rune(data.Text))
		if len(data.Value) == 0 {
			if len(data.Nodes) > 0 {
				data.Value = data.Nodes
			} else {
				data.Value = data.Catalog
			}
		}
		if width == 0 && state.Budget.LoopVersion >= 5 && len(data.Value) > 0 {
			var value any
			if json.Unmarshal(data.Value, &value) == nil {
				switch v := value.(type) {
				case []any:
					width = len(v)
				case map[string]any:
					width = len(v)
				default:
					if value != nil {
						width = 1
					}
				}
			}
		}
		if width == 0 {
			continue
		}
		if state.Budget.LoopVersion >= 5 {
			// A receipt of a read is a view of the original resource, not a
			// newly acquired source. Follow the immutable, backward-only chain.
			valid := true
			for depth := 0; depth < len(state.Operations); depth++ {
				parent, ok := parents[data.Handle]
				if !ok || parent.Kind != "read_data" {
					break
				}
				var out ToolResult
				var prior struct {
					Handle   string `json:"handle"`
					Pointer  string `json:"pointer"`
					Version  string `json:"view_version"`
					Selector string `json:"selector"`
					Offset   int    `json:"offset"`
					Text     string `json:"text"`
				}
				if parent.Stage != "succeeded" || data.Pointer != "/text" || json.Unmarshal([]byte(parent.ResultJSON), &out) != nil || json.Unmarshal(out.Data, &prior) != nil || prior.Handle == "" || prior.Text == "" {
					valid = false
					break
				}
				data.Handle, data.Pointer, data.Version, data.Selector = prior.Handle, prior.Pointer, prior.Version, prior.Selector
				data.Offset += prior.Offset
			}
			if !valid {
				continue
			}
		}
		identity := ""
		selectorIdentity := data.Selector
		if strings.HasPrefix(data.Handle, "ev_") {
			for _, e := range evidence.Entries {
				if e.ID == data.Handle {
					identity = agentdata.Hash([]string{e.DocID, e.Revision, e.Generation, e.ChunkID})
					break
				}
			}
		} else if parent, ok := parents[data.Handle]; ok && parent.Stage == "succeeded" {
			var out ToolResult
			if json.Unmarshal([]byte(parent.ResultJSON), &out) != nil {
				continue
			}
			if parent.Kind == "inspect_source" {
				var page sourceInspection
				if json.Unmarshal(out.Data, &page) == nil {
					identity = agentdata.Hash(page.Source.Body)
					if state.Budget.LoopVersion >= 5 && data.Selector != "" {
						doc, err := goquery.NewDocumentFromReader(strings.NewReader(page.Source.Body))
						if err != nil {
							continue
						}
						paths := []string{}
						for _, node := range doc.Find(strings.TrimSpace(data.Selector)).Nodes {
							paths = append(paths, domSelector(node))
						}
						if len(paths) == 0 {
							continue
						}
						selectorIdentity = agentdata.Hash(paths)
					}
				}
			} else {
				identity = agentdata.Hash(out.Data)
			}
		}
		if identity == "" {
			continue
		}
		key := agentdata.Hash([]string{identity, data.Pointer, data.Version, selectorIdentity})
		part := [2]int{data.Offset, data.Offset + width}
		for _, prior := range ranges[key] {
			if part[0] >= prior[0] && part[1] <= prior[1] && part[1]-part[0] < prior[1]-prior[0] {
				// A narrower host-owned excerpt can restore information lost by
				// a compact preview. Credit each exact refinement once, not its
				// changing call ID or repeated replay.
				if state.Budget.LoopVersion >= 5 {
					// One recovery excerpt per immutable field/view is work. A
					// combinatorial sequence of tiny subranges is not new work.
					if _, seen := refinements["refinement:"+key]; !seen {
						refinements["refinement:"+key] = [][2]int{part}
					}
				} else {
					refinements["refinement:"+key+":"+agentdata.Hash(part)] = [][2]int{part}
				}
				break
			}
		}
		ranges[key] = append(ranges[key], part)
	}
	for key, parts := range ranges {
		sort.Slice(parts, func(i, j int) bool {
			if parts[i][0] == parts[j][0] {
				return parts[i][1] < parts[j][1]
			}
			return parts[i][0] < parts[j][0]
		})
		merged := [][2]int{}
		for _, part := range parts {
			if len(merged) == 0 || part[0] > merged[len(merged)-1][1] {
				merged = append(merged, part)
			} else if part[1] > merged[len(merged)-1][1] {
				merged[len(merged)-1][1] = part[1]
			}
		}
		ranges[key] = merged
	}
	for key, parts := range refinements {
		ranges[key] = parts
	}
	return ranges
}

// advanceHostProgressForTest runs the liveness update without a Service, so
// existing fixtures keep working. Semantic novelty is skipped because there is
// no embedder; every structural rule still applies.
func advanceHostProgressForTest(previous HostProgress, state LoopState) HostProgress {
	return (&Service{}).advanceHostProgress(context.Background(), previous, state)
}

// advanceHostProgress observes one completed round and updates the liveness
// counters. The receiver carries the local embedder, and ctx bounds the
// semantic comparison it may perform (a local model, so the cost is latency).
func (s *Service) advanceHostProgress(ctx context.Context, previous HostProgress, state LoopState) HostProgress {
	signature := hostDeliverySignature(state)
	for _, op := range state.Operations {
		if op.Kind == "model" && op.Stage != "reserved" && op.ID > previous.LastModelOperation {
			previous.NoProgressModelRounds++
			if state.Budget.LoopVersion >= 4 {
				previous.ExplorationRounds++
			}
			previous.LastModelOperation = op.ID
		}
	}
	if state.Budget.LoopVersion >= 4 {
		delivery := hostProgressSignature(state, false)
		read := agentdata.Hash(consumedReadRanges(state))
		if previous.DeliverySignature != "" && previous.DeliverySignature != delivery {
			previous.ExplorationRounds = 0
		} else if previous.ReadSignature != "" && previous.ReadSignature != read && previous.LastModelOperation > previous.LastReadingModel && readingOnlyRound(state, previous.LastModelOperation) {
			// Paging already fetched immutable material is finite necessary work.
			// Exempt this reading turn, without erasing earlier exploration debt.
			if previous.ExplorationRounds > 0 {
				previous.ExplorationRounds--
			}
			previous.LastReadingModel = previous.LastModelOperation
		}
		previous.DeliverySignature, previous.ReadSignature = delivery, read
	}
	// Work receipts may be restored together with their preceding model turn.
	// Account that turn first, then renew progress, matching durable event order.
	structureChanged := previous.Signature != "" && previous.Signature != signature
	if structureChanged {
		previous.NoProgressModelRounds = 0
	}
	previous.Signature = signature
	// Compare every completed decision round, including successful tool work.
	// Delivery novelty and semantic similarity are independent measurements.
	semanticIdle := false
	semanticMatch := ""
	if model, settled := lastSettledModel(state); settled && model.ID > previous.SemanticCheckedModel {
		previous.SemanticCheckedModel = model.ID
		check := s.assessSemanticRepeat(ctx, state)
		previous.SemanticCheck = &check
		repeat, matched := check.Repeated, check.MatchedRound
		previous.SemanticMatchedRound = matched
		semanticIdle, semanticMatch = repeat && !structureChanged, matched
	}
	previous.Recovery = advanceDeliveryRecovery(previous.Recovery, state, previous, semanticIdle, semanticMatch)
	return previous
}

func readingOnlyRound(state LoopState, modelID uint64) bool {
	found := false
	for _, op := range state.Operations {
		if op.ID <= modelID {
			continue
		}
		if op.Kind != "read_data" || op.Stage != "succeeded" {
			return false
		}
		var receipt ToolResult
		if json.Unmarshal([]byte(op.ResultJSON), &receipt) != nil || receipt.Error != "" {
			return false
		}
		found = true
	}
	return found
}

// explorationReviewWindow is the number of unproductive rounds after which the
// host tells the model, in its own context, that preparation has not advanced
// delivery.
//
// It must fire early enough that the model can still act on it, and it must not
// scale with delivery breadth. The previous width (delivery breadth times the
// stagnation limit) produced an 18-round review window on the procurement
// contract while the no-progress limit ends a run at 6 consecutive unproductive
// rounds, so the review arrived after the run was already over and the model
// never received the one signal that could change its strategy.
//
// Two stagnation windows: the first window issues the recovery review, the
// second is the allowance the model has to act on it before the run ends.
func explorationReviewWindow(state LoopState) int64 {
	return 2 * hostProgressLimit(state.Budget)
}

func hostProgressLimit(b BudgetSnapshot) int64 {
	if b.Limits.StagnationModelCalls > 0 {
		return b.Limits.StagnationModelCalls
	}
	return DefaultBudgetLimits().StagnationModelCalls
}
