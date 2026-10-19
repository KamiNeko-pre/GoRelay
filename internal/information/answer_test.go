package information

import (
	"context"
	"testing"

	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/knowledge"
)

func TestCitedAnswerUsesOnlyCurrentDeliveredRecordEvidence(t *testing.T) {
	rows := []aiapp.Record{{Key: "item-A", Provenance: aiapp.Provenance{TaskID: 51, DataVersion: "asset-v2", URL: "https://source.example/items"}}}
	for _, tc := range []struct {
		name string
		head knowledge.Head
		want bool
	}{
		{"current", knowledge.Head{ID: "a", Active: true, TaskID: 51, RecordKey: "item-A", DataVersion: "asset-v2", URL: "https://source.example/items"}, true},
		{"old-version", knowledge.Head{ID: "b", Active: true, TaskID: 51, RecordKey: "item-A", DataVersion: "asset-v1", URL: "https://source.example/items"}, false},
		{"other-task", knowledge.Head{ID: "c", Active: true, TaskID: 52, RecordKey: "item-A", DataVersion: "asset-v2", URL: "https://source.example/items"}, false},
		{"revoked", knowledge.Head{ID: "d", Active: false, TaskID: 51, RecordKey: "item-A", DataVersion: "asset-v2", URL: "https://source.example/items"}, false},
		{"asset-description", knowledge.Head{ID: "e", Active: true, TaskID: 0, RecordKey: "item-A", DataVersion: "asset-v2", URL: "https://source.example/items"}, false},
		{"outside-scope", knowledge.Head{ID: "f", Active: true, TaskID: 51, RecordKey: "item-A", DataVersion: "asset-v2", URL: "https://other.example/items"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := recordHeadMatches(tc.head, rows, []string{"source.example"}); got != tc.want {
				t.Fatalf("matched=%v want=%v", got, tc.want)
			}
		})
	}
}

func TestAnswerRejectsForeignCitationBeforeReadingSQL(t *testing.T) {
	answer := aiapp.RAGAnswer{Status: "answered", Answer: "an observed fact", Citations: []string{"invented"}, Evidence: knowledge.EvidenceSet{Entries: []knowledge.Evidence{}}}
	if err := aiapp.ValidateCitedAnswer(context.Background(), nil, &answer, false); err == nil {
		t.Fatal("invented citation accepted")
	}
}
