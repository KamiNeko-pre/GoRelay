package worker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"gorelay/internal/fetchgov"
	"gorelay/internal/pipeline"
	"gorelay/internal/programrunner"
	"gorelay/internal/repository"
	taskmodel "gorelay/internal/task"

	"github.com/redis/go-redis/v9"
)

var activeCancels sync.Map // map[uint64]context.CancelFunc

// CancelActiveTask cancels an in-flight local task execution immediately.
func CancelActiveTask(taskID uint64) bool {
	if fn, ok := activeCancels.Load(taskID); ok {
		fn.(context.CancelFunc)()
		return true
	}
	return false
}

// Processor owns task-state decisions and acknowledgements after delivery.
type Processor struct {
	client  *redis.Client
	config  Config
	gate    chan struct{}
	execute func(context.Context, taskmodel.CrawlTask) (repository.PersistTaskResult, error)
}

// NewProcessor only stores validated dependencies; it does not process messages.
func NewProcessor(client *redis.Client, cfg Config, executors ...func(context.Context, taskmodel.CrawlTask) (repository.PersistTaskResult, error)) (*Processor, error) {
	if client == nil {
		return nil, fmt.Errorf("Redis client is required")
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate processor config: %w", err)
	}
	if cfg.ExecutionTimeout <= 0 {
		cfg.ExecutionTimeout = 30 * time.Second
	}
	var execute func(context.Context, taskmodel.CrawlTask) (repository.PersistTaskResult, error)
	if len(executors) > 0 && executors[0] != nil {
		execute = executors[0]
	}
	return &Processor{client: client, config: cfg, gate: make(chan struct{}, 1), execute: execute}, nil
}

// ProcessNewMessage handles a message read with the special ">" stream ID.
func (processor *Processor) ProcessNewMessage(ctx context.Context, message redis.XMessage) error {
	return processor.process(ctx, message, false)
}

func (processor *Processor) process(ctx context.Context, message redis.XMessage, reclaim bool) error {
	// Normal delivery and recovery share one execution slot per worker.
	select {
	case processor.gate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-processor.gate }()
	loadCtx, stopLoad := context.WithTimeout(ctx, 5*time.Second)
	crawlTask, err := loadCrawlTaskFromMessage(loadCtx, message)
	stopLoad()
	if err != nil {
		return err
	}
	if crawlTask.Status.IsTerminal() {
		if err := processor.ack(ctx, message.ID); err != nil {
			return err
		}
		processor.recordEvent(ctx, crawlTask.ID, taskmodel.EventStageAcked, taskmodel.EventLevelInfo, "acked", "终态任务无需重复执行，Redis 消息已确认")
		return nil
	}
	// Reserve completion headroom inside the lease unless the task explicitly
	// specifies an execution deadline (in which case lease tracks ClaimMinIdle
	// and is renewed periodically while active).
	executionDeadline := time.Now().Add(processor.config.ExecutionTimeout + 5*time.Second)
	leaseDuration := processor.config.ExecutionTimeout + 15*time.Second
	if crawlTask.SourceID != "" && crawlTask.ExecutionDeadline != nil {
		executionDeadline = *crawlTask.ExecutionDeadline
		leaseDuration = processor.config.ClaimMinIdle
	}
	claimCtx, stopClaim := context.WithTimeout(ctx, 5*time.Second)
	token, err := repository.ClaimCrawlTask(claimCtx, crawlTask, leaseDuration, reclaim)
	stopClaim()
	if errors.Is(err, repository.ErrTaskTransitionConflict) {
		// Another attempt owns the task. Keep this message pending; recovery
		// will ACK it after terminal commit or adopt it after lease expiry.
		return nil
	}
	if err != nil {
		return fmt.Errorf("claim task %d: %w", crawlTask.ID, err)
	}
	crawlTask.RunToken = token
	processor.recordEvent(ctx, crawlTask.ID, taskmodel.EventStageClaimed, taskmodel.EventLevelInfo, "claimed", "Worker 已获取任务租约")
	execCtx, cancel := context.WithCancel(ctx)
	activeCancels.Store(crawlTask.ID, cancel)
	defer activeCancels.Delete(crawlTask.ID)
	if !executionDeadline.IsZero() {
		var deadlineCancel context.CancelFunc
		execCtx, deadlineCancel = context.WithDeadline(execCtx, executionDeadline)
		defer deadlineCancel()
	}
	var stopRenew func() error
	if crawlTask.SourceID != "" {
		stopRenew = processor.renewExecution(execCtx, cancel, crawlTask.ID, token, leaseDuration)
	}
	var persist repository.PersistTaskResult
	executionErr := execCtx.Err()
	if executionErr == nil {
		persist, executionErr = processor.runExecution(execCtx, crawlTask)
	}
	if stopRenew != nil {
		if err := stopRenew(); err != nil {
			cancel()
			return err
		}
	}
	cancel()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	status := taskmodel.StatusSucceeded
	failureCode, lastError := "", ""
	if executionErr != nil {
		status, failureCode, lastError = classifyExecutionError(ctx, crawlTask.ID, executionErr, func(c context.Context, id uint64) bool {
			latest, getErr := repository.GetCrawlTask(c, id)
			return getErr == nil && latest.Cancelled
		})
		processor.recordEvent(ctx, crawlTask.ID, taskmodel.EventStageFailed, taskmodel.EventLevelError, failureCode, lastError)
	} else {
		processor.recordEvent(ctx, crawlTask.ID, taskmodel.EventStageLuaFinished, taskmodel.EventLevelInfo, "connector_finished", "连接器执行完成，结果等待事务提交")
	}
	commitCtx, stop := context.WithTimeout(ctx, 10*time.Second)
	defer stop()
	if err := repository.CompleteCrawlTask(commitCtx, crawlTask.ID, status, failureCode, lastError, time.Now(), token, persist); err != nil {
		return fmt.Errorf("complete task %d: %w", crawlTask.ID, err)
	}
	if err := processor.ack(ctx, message.ID); err != nil {
		return err
	}
	if status == taskmodel.StatusSucceeded {
		processor.recordEvent(ctx, crawlTask.ID, taskmodel.EventStageMySQLCommitted, taskmodel.EventLevelInfo, "mysql_committed", "结果快照与任务状态已提交")
		processor.recordEvent(ctx, crawlTask.ID, taskmodel.EventStageCompleted, taskmodel.EventLevelInfo, "completed", "任务已完成")
	} else {
		processor.recordEvent(ctx, crawlTask.ID, taskmodel.EventStageMySQLCommitted, taskmodel.EventLevelInfo, "mysql_committed", "失败状态与错误信息已提交")
	}
	processor.recordEvent(ctx, crawlTask.ID, taskmodel.EventStageAcked, taskmodel.EventLevelInfo, "acked", "Redis 消息已确认")
	return nil
}

