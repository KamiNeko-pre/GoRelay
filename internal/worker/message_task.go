package worker

import (
	"context"
	"fmt"
	"strconv"

	"github.com/redis/go-redis/v9"
	"gorelay/internal/repository"
	taskmodel "gorelay/internal/task"
)

// loadCrawlTaskFromMessage will be shared by new-message and recovery handling.
// It only validates the task reference and loads MySQL state; it does not ACK.
func loadCrawlTaskFromMessage(
	ctx context.Context,
	message redis.XMessage,
) (taskmodel.CrawlTask, error) {
	rawTaskID, exists := message.Values["task_id"]
	if !exists {
		return taskmodel.CrawlTask{}, fmt.Errorf("message %q: task_id is missing", message.ID)
	}
	taskIDText, ok := rawTaskID.(string)
	if !ok {
		return taskmodel.CrawlTask{}, fmt.Errorf("message %q: task_id must be a string, got %T", message.ID, rawTaskID)
	}
	taskID, err := strconv.ParseUint(taskIDText, 10, 64)
	if err != nil {
		return taskmodel.CrawlTask{}, fmt.Errorf("message %q: parse task_id: %w", message.ID, err)
	}
	if taskID == 0 {
		return taskmodel.CrawlTask{}, fmt.Errorf("message %q: task_id must be positive", message.ID)
	}
	crawlTask, err := repository.GetCrawlTask(ctx, taskID)
	if err != nil {
		return taskmodel.CrawlTask{}, fmt.Errorf("message %q: load task %d: %w", message.ID, taskID, err)
	}
	return crawlTask, nil
}
