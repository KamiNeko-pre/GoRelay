package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"time"

	task "gorelay/internal/task"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// FrozenRule stores an immutable hash-addressed snapshot of a local rule script.
type FrozenRule struct {
	Hash      string    `json:"hash" gorm:"primaryKey;size:64"`
	Code      string    `json:"code" gorm:"type:longtext"`
	CreatedAt time.Time `json:"created_at"`
}

func (FrozenRule) TableName() string { return "frozen_rules" }

func GetFrozenRule(ctx context.Context, hash string) (FrozenRule, error) {
	var rule FrozenRule
	err := DB.WithContext(ctx).First(&rule, "hash = ?", hash).Error
	return rule, err
}

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
		sum := sha256.Sum256(code)
		hash := hex.EncodeToString(sum[:])
		frozen := FrozenRule{Hash: hash, Code: string(code)}
		if err := tx.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&frozen).Error; err != nil {
			return err
		}
		child.RuleHash = hash
		return nil
	}
	return nil
}
