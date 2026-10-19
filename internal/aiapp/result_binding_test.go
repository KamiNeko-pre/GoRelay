package aiapp

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/engine"
)

func TestBindRecordsRequiresActualSuccessfulFetch(t *testing.T) {
	now := time.Now().UTC()
	program := engine.ProgramResult{Collection: map[string]any{"items": []any{map[string]any{"name": "A", "count": 2.0, "source_url": "https://example.com/detail"}}}, Requests: []acquisition.Receipt{{URL: "https://example.com/detail", Method: http.MethodGet, Status: 200, FetchedAt: now}}}
	binding := RecordBinding{SourceID: "source", DataVersion: "code-v1", TaskID: 4, SchemaVersion: "v1", KeyFields: []string{"name"}}
	records, err := BindRecords(program, binding)
	if err != nil || len(records) != 1 || records[0].Provenance.TaskID != 4 || !records[0].Provenance.FetchedAt.Equal(now) || records[0].Fields["count"] != json.Number("2") {
		t.Fatalf("bad binding: %+v %v", records, err)
	}
	program.Collection["items"].([]any)[0].(map[string]any)["source_url"] = "https://example.com/never-fetched"
	if _, err = BindRecords(program, binding); err == nil {
		t.Fatal("invented source accepted")
	}
	program.Collection["items"].([]any)[0].(map[string]any)["source_url"] = "https://example.com/detail"
	program.Requests[0].Status = 404
	if _, err = BindRecords(program, binding); err == nil {
		t.Fatal("failed HTTP response became evidence")
	}
	program.Requests[0].Status = 200
	program.Requests[0].Method = http.MethodHead
	if _, err = BindRecords(program, binding); err == nil {
		t.Fatal("HEAD headers became body evidence")
	}
	program.Requests[0].Method = http.MethodGet
	program.Requests[0].RequestedURL = "https://example.com/detail"
	program.Requests[0].URL = "https://example.com/canonical"
	records, err = BindRecords(program, binding)
	if err != nil || records[0].Provenance.URL != "https://example.com/canonical" {
		t.Fatalf("redirect provenance lost: %+v %v", records, err)
	}
}

func TestMergeComplementsFieldsWithFieldProvenance(t *testing.T) {
	a := Record{Key: "a", Fields: map[string]any{"name": "A", "version": "1"}, Provenance: Provenance{SourceID: "release"}}
	b := Record{Key: "b", Fields: map[string]any{"name": "A", "license": "MIT"}, Provenance: Provenance{SourceID: "catalog"}}
	got, err := MergeRecords([]Record{a, b}, []string{"name"})
	if err != nil || len(got.Records) != 1 || got.Records[0].Fields["license"] != "MIT" || got.Records[0].FieldSources["license"][0].SourceID != "catalog" || got.Records[0].FieldSources["version"][0].SourceID != "release" {
		t.Fatalf("lost join/field evidence: %+v %v", got, err)
	}
}
