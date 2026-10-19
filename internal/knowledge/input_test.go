package knowledge

import (
	"strings"
	"testing"
)

func TestNormalizeInputPreservesEvidence(t *testing.T) {
	raw := Input{
		Source: " manual ", SourceKey: " context-notes ", URL: " https://go.dev/blog/context ",
		Format: "markdown", Body: "\r\n# Context 笔记\r\n\r\n```go\r\nselect {\r\n    case <-ctx.Done():\r\n        return\r\n}\r\n```\r\n",
	}
	wantBody := "# Context 笔记\n\n```go\nselect {\n    case <-ctx.Done():\n        return\n}\n```"
	got, err := NormalizeInput(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != "manual" || got.SourceKey != "context-notes" || got.URL != "https://go.dev/blog/context" || got.Title != "Context 笔记" || got.Body != wantBody || got.Format != "markdown" {
		t.Fatalf("wrong normalized evidence: %#v", got)
	}
	if raw.Source != " manual " || !strings.Contains(raw.Body, "\r\n") {
		t.Fatal("normalization changed the caller's input")
	}
	again, err := NormalizeInput(got)
	if err != nil || again != got {
		t.Fatalf("normalization must be idempotent: %#v, %v", again, err)
	}
}

func TestNormalizeInputKeepsExplicitTitleAndPlainText(t *testing.T) {
	input := Input{Source: "manual", SourceKey: "notes", Title: " Explicit title ", Body: "first\rsecond\r\nthird\n    code indentation", Format: "text"}
	got, err := NormalizeInput(input)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "Explicit title" || got.Body != "first\nsecond\nthird\n    code indentation" {
		t.Fatalf("lost title or paragraph/code structure: %#v", got)
	}
}

func TestNormalizeInputPreservesIndentedBoundaryLines(t *testing.T) {
	for _, tc := range []struct {
		name   string
		format string
		body   string
		want   string
	}{
		{"plain_text", "text", " \r\n    first line\r\n\tsecond line  \r\n \r\n", "    first line\n\tsecond line  "},
		{"indented_markdown_code", "markdown", "\r\n    ctx, cancel := context.WithCancel(parent)\r\n    defer cancel()\r\n", "    ctx, cancel := context.WithCancel(parent)\n    defer cancel()"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := Input{Source: "manual", SourceKey: tc.name, Title: "Code evidence", Body: tc.body, Format: tc.format}
			got, err := NormalizeInput(input)
			if err != nil {
				t.Fatal(err)
			}
			if got.Body != tc.want {
				t.Fatalf("boundary whitespace erased code evidence: got %q, want %q", got.Body, tc.want)
			}
		})
	}
}

func TestNormalizeInputRejectsInvalidDocuments(t *testing.T) {
	valid := Input{Source: "manual", SourceKey: "notes", Title: "Notes", Body: "body", Format: "text"}
	cases := []struct {
		name string
		edit func(*Input)
	}{
		{"missing_source", func(in *Input) { in.Source = " " }},
		{"missing_source_key", func(in *Input) { in.SourceKey = "\t" }},
		{"source_limit", func(in *Input) { in.Source = strings.Repeat("s", 129) }},
		{"source_key_limit", func(in *Input) { in.SourceKey = strings.Repeat("k", 1025) }},
		{"empty_body", func(in *Input) { in.Body = " \r\n " }},
		{"body_limit", func(in *Input) { in.Body = strings.Repeat("x", MaxBodyBytes+1) }},
		{"body_encoding", func(in *Input) { in.Body = string([]byte{0xff}) }},
		{"missing_title", func(in *Input) { in.Title = " " }},
		{"title_limit", func(in *Input) { in.Title = strings.Repeat("t", 513) }},
		{"title_encoding", func(in *Input) { in.Title = string([]byte{0xff}) }},
		{"unextracted_html", func(in *Input) { in.Format = "html" }},
		{"unsafe_scheme", func(in *Input) { in.URL = "javascript:alert(1)" }},
		{"missing_host", func(in *Input) { in.URL = "https:///missing-host" }},
		{"credentials_in_url", func(in *Input) { in.URL = "https://user:password@example.com/article" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := valid
			tc.edit(&input)
			got, err := NormalizeInput(input)
			if err == nil || got != (Input{}) {
				t.Fatalf("invalid input must fail without a partial value: %#v, %v", got, err)
			}
		})
	}
}

func TestNormalizeInputAcceptsBodyAtLimit(t *testing.T) {
	input := Input{Source: "manual", SourceKey: "large", Title: "Large", Body: strings.Repeat("x", MaxBodyBytes), Format: "text"}
	got, err := NormalizeInput(input)
	if err != nil || len(got.Body) != MaxBodyBytes {
		t.Fatalf("exact size boundary rejected: %d, %v", len(got.Body), err)
	}
}
