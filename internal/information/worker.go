package information

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/agentstatus"
	"go-lua-crawler/internal/aiapp"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func (s *Service) Cancel(ctx context.Context, id string) error {
	var request Request
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&request, "id = ?", id).Error; err != nil {
			return err
		}
		if request.Stage == "cancelled" {
			return nil
		}
		if terminal(request.Stage) {
			return agentdata.ErrConflict
		}
		return tx.Model(&Request{}).Where("id = ?", id).Updates(map[string]any{"stage": "cancelled", "lease_token": "", "lease_until": nil, "status_json": string(mustMarshal(agentstatus.Of(agentstatus.TaskCancelled)))}).Error
	})
	if err != nil {
		return err
	}
	if request.ChildRunID != "" {
		if err := s.Crawler.CancelWithDelivery(ctx, request.ChildRunID); err != nil && !errors.Is(err, agentdata.ErrConflict) {
			return err
		}
	}
	return nil
}

// Run schedules only upper-layer requests. Existing agent-worker, indexer and
// crawl workers retain ownership of Lua exploration, indexing and execution.
func (s *Service) Run(ctx context.Context) error {
	if s.DB == nil {
		return fmt.Errorf("information worker database unavailable")
	}
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() { cancel(); wg.Wait() }()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	slots := make(chan struct{}, 2)
	active := map[string]bool{}
	var mu sync.Mutex
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			var pending []Request
			if err := s.DB.WithContext(ctx).Where("stage NOT IN ? AND (lease_until IS NULL OR lease_until <= ?)", []string{"completed", "failed", "cancelled", "expired"}, time.Now()).Order("created_at").Limit(20).Find(&pending).Error; err != nil {
				return err
			}
			for _, request := range pending {
				mu.Lock()
				busy := active[request.ID]
				mu.Unlock()
				if busy {
					continue
				}
				select {
				case slots <- struct{}{}:
				default:
					continue
				}
				mu.Lock()
				active[request.ID] = true
				mu.Unlock()
				wg.Add(1)
				go func(id string) {
					defer wg.Done()
					defer func() { <-slots; mu.Lock(); delete(active, id); mu.Unlock() }()
					if err := s.Advance(ctx, id); err != nil && !errors.Is(err, agentdata.ErrConflict) {
						status := aiapp.OperationalStatus(err)
						if status.SystemLog {
							log.Printf("information request=%s internal_code=%d owner=%s error=%v", id, status.Code, status.Owner, err)
						}
					}
				}(request.ID)
			}
		}
	}
}
