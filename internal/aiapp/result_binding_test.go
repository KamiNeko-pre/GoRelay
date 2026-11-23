package aiapp

import (
	"net/http"
	"testing"
	"time"

	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/engine"
)

func TestBindRecordsAndCapturedDocumentsAcceptPOST(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	target := "https://example.org/api/search"
	result := engine.ProgramResult{
		Collection: map[string]any{
			"items": []any{
				map[string]any{"title": "Job A", "source_url": target},
			},
		},
		Requests: []acquisition.Receipt{
			{Method: http.MethodPost, URL: target, Status: 200, FetchedAt: now},
		},
		SourceEvidence: []acquisition.Response{
			{
				Receipt: acquisition.Receipt{Method: http.MethodPost, URL: target, Status: 200, FetchedAt: now},
				Body:    `{"items":[{"title":"Job A"}]}`,
			},
		},
	}
	binding := RecordBinding{SourceID: "api-post", DataVersion: "v1", TaskID: 1, SchemaVersion: "v1"}
	records, err := BindRecords(result, binding)
	if err != nil || len(records) != 1 || records[0].Fields["title"] != "Job A" {
		t.Fatalf("expected BindRecords to accept POST receipts, got records=%+v err=%v", records, err)
	}
	docs, err := CapturedSourceDocuments(records, result.SourceEvidence)
	if err != nil || len(docs) != 1 {
		t.Fatalf("expected CapturedSourceDocuments to preserve POST source, got docs=%+v err=%v", docs, err)
	}

	deleteResult := result
	deleteResult.Requests = []acquisition.Receipt{{Method: http.MethodDelete, URL: target, Status: 200, FetchedAt: now}}
	if _, err := BindRecords(deleteResult, binding); err == nil {
		t.Fatal("expected unsupported DELETE method to be rejected")
	}
}
