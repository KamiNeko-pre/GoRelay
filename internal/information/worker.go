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
		if request.Status == "cancelled" {
			return nil
		}
		if terminal(request.Status) {
			return agentdata.ErrConflict
		}
		return tx.Model(&Request{}).Where("id = ?", id).Updates(map[string]any{"stage": "cancelled", "status": "cancelled", "lease_token": "", "lease_until": nil, "status_json": string(mustMarshal(agentstatus.Of(agentstatus.TaskCancelled)))}).Error
	})
	if err != nil {
		return err
	}
	return s.cancelDiscoveryWork(ctx, request)
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
			if err := s.SweepMonitors(ctx); err != nil {
				log.Printf("information monitor scheduling error=%v", err)
			}
			pending, err := s.readyRequests(ctx, 20)
			if err != nil {
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

// Page through waiting requests before applying the runnable limit. A pending
// dependency must never consume the whole scheduling window of a newer child.
func (s *Service) readyRequests(ctx context.Context, limit int) ([]Request, error) {
	ready := make([]Request, 0, limit)
	var after *Request
	minValidDeadline := time.Unix(1, 0).UTC()
	for len(ready) < limit {
		now := time.Now()
		query := s.DB.WithContext(ctx).Where(
			"(lease_until IS NULL OR lease_until <= ?) AND (status IN ? OR (status = ? AND deadline IS NOT NULL AND deadline >= ? AND deadline <= ?))",
			now,
			[]string{"queued", "running", "waiting_dependency"},
			"needs_input",
			minValidDeadline,
			now,
		).Order("created_at,id").Limit(limit)
		if after != nil {
			query = query.Where("created_at > ? OR (created_at = ? AND id > ?)", after.CreatedAt, after.CreatedAt, after.ID)
		}
		var page []Request
		if err := query.Find(&page).Error; err != nil {
			return nil, err
		}
		for _, r := range page {
			ok, err := s.informationReady(ctx, r)
			if err != nil {
				log.Printf("information readiness request=%s error=%v", r.ID, err)
				continue
			}
			if ok {
				ready = append(ready, r)
				if len(ready) == limit {
					return ready, nil
				}
			}
		}
		if len(page) < limit {
			break
		}
		last := page[len(page)-1]
		after = &last
	}
	return ready, nil
}
