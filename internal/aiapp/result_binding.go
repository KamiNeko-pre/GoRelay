package aiapp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"

	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/engine"
)

type RecordBinding struct {
	SourceID      string
	DataVersion   string
	TaskID        uint64
	SchemaVersion string
	KeyFields     []string
}

// The caller supplies immutable task/source identity. Lua supplies fields and an
// observed source_url, but cannot invent the task ID, source version or fetch time.
// URL binding proves acquisition, not that a value is semantically supported.
func BindRecords(result engine.ProgramResult, binding RecordBinding) ([]Record, error) {
	records := []Record{}
	if binding.SourceID == "" || binding.DataVersion == "" || binding.TaskID == 0 || binding.SchemaVersion == "" {
		return records, fmt.Errorf("record binding identity is incomplete")
	}
	receipts := map[string]acquisition.Receipt{}
	for _, receipt := range result.Requests {
		if receipt.Method == http.MethodGet && receipt.Status >= 200 && receipt.Status < 300 && !receipt.FetchedAt.IsZero() {
			receipts[receipt.URL] = receipt
			if receipt.RequestedURL != "" {
				receipts[receipt.RequestedURL] = receipt
			}
		}
	}
	items, ok := result.Collection["items"].([]any)
	if !ok {
		return records, fmt.Errorf("program items must be an array")
	}
	for i, item := range items {
		row, ok := item.(map[string]any)
		if !ok {
			return records, fmt.Errorf("item %d must be an object", i)
		}
		sourceURL, ok := row["source_url"].(string)
		receipt, observed := receipts[sourceURL]
		if !ok || !observed {
			return records, fmt.Errorf("item %d source_url has no successful fetch receipt", i)
		}
		keyParts := []any{binding.SourceID, binding.TaskID}
		if len(binding.KeyFields) == 0 {
			keyParts = append(keyParts, i)
		} else {
			for _, field := range binding.KeyFields {
				v, ok := row[field]
				if !ok || v == nil {
					return records, fmt.Errorf("item %d lacks business key %q", i, field)
				}
				keyParts = append(keyParts, v)
			}
		}
		key, err := json.Marshal(keyParts)
		if err != nil {
			return records, err
		}
		data, err := json.Marshal(row)
		if err != nil {
			return records, err
		}
		fields := map[string]any{}
		// Preserve numeric precision; values remain JSON numbers, not strings.
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		if err = decoder.Decode(&fields); err != nil {
			return records, err
		}
		proposed := fields["_evidence"]
		delete(fields, "_evidence")
		support := hostFieldSupport(result, sourceURL, fields, proposed)
		records = append(records, Record{Key: string(key), SchemaVersion: binding.SchemaVersion, Fields: fields, Provenance: Provenance{SourceID: binding.SourceID, URL: receipt.URL, FetchedAt: receipt.FetchedAt, DataVersion: binding.DataVersion, TaskID: binding.TaskID, FieldSupport: support}})
	}
	return records, nil
}
