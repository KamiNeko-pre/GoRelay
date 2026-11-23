package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"gorelay/internal/config"
	"gorelay/internal/repository"
	taskmodel "gorelay/internal/task"
	"gorm.io/gorm"
)

// ExecuteTask loads the target rule, owns one Lua VM for the task, and returns
// the result persistence callback for the Worker to commit with the terminal state.
func ExecuteTask(ctx context.Context, task taskmodel.CrawlTask) (repository.PersistTaskResult, error) {
	if task.RuleHash != "" {
		rule, err := repository.GetFrozenRule(ctx, task.RuleHash)
		if err != nil {
			return nil, fmt.Errorf("load frozen rule: %w", err)
		}
		result, err := executeLuaScriptContext(ctx, rule.Code, task.URL, time.Duration(config.Get().Engine.LuaTimeout)*time.Second)
		if err != nil {
			return nil, err
		}
		payload, err := json.Marshal(result)
		if err != nil {
			return nil, err
		}
		return func(tx *gorm.DB) error {
			return tx.Create(&repository.CrawlResult{TaskID: task.ID, Payload: string(payload)}).Error
		}, nil
	}
	rules, err := repository.ListRuleDefinitions()
	if err != nil {
		return nil, fmt.Errorf("load rule definitions: %w", err)
	}
	for _, rule := range rules {
		if rule.ID != task.Target {
			continue
		}
		content, err := os.ReadFile(rule.Script)
		if err != nil {
			return nil, fmt.Errorf("load rule %s: %w", task.Target, err)
		}
		result, err := executeLuaScriptContext(ctx, string(content), task.URL, time.Duration(config.Get().Engine.LuaTimeout)*time.Second)
		if err != nil {
			return nil, err
		}
		payload, err := json.Marshal(result)
		if err != nil {
			return nil, err
		}
		return func(tx *gorm.DB) error {
			return tx.Create(&repository.CrawlResult{TaskID: task.ID, Payload: string(payload)}).Error
		}, nil
	}
	return nil, fmt.Errorf("no rule configured for target %q", task.Target)
}
