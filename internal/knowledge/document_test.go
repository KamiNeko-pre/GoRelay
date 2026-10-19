package knowledge

import (
	"strings"
	"testing"
)

func TestMarkdownImportPreservesBodyAndStableIdentity(t *testing.T) {
	input := Input{Source: "manual", SourceKey: "context-notes", Format: "markdown", Body: "# Context 笔记\r\n\r\n取消信号向子操作传播。\r\n\r\n```go\r\nctx, cancel := context.WithCancel(parent)\r\n```\r\n"}
	first, err := NewDocument(input)
	if err != nil {
		t.Fatal(err)
	}
	if first.Title != "Context 笔记" || strings.Contains(first.Body, "\r") || !strings.Contains(first.Body, "```go\nctx, cancel") {
		t.Fatalf("title/body were not preserved: %#v", first)
	}
	input.Body += "\r\n保留全文，不用摘要替代。"
	second, err := NewDocument(input)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || first.Revision == second.Revision {
		t.Fatal("same source must keep ID while changed content gets a new revision")
	}
	duplicate, err := NewDocument(input)
	if err != nil || second != duplicate {
		t.Fatalf("identical import must be deterministic: %v", err)
	}
}

func TestDocumentRejectsMissingProvenanceAndBody(t *testing.T) {
	cases := []Input{
		{SourceKey: "notes", Title: "notes", Body: "body", Format: "text"},
		{Source: "manual", Title: "notes", Body: "body", Format: "text"},
		{Source: "manual", SourceKey: "notes", Title: "notes", Body: "  \n", Format: "text"},
		{Source: "web", SourceKey: "page", URL: "javascript:alert(1)", Title: "notes", Body: "body", Format: "text"},
		{Source: "web", SourceKey: "page", URL: "https:///missing-host", Title: "notes", Body: "body", Format: "text"},
		{Source: "manual", SourceKey: "notes", Body: "no title", Format: "text"},
		{Source: "manual", SourceKey: "notes", Title: "notes", Body: "body", Format: "html"},
		{Source: "manual", SourceKey: "notes", Title: "notes", Body: string([]byte{0xff}), Format: "text"},
	}
	for index, input := range cases {
		document, err := NewDocument(input)
		if err == nil {
			t.Errorf("case %d accepted invalid document", index)
		} else if document != (Document{}) {
			t.Errorf("case %d returned a partial document on error", index)
		}
	}
}

func TestDocumentHasExplicitSizeLimit(t *testing.T) {
	input := Input{Source: "manual", SourceKey: "large", Title: "large", Body: strings.Repeat("x", MaxBodyBytes+1), Format: "text"}
	if _, err := NewDocument(input); err == nil {
		t.Fatal("oversized body accepted")
	}
}

func TestDocumentIdentitySeparatesSourcesAndKeys(t *testing.T) {
	input := Input{Source: "manual", SourceKey: "notes", Title: "Notes", Body: "same body", Format: "text"}
	first, err := NewDocument(input)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []struct {
		name string
		edit func(*Input)
	}{
		{"different_source", func(in *Input) { in.Source = "go-blog" }},
		{"different_key", func(in *Input) { in.SourceKey = "another-note" }},
	} {
		t.Run(change.name, func(t *testing.T) {
			other := input
			change.edit(&other)
			document, err := NewDocument(other)
			if err != nil {
				t.Fatal(err)
			}
			if document.ID == first.ID {
				t.Fatal("different source identity collapsed into one document")
			}
		})
	}
}

func TestDocumentRevisionTracksEvidenceChanges(t *testing.T) {
	input := Input{Source: "manual", SourceKey: "notes", Title: "Notes", Body: "body", Format: "text", URL: "https://example.com/notes"}
	first, err := NewDocument(input)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []struct {
		name string
		edit func(*Input)
	}{
		{"title", func(in *Input) { in.Title = "Updated notes" }},
		{"body", func(in *Input) { in.Body = "updated body" }},
		{"format", func(in *Input) { in.Format = "markdown" }},
		{"citation_url", func(in *Input) { in.URL = "https://example.com/notes-v2" }},
	} {
		t.Run(change.name, func(t *testing.T) {
			updated := input
			change.edit(&updated)
			document, err := NewDocument(updated)
			if err != nil {
				t.Fatal(err)
			}
			if document.ID != first.ID || document.Revision == first.Revision {
				t.Fatal("evidence update must preserve identity and change revision")
			}
		})
	}
}

func TestDocumentIncludesNormalizedEvidence(t *testing.T) {
	input := Input{Source: " manual ", SourceKey: " notes ", Title: " Notes ", Body: " first\r\n    code ", Format: "text", URL: " https://example.com/notes "}
	prepared, err := NormalizeInput(input)
	if err != nil {
		t.Fatal(err)
	}
	document, err := NewDocument(input)
	if err != nil {
		t.Fatal(err)
	}
	if document.ID == "" || document.Revision == "" || document.Source != prepared.Source || document.SourceKey != prepared.SourceKey || document.URL != prepared.URL || document.Title != prepared.Title || document.Body != prepared.Body || document.Format != prepared.Format {
		t.Fatalf("document lost normalized evidence or identifiers: %#v", document)
	}
}
