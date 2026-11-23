package knowledge

import (
	"context"
	"fmt"
	"go-lua-crawler/internal/agentdata"
	"gorm.io/gorm"
)

func (i *Indexer) reserveEmbeddingOperation(ctx context.Context, work IndexWork, attempt IndexAttempt, texts []string) (agentdata.Operation, error) {
	if work.OriginRunID == "" {
		return agentdata.Operation{}, nil
	}
	var op agentdata.Operation
	var fresh bool
	err := i.Store.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := workOwned(tx, work); err != nil {
			return err
		}
		var reserveErr error
		op, fresh, reserveErr = agentdata.NewStore(tx).ReserveIndexOperation(ctx, work.OriginRunID, fmt.Sprintf("index-cost:%d", attempt.ID))
		return reserveErr
	})
	if err != nil {
		return agentdata.Operation{}, err
	}
	if op.ID != 0 && !fresh {
		return op, fmt.Errorf("%w: index request outcome already reserved; do not silently repeat", agentdata.ErrConflict)
	}
	return op, nil
}
