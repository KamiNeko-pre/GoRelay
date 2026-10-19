package knowledge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go-lua-crawler/internal/agentdata"
	task "go-lua-crawler/internal/task"
	"go-lua-crawler/internal/testdb"
	"testing"
	"time"
)

type pricedEmbed struct{ calls int }

func (p *pricedEmbed) EmbedTexts(context.Context, []string) ([][]float32, error) {
	p.calls++
	return [][]float32{{1, 0}}, nil
}
func (p *pricedEmbed) QuoteEmbedding(texts []string, overhead int64) (agentdata.SpendQuote, error) {
	n := overhead
	for _, s := range texts {
		n += int64(len(s))
	}
	return agentdata.SpendQuote{Stage: "embedding", Model: "fixture", InputTokens: n, Basis: "fixture byte bound"}, nil
}

func TestIndexEmbeddingAdmissionPrecedesProviderCall(t *testing.T) {
	for _, limit := range []int64{5, 1000} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			db := testdb.Open(t)
			data := agentdata.NewStore(db)
			if err := data.Migrate(); err != nil {
				t.Fatal(err)
			}
			store := NewStore(db)
			if err := store.Migrate(); err != nil {
				t.Fatal(err)
			}
			zero := int64(0)
			policy := agentdata.SpendBudget{Policy: agentdata.SpendPolicy{MaxTokens: limit, MaxMicrounits: 1000, Currency: "fixture", PriceDate: "2026-10-09", Models: map[string]agentdata.SpendRate{"fixture": {Input: &zero}}}}
			budget, _ := json.Marshal(map[string]any{"spend": policy})
			run, err := data.CreateRun(context.Background(), "index-spend", "index acquired evidence", "{}", string(budget), time.Now().Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			child := task.CrawlTask{RequestKey: "index-source", RunID: run.ID}
			if err = db.Create(&child).Error; err != nil {
				t.Fatal(err)
			}
			doc, err := NewDocument(Input{Source: "fixture", SourceKey: "row", Title: "evidence", Body: "a useful source paragraph", Format: "text", TaskID: child.ID})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = store.Put(context.Background(), doc, "fp"); err != nil {
				t.Fatal(err)
			}
			embed := &pricedEmbed{}
			index := Indexer{Store: store, Embed: embed, Vectors: &testVectors{points: map[string]VectorPoint{}}, Fingerprint: "fp", RequireSpendPolicy: true}
			err = index.Once(context.Background())
			if limit == 5 {
				if !errors.Is(err, agentdata.ErrSpendAdmission) || embed.calls != 0 {
					t.Fatalf("charged before index admission: %v %d", err, embed.calls)
				}
			} else {
				if err != nil || embed.calls != 1 {
					t.Fatalf("funded index did not execute: %v %d", err, embed.calls)
				}
			}
		})
	}
}
