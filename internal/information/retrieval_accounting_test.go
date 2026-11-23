package information

import (
	"context"

	"go-lua-crawler/internal/knowledge"
)

type measuredLookupEmbedder struct{}

func (measuredLookupEmbedder) EmbedTexts(context.Context, []string) ([][]float32, error) {
	return [][]float32{{1, 0}}, nil
}
func (measuredLookupEmbedder) EmbedMeasured(context.Context, []string) (knowledge.EmbeddingMeasurement, error) {
	return knowledge.EmbeddingMeasurement{Vectors: [][]float32{{1, 0}}, Model: "local-test-embed", Usage: &knowledge.EmbeddingUsage{PromptTokens: 17, TotalTokens: 17}}, nil
}
