package repository

import (
	"context"
	"errors"
	"fmt"
	"go-lua-crawler/internal/agentdata"
	task "go-lua-crawler/internal/task"
	"gorm.io/gorm"
	"os"
)

// Existing unknown targets keep their old acceptance behavior and fail at
// execution. Every known rule accepted with available config binds its code.
func snapshotKnownRule(ctx context.Context, tx *gorm.DB, child *task.CrawlTask) error {
	if child.ProgramHash != "" || child.RuleHash != "" {
		return nil
	}
	rules, err := ListRuleDefinitions()
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, rule := range rules {
		if rule.ID != child.Target {
			continue
		}
		code, err := os.ReadFile(rule.Script)
		if err != nil {
			return err
		}
		if len(code) == 0 || len(code) > 64<<10 {
			return fmt.Errorf("known rule code must contain 1..65536 bytes")
		}
		a, err := agentdata.NewStore(tx).SaveArtifact(ctx, agentdata.ProgramArtifact{Code: string(code), Kind: "trusted_legacy", PolicyVersion: "trusted-legacy-v1"})
		if err != nil {
			return err
		}
		child.RuleHash = a.Hash
		return nil
	}
	return nil
}
