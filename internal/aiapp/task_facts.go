package aiapp

import (
	"encoding/json"
	"go-lua-crawler/internal/agentdata"
	"sort"
)

func terminalChildFingerprint(state LoopState) string {
	terminal := state
	terminal.Children = nil
	terminal.TaskResults = nil
	for _, row := range state.TaskResults {
		if row.Task.Status.IsTerminal() {
			terminal.TaskResults = append(terminal.TaskResults, row)
		}
	}
	sort.Slice(terminal.TaskResults, func(i, j int) bool { return terminal.TaskResults[i].Task.ID < terminal.TaskResults[j].Task.ID })
	return agentdata.Hash(loopTaskFacts(terminal))
}

func unobservedTerminalChildren(state LoopState) bool {
	hasTerminal := false
	for _, row := range state.TaskResults {
		hasTerminal = hasTerminal || row.Task.Status.IsTerminal()
	}
	if !hasTerminal {
		return false
	}
	for i := len(state.Operations) - 1; i >= 0; i-- {
		op := state.Operations[i]
		if op.Kind != "model" || op.Stage != "succeeded" {
			continue
		}
		var response ChatResponse
		if json.Unmarshal([]byte(op.ResultJSON), &response) == nil && response.ObservedChildren != "" {
			return response.ObservedChildren != terminalChildFingerprint(state)
		}
		// Older saved responses have no snapshot marker. Only provably newer
		// terminal receipts trigger another decision; no history migration needed.
		for _, row := range state.TaskResults {
			if row.Task.Status.IsTerminal() && row.Task.FinishedAt != nil && row.Task.FinishedAt.After(op.CreatedAt) {
				return true
			}
		}
		return false
	}
	return true
}

// Shared by live decisions and context compaction. The frozen spec already owns
// completion requirements; child rows contribute execution identity/state only.
func loopTaskFacts(state LoopState) []map[string]any {
	tasks := make([]map[string]any, 0, len(state.Children))
	for _, row := range state.TaskResults {
		entry := map[string]any{"task_id": row.Task.ID, "source_id": row.Link.SourceID, "status": row.Task.Status, "failure_code": row.Task.FailureCode, "error": row.Task.LastError, "records": len(row.Records)}
		if len(row.OutputMetadata) > 0 {
			keys := []string{}
			for key := range row.OutputMetadata {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			entry["output_metadata_keys"] = keys
			entry["read_output_with"] = "get_task_results returns program_outputs for continuation/aggregation metadata; it is not acquired factual evidence"
		}
		tasks = append(tasks, entry)
	}
	if len(tasks) == 0 {
		for _, row := range state.Children {
			tasks = append(tasks, map[string]any{"task_id": row.TaskID, "source_id": row.SourceID, "artifact_hash": row.ArtifactHash})
		}
	}
	return tasks
}
