package aiapp

import (
	"context"
	"go-lua-crawler/internal/knowledge"
)

// ValidateCitedAnswer shares existing SQL reference checks with the frontend.
func ValidateCitedAnswer(ctx context.Context, store *knowledge.Store, answer *RAGAnswer, _ bool) error {
	if err := validateRAGAnswer(ctx, store, *answer); err != nil {
		return err
	}
	answer.SupportLevel = "references_verified"
	return nil
}
