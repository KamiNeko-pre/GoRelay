package worker

import (
	"context"
	"errors"
	"fmt"
	"go-lua-crawler/internal/engine"
	"time"

	"go-lua-crawler/internal/repository"
	taskmodel "go-lua-crawler/internal/task"

	"github.com/redis/go-redis/v9"
)

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
	execute := engine.ExecuteTask
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
		processor.recordEvent(ctx, crawlTask.ID, taskmodel.EventStageAcked, taskmodel.EventLevelInfo, "消息确认", "终态任务无需重复执行，Redis 消息已确认")
		return nil
	}
	// Reserve ten seconds for completion inside the lease. Acquisition time
	// consumes this absolute budget instead of granting a fresh lease afterward.
	executionDeadline := time.Now().Add(processor.config.ExecutionTimeout + 5*time.Second)
	leaseDuration := processor.config.ExecutionTimeout + 15*time.Second
	if crawlTask.ProgramHash != "" && crawlTask.ExecutionDeadline != nil {
		executionDeadline = *crawlTask.ExecutionDeadline
		leaseDuration = max(time.Second, time.Until(executionDeadline)+10*time.Second)
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
	processor.recordEvent(ctx, crawlTask.ID, taskmodel.EventStageClaimed, taskmodel.EventLevelInfo, "获取任务", "Worker 已获取任务租约")
	execCtx, cancel := context.WithDeadline(ctx, executionDeadline)
	if err := execCtx.Err(); err != nil {
		cancel()
		return err
	}
	persist, executionErr := processor.runExecution(execCtx, crawlTask)
	cancel()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	status := taskmodel.StatusSucceeded
	failureCode, lastError := "", ""
	if executionErr != nil {
		status, failureCode, lastError = taskmodel.StatusFailed, "lua_execution", executionErr.Error()
		if errors.Is(executionErr, context.DeadlineExceeded) {
			failureCode = "lua_timeout"
		}
		processor.recordEvent(ctx, crawlTask.ID, taskmodel.EventStageFailed, taskmodel.EventLevelError, failureCode, lastError)
	} else {
		processor.recordEvent(ctx, crawlTask.ID, taskmodel.EventStageLuaFinished, taskmodel.EventLevelInfo, "lua_finished", "Lua 规则执行完成，结果等待事务提交")
	}
	commitCtx, stop := context.WithTimeout(ctx, 10*time.Second)
	defer stop()
	if err := repository.CompleteCrawlTask(commitCtx, crawlTask.ID, status, failureCode, lastError, time.Now(), token, persist); err != nil {
		return fmt.Errorf("complete task %d: %w", crawlTask.ID, err)
	}
	if status == taskmodel.StatusSucceeded {
		processor.recordEvent(ctx, crawlTask.ID, taskmodel.EventStageMySQLCommitted, taskmodel.EventLevelInfo, "mysql_committed", "结果快照与任务状态已提交")
		processor.recordEvent(ctx, crawlTask.ID, taskmodel.EventStageCompleted, taskmodel.EventLevelInfo, "completed", "任务已完成")
	} else {
		processor.recordEvent(ctx, crawlTask.ID, taskmodel.EventStageMySQLCommitted, taskmodel.EventLevelInfo, "mysql_committed", "失败状态与错误信息已提交")
	}
	if err := processor.ack(ctx, message.ID); err != nil {
		return err
	}
	processor.recordEvent(ctx, crawlTask.ID, taskmodel.EventStageAcked, taskmodel.EventLevelInfo, "acked", "Redis 消息已确认")
	return nil
}

func (processor *Processor) runExecution(ctx context.Context, task taskmodel.CrawlTask) (persist repository.PersistTaskResult, err error) {
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
