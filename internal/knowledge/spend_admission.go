package knowledge

import (
	"context"
	"fmt"
	"go-lua-crawler/internal/agentdata"
	"gorm.io/gorm"
)

func (i *Indexer) reserveEmbeddingSpend(ctx context.Context, work IndexWork, attempt IndexAttempt, texts []string) (agentdata.Operation, error) {
	if work.OriginRunID == "" {
		if i.RequireSpendPolicy {
			return agentdata.Operation{}, fmt.Errorf("%w: index work needs a funded Run", agentdata.ErrSpendAdmission)
		}
		return agentdata.Operation{}, nil
	}
	data := agentdata.NewStore(i.Store.DB)
	run, err := data.Run(ctx, work.OriginRunID)
	if err != nil {
		return agentdata.Operation{}, err
	}
	budget, err := agentdata.SpendOf(run.BudgetJSON)
	if err != nil {
		return agentdata.Operation{}, err
	}
	if budget == nil {
		if i.RequireSpendPolicy {
			return agentdata.Operation{}, fmt.Errorf("%w: index Run has no frozen spend policy", agentdata.ErrSpendAdmission)
		}
		return agentdata.Operation{}, nil
	}
	provider, ok := i.Embed.(interface {
		QuoteEmbedding([]string, int64) (agentdata.SpendQuote, error)
	})
	if !ok {
		return agentdata.Operation{}, fmt.Errorf("%w: index embedding cannot quote its input", agentdata.ErrSpendAdmission)
	}
	quote, err := provider.QuoteEmbedding(texts, budget.Policy.InputOverhead)
	if err != nil {
		return agentdata.Operation{}, err
	}
	quote.Stage = "index_embedding"
	var op agentdata.Operation
	var fresh bool
	err = i.Store.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := workOwned(tx, work); err != nil {
			return err
		}
		var reserveErr error
		op, fresh, reserveErr = agentdata.NewStore(tx).ReserveIndexSpend(ctx, run.ID, fmt.Sprintf("index-cost:%d", attempt.ID), &quote)
		return reserveErr
	})
	if err != nil {
		return agentdata.Operation{}, err
	}
	if op.ID != 0 && !fresh {
		return op, fmt.Errorf("%w: index request outcome already reserved; do not silently repeat", agentdata.ErrSpendAdmission)
	}
	return op, nil
}
