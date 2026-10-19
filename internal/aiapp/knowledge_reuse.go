package aiapp

import (
	"context"
	"encoding/json"
	"fmt"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/knowledge"
	"go-lua-crawler/internal/repository"
	"net/url"
	"strings"
)

type ReuseResult struct {
	Evidence knowledge.EvidenceSet `json:"evidence"`
	Records  []Record              `json:"records"`
}

func (s *Service) runEvidence(ctx context.Context, id string) (knowledge.EvidenceSet, error) {
	ops, err := s.Data.Operations(ctx, id)
	if err != nil {
		return knowledge.EvidenceSet{}, err
	}
	return evidenceFromOperations(id, ops), nil
}

func evidenceFromOperations(id string, ops []agentdata.Operation) knowledge.EvidenceSet {
	set := knowledge.EvidenceSet{RequestID: id}
	for _, op := range ops {
		if op.Kind == "search_knowledge" && op.Stage == "succeeded" {
			var out ToolResult
			var old knowledge.EvidenceSet
			if json.Unmarshal([]byte(op.ResultJSON), &out) == nil && json.Unmarshal(out.Data, &old) == nil {
				set.Entries = append(set.Entries, old.Entries...)
			}
		}
	}
	return set
}

func canonicalEvidenceIDs(prior knowledge.EvidenceSet, next knowledge.EvidenceSet) knowledge.EvidenceSet {
	known := map[string]string{}
	identity := func(e knowledge.Evidence) string {
		return agentdata.Hash([]string{e.DocID, e.Revision, e.Generation, e.ChunkID})
	}
	for _, e := range prior.Entries {
		key := identity(e)
		if known[key] == "" {
			known[key] = e.ID
		}
	}
	for n := range next.Entries {
		e := &next.Entries[n]
		if e.DocID == "" || e.Revision == "" || e.Generation == "" || e.ChunkID == "" {
			continue
		}
		key := identity(*e)
		if id := known[key]; id != "" {
			e.ID = id
		} else {
			known[key] = e.ID
		}
	}
	return next
}

// Field bindings create a run-local view of an acquired snapshot. They cannot
// supply literals, change provenance, or publish another knowledge generation.
func projectReusedRecord(spec TaskSpec, row Record, bindings map[string]string) (Record, error) {
	if len(bindings) == 0 {
		return row, nil
	}
	wanted := map[string]string{}
	for _, field := range spec.RequiredFields {
		wanted[field.Name] = field.Type
	}
	view := row
	view.Provenance.FetchedAt = view.Provenance.FetchedAt.UTC()
	raw, err := json.Marshal(view)
	if err != nil {
		return Record{}, err
	}
	var source any
	if err := json.Unmarshal(raw, &source); err != nil {
		return Record{}, err
	}
	out := row
	out.Fields = make(map[string]any, len(row.Fields)+len(bindings))
	for name, value := range row.Fields {
		out.Fields[name] = value
	}
	for target, pointer := range bindings {
		kind, requested := wanted[target]
		if !requested || (!strings.HasPrefix(pointer, "/fields/") && !strings.HasPrefix(pointer, "/provenance/")) {
			return Record{}, fmt.Errorf("field binding must select acquired fields/provenance for a requested column")
		}
		value, err := jsonPointer(source, pointer)
		if err != nil || !fieldMatches(value, kind) {
			return Record{}, fmt.Errorf("field binding %s has missing or incompatible source value", target)
		}
		out.Fields[target] = value
	}
	return out, nil
}

func (s *Service) reuseKnowledge(ctx context.Context, r agentdata.Run, call ToolCall, ids []string, bindings map[string]string) (ReuseResult, error) {
	out := ReuseResult{Evidence: knowledge.EvidenceSet{RequestID: r.ID}, Records: []Record{}}
	if len(ids) == 0 || len(ids) > 8 {
		return out, fmt.Errorf("select 1..8 evidence IDs")
	}
	spec, err := specOf(r)
	if err != nil {
		return out, err
	}
	set, err := s.runEvidence(ctx, r.ID)
	if err != nil {
		return out, err
	}
	reader := knowledge.SetReader{Store: s.Knowledge, Set: set}
	seen := map[string]bool{}
	for _, id := range ids {
		args, _ := json.Marshal(map[string]string{"evidence_id": id})
		if _, err = s.ReadOnly(ctx, ToolCall{ID: call.ID + "/" + id, Type: "function", Function: FunctionCall{Name: "read_evidence", Arguments: string(args)}}, reader); err != nil {
			return out, err
		}
		ev, err := reader.ReadEvidence(ctx, id)
		if err != nil {
			return out, err
		}
		if ev.TaskID == 0 || ev.RecordKey == "" {
			return out, fmt.Errorf("evidence has no acquired typed record; use read_evidence for standalone document answers")
		}
		var saved repository.CrawlResult
		if err = s.Data.DB.WithContext(ctx).First(&saved, "task_id = ?", ev.TaskID).Error; err != nil {
			return out, err
		}
		var payload AcquisitionResult
		if err = json.Unmarshal([]byte(saved.Payload), &payload); err != nil {
			return out, err
		}
		found := false
		for _, row := range payload.Records {
			if row.Key != ev.RecordKey || row.Provenance.DataVersion != ev.DataVersion {
				continue
			}
			if !recordInScope(row, scope(r)) {
				return out, fmt.Errorf("stored source is outside original scope")
			}
			key := agentdata.Hash([]any{row.Key, row.Provenance.TaskID, row.Provenance.DataVersion})
			if !seen[key] {
				projected, err := projectReusedRecord(spec, row, bindings)
				if err != nil {
					return out, err
				}
				out.Records = append(out.Records, projected)
				seen[key] = true
			}
			found = true
			break
		}
		if !found {
			return out, fmt.Errorf("evidence does not bind to the saved acquisition snapshot")
		}
		out.Evidence.Entries = append(out.Evidence.Entries, ev)
	}
	return out, nil
}
func recordInScope(row Record, domains []string) bool {
	if len(domains) == 0 {
		return true
	}
	u, err := url.Parse(row.Provenance.URL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	for _, d := range domains {
		d = strings.ToLower(strings.TrimSpace(d))
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}
func (s *Service) currentReusedRecords(ctx context.Context, r agentdata.Run, ops []agentdata.Operation) ([]Record, []string, error) {
	var err error
	rows := []Record{}
	issues := []string{}
	seen := map[string]bool{}
	for _, op := range ops {
		if op.Kind != "reuse_knowledge" || op.Stage != "succeeded" {
			continue
		}
		var response ToolResult
		var result ReuseResult
		if err = json.Unmarshal([]byte(op.ResultJSON), &response); err != nil {
			return nil, nil, err
		}
		if err = json.Unmarshal(response.Data, &result); err != nil {
			return nil, nil, err
		}
		ids := []string{}
		for _, ev := range result.Evidence.Entries {
			ids = append(ids, ev.ID)
		}
		if err = knowledge.CheckReferences(ctx, knowledge.SetReader{Store: s.Knowledge, Set: result.Evidence}, ids); err != nil {
			issues = append(issues, op.Key+": "+err.Error())
			continue
		}
		for _, row := range result.Records {
			// Origin identity deduplicates snapshots, not distinct run-local
			// field views. Complementary bindings of that snapshot must reach
			// MergeRecords together; exact repeated views remain idempotent.
			key := agentdata.Hash([]any{row.Key, row.Provenance.TaskID, row.Provenance.DataVersion, row.Fields})
			if !seen[key] {
				rows = append(rows, row)
				seen[key] = true
			}
		}
	}
	return rows, issues, nil
}
