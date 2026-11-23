package knowledge

import (
	"testing"
)

func TestAcquisitionDocumentEvidenceDoesNotRequireIndex(t *testing.T) {
	for _, body := range []string{`{"provider":"alpha","cost":100}`, `{"publisher":"beta","version":"2.1"}`, `{"公告":"发布新版本"}`} {
		doc, err := NewDocument(Input{Source: "observed", SourceKey: body, Title: "source facts", URL: "https://source.example/facts", Body: body, Format: "text", TaskID: 7, RecordKey: "record", DataVersion: "v1"})
		if err != nil {
			t.Fatal(err)
		}
		head := Head{ID: doc.ID, Revision: doc.Revision, Active: true}
		first, err := DocumentEvidence(doc, head)
		if err != nil || first.Text != body || !matchesCurrentHead(first, head) {
			t.Fatalf("saved source cannot support delivery before indexing: %+v %v", first, err)
		}
		if first.End != len([]rune(body)) {
			t.Fatal("source offset changed from rune units to bytes")
		}
		head.Generation = "later-background-index"
		second, err := DocumentEvidence(doc, head)
		if err != nil || first.ID != second.ID || !matchesCurrentHead(first, head) {
			t.Fatal("index publication invalidated unchanged source evidence")
		}
		head.Revision = "superseded"
		if matchesCurrentHead(first, head) {
			t.Fatal("obsolete source revision remained valid")
		}
		if _, err := DocumentEvidence(doc, head); err == nil {
			t.Fatal("superseded source accepted")
		}
		head.Revision, head.Active = doc.Revision, false
		if matchesCurrentHead(first, head) {
			t.Fatal("revoked source remained valid")
		}
	}
}
