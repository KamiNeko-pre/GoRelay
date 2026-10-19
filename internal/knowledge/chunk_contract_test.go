package knowledge

import (
	"strings"
	"testing"
)

func TestChunkLayoutOmitsWhitespaceAndPreservesOriginalOffsets(t *testing.T) {
	for _, space := range []string{strings.Repeat("\n\t ", 400), strings.Repeat("\u3000\r\n", 400)} {
		body := "第一段有效正文" + space + "末尾有效正文"
		doc := Document{ID: "doc", Revision: "revision", Body: body}
		chunks := SplitDocument(doc, 300, 40, "generation")
		if len(chunks) == 0 {
			t.Fatal("nonempty document lost")
		}
		for n, c := range chunks {
			if strings.TrimSpace(c.Text) == "" {
				t.Fatalf("empty embedding input emitted at %d:%d", c.Start, c.End)
			}
			if c.Text != string([]rune(body)[c.Start:c.End]) || c.Number != n {
				t.Fatal("original evidence offsets or manifest numbering changed")
			}
		}
		if strings.TrimSpace(chunks[0].Text) != "第一段有效正文" || !strings.HasSuffix(chunks[len(chunks)-1].Text, "末尾有效正文") {
			t.Fatal("edge content lost")
		}
	}
	if chunks := SplitDocument(Document{ID: "empty", Body: strings.Repeat("\n\t", 400)}, 300, 40, "g"); len(chunks) != 0 {
		t.Fatal("whitespace-only document emitted chunks")
	}
	// Protected old layout: ordinary text still has the same overlaps and offsets.
	plain := Document{ID: "normal", Body: strings.Repeat("甲乙丙", 220)}
	chunks := SplitDocument(plain, 300, 40, "g")
	if len(chunks) != 3 || chunks[1].Start != 260 || chunks[2].End != 660 {
		t.Fatal("ordinary chunk layout changed")
	}
}
