package knowledge

import "testing"

func TestLexicalIndexRanksExactAndHanMatches(t *testing.T) {
	idx := BuildLexicalIndex([]LexicalDocument{
		{ID: "doc-aws", Text: "AWS Lambda pricing update and invocation billing details"},
		{ID: "doc-go", Text: "Go 1.26 release notes and runtime memory improvements 版本发布"},
	})
	hits := idx.Search("Go 1.26 版本发布", 5)
	if len(hits) != 1 || hits[0].ID != "doc-go" {
		t.Fatalf("unexpected lexical hits: %+v", hits)
	}
}
