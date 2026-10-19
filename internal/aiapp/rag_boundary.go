package aiapp

import (
	"context"
	"go-lua-crawler/internal/knowledge"
)

// ValidateCitedAnswer shares existing SQL reference and excerpt checks with an
// upper information frontend; it introduces no RAG tool into the crawler loop.
func ValidateCitedAnswer(ctx context.Context, store *knowledge.Store, answer *RAGAnswer, extractive bool) error {
	if err := validateRAGAnswer(ctx, store, *answer); err != nil {
		return err
	}
	answer.SupportLevel = "references_only_analysis_unverified"
	if extractive {
		return checkExtractiveSummary(answer)
	}
	return nil
}
