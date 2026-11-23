package engine

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"go-lua-crawler/internal/config"
)

// Temporary live verification for the nowcoder rule. Not part of the suite.
func TestNowcoderRuleAgainstLive(t *testing.T) {
	abs, err := filepath.Abs(filepath.Join("..", "..", "configs", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	os.Setenv("LUA_SPIDER_CONFIG", abs)
	config.InitConfig()

	script, err := os.ReadFile(filepath.Join("..", "..", "scripts", "jobs", "nowcoder.lua"))
	if err != nil {
		t.Fatal(err)
	}

	result, err := executeLuaScript(string(script), "https://www.nowcoder.com/jobs/intern/center", 90*time.Second)
	if err != nil {
		t.Fatalf("execute nowcoder rule: %v", err)
	}

	meta, _ := result.Collection["meta"].(map[string]any)
	items, _ := result.Collection["items"].([]any)
	t.Logf("meta = %v", meta)
	t.Logf("items = %d", len(items))
	if len(items) > 0 {
		t.Logf("first item = %v", items[0])
	}
}
