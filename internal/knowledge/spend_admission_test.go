package knowledge

import (
	"context"
	"go-lua-crawler/internal/agentdata"
	task "go-lua-crawler/internal/task"
	"go-lua-crawler/internal/testdb"
	"testing"
)

type pricedEmbed struct{ calls int }

func (p *pricedEmbed) EmbedTexts(context.Context, []string) ([][]float32, error) {
	p.calls++
	return [][]float32{{1, 0}}, nil
}
func TestNativeIndexDoesNotNeedPriceAdmission(t *testing.T) {
	ctx := context.Background()
	db := testdb.Open(t)
	if err := agentdata.NewStore(db).Migrate(); err != nil {
		t.Fatal(err)
	}
	store := NewStore(db)
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	child := task.CrawlTask{RequestKey: "native-index-no-budget"}
	if err := db.Create(&child).Error; err != nil {
		t.Fatal(err)
	}
	doc, err := NewDocument(Input{Source: "fixture", SourceKey: "row", Title: "evidence", Body: "a useful paragraph", Format: "text", TaskID: child.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Put(ctx, doc, "fp"); err != nil {
		t.Fatal(err)
	}
	embed := &pricedEmbed{}
	index := Indexer{Store: store, Embed: embed, Vectors: &testVectors{points: map[string]VectorPoint{}}, Fingerprint: "fp"}
	if err = index.Once(ctx); err != nil || embed.calls != 1 {
		t.Fatalf("native indexing failed: %v, calls=%d", err, embed.calls)
	}
	head, err := store.Head(ctx, doc.ID)
	if err != nil || head.Generation == "" {
		t.Fatalf("native index not published: %v", err)
	}
}