func classifyExecutionError(
	ctx context.Context,
	taskID uint64,
	err error,
	isCancelled func(context.Context, uint64) bool,
) (taskmodel.Status, string, string) {
	msg := err.Error()
	switch {
	case errors.Is(err, pipeline.ErrSilentDrift):
		return taskmodel.StatusFailed, "silent_drift", msg
	case errors.Is(err, context.DeadlineExceeded):
		return taskmodel.StatusFailed, "timeout", msg
	case errors.Is(err, context.Canceled):
		if isCancelled != nil && isCancelled(ctx, taskID) {
			return taskmodel.StatusCancelled, "cancelled", msg
		}
		return taskmodel.StatusFailed, "cancelled", msg
	}
	var ge *fetchgov.GovernanceError
	if errors.As(err, &ge) && strings.TrimSpace(ge.Code) != "" {
		return taskmodel.StatusFailed, strings.ToLower(strings.TrimSpace(ge.Code)), msg
	}
	var pe *programrunner.Error
	if errors.As(err, &pe) && strings.TrimSpace(pe.Code) != "" {
		return taskmodel.StatusFailed, strings.ToLower(strings.TrimSpace(pe.Code)), msg
	}
	return taskmodel.StatusFailed, "execution_error", msg
}

func (processor *Processor) runExecution(ctx context.Context, task taskmodel.CrawlTask) (persist repository.PersistTaskResult, err error) {
	if processor.execute == nil {
		return nil, fmt.Errorf("task executor is not configured")
	}
	defer func() {
		if value := recover(); value != nil {
			persist = nil
			err = fmt.Errorf("executor panic: %v", value)
		}
	}()
	return processor.execute(ctx, task)
}

func (processor *Processor) recordEvent(ctx context.Context, taskID uint64, stage, level, code, message string) {
	repository.RecordTaskEventBestEffort(ctx, taskmodel.TaskEvent{
		TaskID:  taskID,
		Stage:   stage,
		Level:   level,
		Code:    code,
		Message: message,
	})
}

func (processor *Processor) ack(ctx context.Context, messageID string) error {
	if err := processor.client.XAck(ctx, processor.config.Stream, processor.config.Group, messageID).Err(); err != nil {
		return fmt.Errorf("ack message %q (stream %q, group %q): %w", messageID, processor.config.Stream, processor.config.Group, err)
	}
	return nil
}

// ProcessClaimedMessage handles an idle PEL message obtained by XAUTOCLAIM.
func (processor *Processor) ProcessClaimedMessage(ctx context.Context, message redis.XMessage) error {
	return processor.process(ctx, message, true)
}

func (processor *Processor) renewExecution(ctx context.Context, cancel context.CancelFunc, taskID uint64, token string, lease time.Duration) func() error {
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(max(time.Millisecond, lease/3))
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				done <- nil
				return
			case <-ctx.Done():
				done <- nil
				return
			case <-ticker.C:
				renewCtx, release := context.WithTimeout(ctx, min(5*time.Second, lease/3))
				err := repository.RenewCrawlTaskLease(renewCtx, taskID, token, lease)
				release()
				if err != nil {
					cancel()
					done <- fmt.Errorf("renew task %d ownership: %w", taskID, err)
					return
				}
			}
		}
	}()
	return func() error { close(stop); return <-done }
}
