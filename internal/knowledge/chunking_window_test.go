package knowledge

import (
	"context"
	"fmt"
	"go-lua-crawler/internal/testdb"
	"strings"
	"testing"
	"unicode/utf8"
)

type limitedWindowEmbed struct{ count int }

func (e *limitedWindowEmbed) EmbedTexts(_ context.Context, texts []string) ([][]float32, error) {
	vectors := make([][]float32, len(texts))
	for n, text := range texts {
		if utf8.RuneCountInString(text) > 300 {
			return nil, fmt.Errorf("window exceeds configured capacity")
		}
		e.count++
		vectors[n] = []float32{1, 0}
	}
	return vectors, nil
}

func TestIndexerHonorsConfiguredWindowsAndKeepsWholeBody(t *testing.T) {
	for _, body := range []string{strings.Repeat("计算机就业资料。", 120), strings.Repeat("another independent mixed 文本 ", 50)} {
		store := NewStore(testdb.Open(t))
		if err := store.Migrate(); err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		doc, err := NewDocument(Input{Source: "window", SourceKey: "doc", Title: "evidence", Body: body, Format: "text"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = store.Put(ctx, doc, "window-300-40"); err != nil {
			t.Fatal(err)
		}
		embed := &limitedWindowEmbed{}
		indexer := Indexer{Store: store, Embed: embed, Vectors: &testVectors{points: map[string]VectorPoint{}}, Fingerprint: "window-300-40", ChunkSize: 300, ChunkOverlap: 40}
		if err = indexer.Once(ctx); err != nil {
			t.Fatal(err)
		}
		head, err := store.Head(ctx, doc.ID)
		if err != nil || !head.Active || head.Generation == "" {
			t.Fatal(head, err)
		}
		chunks, err := store.GenerationChunks(ctx, head.Generation)
		if err != nil || len(chunks) != embed.count {
			t.Fatal(chunks, err)
		}
		covered := make([]bool, len([]rune(body)))
		for _, chunk := range chunks {
			if chunk.Text != string([]rune(body)[chunk.Start:chunk.End]) {
				t.Fatal("text changed")
			}
			for n := chunk.Start; n < chunk.End; n++ {
				covered[n] = true
			}
		}
		for _, ok := range covered {
			if !ok {
				t.Fatal("body truncated")
			}
		}
	}
}
