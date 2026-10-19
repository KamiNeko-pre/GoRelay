package knowledge

import (
	"context"
	"fmt"
	"go-lua-crawler/internal/testdb"
	"testing"
)

func TestModelRetargetCoversBeyondWorkbenchPage(t *testing.T) {
	s := NewStore(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for n := 0; n < 105; n++ {
		doc, _ := NewDocument(Input{Source: "fixture", SourceKey: fmt.Sprint(n), Title: "A", Body: "body", Format: "text"})
		if _, err := s.Put(ctx, doc, "old"); err != nil {
			t.Fatal(err)
		}
	}
	ui, _ := s.Heads(ctx)
	if len(ui) != 100 {
		t.Fatal("protected workbench pagination changed")
	}
	rows, err := s.RetargetModel(ctx, "new")
	if err != nil || rows != 105 {
		t.Fatalf("documents outside workbench page missed: %d %v", rows, err)
	}
	rows, err = s.RetargetModel(ctx, "new")
	if err != nil || rows != 0 {
		t.Fatal("unchanged model rebuilt")
	}
	var h Head
	s.DB.First(&h)
	if err := s.Invalidate(ctx, h.ID); err != nil {
		t.Fatal(err)
	}
	rows, err = s.RetargetModel(ctx, "third")
	if err != nil || rows != 104 {
		t.Fatalf("inactive document resurrected: %d %v", rows, err)
	}
}
