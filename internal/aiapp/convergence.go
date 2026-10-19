package aiapp

import (
	"encoding/json"
	"go-lua-crawler/internal/agentdata"
)

// Compare executed observation batches, not changing model/tool-call IDs.
// New arguments (including page offsets) or changed resource data reset repeats.
func repeatedObservationRounds(operations []agentdata.Operation) int {
	batches := observationBatches(operations)
	if len(batches) == 0 {
		return 0
	}
	n := 1
	last := batches[len(batches)-1]
	for i := len(batches) - 2; i >= 0 && batches[i] == last; i-- {
		n++
	}
	return n
}

// A repeated period must include the same inputs AND observed outcomes.
// Changed pages, corrected programs and recovered sources break the period.
func repeatedObservationCycle(operations []agentdata.Operation) (period, repeats int) {
	batches := observationBatches(operations)
	for width := 1; width <= 4 && width*2 <= len(batches); width++ {
		count := 1
		for start := len(batches) - 2*width; start >= 0; start -= width {
			same := true
			for i := 0; i < width; i++ {
				if batches[start+i] != batches[len(batches)-width+i] {
					same = false
					break
				}
			}
			if !same {
				break
			}
			count++
		}
		if count >= 2 && count > repeats {
			period, repeats = width, count
		}
	}
	return
}

func observationBatches(operations []agentdata.Operation) []string {
	batches := []string{}
	batch := []any{}
	started := false
	flush := func() {
		if started {
			batches = append(batches, agentdata.Hash(batch))
		}
		batch = []any{}
	}
	for _, op := range operations {
		if op.Kind == "model" {
			flush()
			started = op.Stage == "succeeded"
			continue
		}
		if !started || op.Kind == "worker_attempt" {
			continue
		}
		var receipt ToolResult
		if json.Unmarshal([]byte(op.ResultJSON), &receipt) != nil || receipt.Name == "" {
			continue
		}
		var data map[string]any
		if json.Unmarshal(receipt.Data, &data) != nil {
			data = map[string]any{"raw": string(receipt.Data)}
		}
		// Provider accounting does not represent new task evidence.
		delete(data, "metrics")
		delete(data, "request_id")
		delete(data, "cache_hit")
		delete(data, "cache_original_call_id")
		if p, ok := data["progress"].(map[string]any); ok {
			delete(p, "version")
			delete(p, "notes")
			delete(p, "next_purpose")
		}
		input := op.InputHash
		if op.Stage == "succeeded" && receipt.Error == "" && receipt.Name == "read_data" && data["offset"] != nil && data["view_version"] != nil {
			// Different read arguments are not new information by themselves.
			// Only a versioned response with its actual offset can prove the
			// same read window. Legacy/unlocated outputs keep their input identity.
			input = "observed-read"
		}
		if op.Kind == "update_task_progress" {
			input = "checkpoint"
			delete(data, "version")
			delete(data, "notes")
			delete(data, "next_purpose")
		}
		batch = append(batch, map[string]any{"kind": op.Kind, "input": input, "stage": op.Stage, "error": receipt.Error, "data": data})
	}
	flush()
	return batches
}
