package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	taskmodel "go-lua-crawler/internal/task"
)

func TestLuaParentCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := executeLuaScriptContext(ctx, "while true do end", "https://example.com", time.Minute)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
}

func TestExecuteTaskRejectsTargetWithoutRegisteredRule(t *testing.T) {
	rulesPath := filepath.Join(t.TempDir(), "rules.yaml")
	if err := os.WriteFile(rulesPath, []byte(`rules:
  - id: known-rule
    name: Known rule
    script: scripts/hackernews_collection.lua
    source: Test
    schedule: 1h
    min_items: 0
    item_key: url
`), 0o600); err != nil {
		t.Fatalf("write rule definitions: %v", err)
	}
	t.Setenv("LUA_SPIDER_RULES_FILE", rulesPath)

	_, err := ExecuteTask(context.Background(), taskmodel.CrawlTask{
		ID:     1,
		Target: "missing-rule",
		URL:    "https://example.com",
	})
	if err == nil || !strings.Contains(err.Error(), `no rule configured for target "missing-rule"`) {
		t.Fatalf("error = %v, want missing target rule error", err)
	}
}
