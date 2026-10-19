package engine

import (
	"context"
	"encoding/json"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/config"
	"go-lua-crawler/internal/repository"
	task "go-lua-crawler/internal/task"
	"go-lua-crawler/internal/testdb"
	"os"
	"path/filepath"
	"testing"
)

func TestAcceptedKnownRuleExecutesFrozenCodeAfterDiskReplacement(t *testing.T) {
	configPath, err := filepath.Abs("../../configs/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("LUA_SPIDER_CONFIG", configPath)
	config.InitConfig()
	db := testdb.Open(t)
	if err := agentdata.NewStore(db).Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&repository.CrawlResult{}); err != nil {
		t.Fatal(err)
	}
	old := repository.DB
	repository.DB = db
	t.Cleanup(func() { repository.DB = old })
	dir := t.TempDir()
	script := filepath.Join(dir, "rule.lua")
	os.WriteFile(script, []byte(`return true,{items={{title="accepted version",url=TARGET_URL}}}`), 0600)
	rules := filepath.Join(dir, "rules.yaml")
	os.WriteFile(rules, []byte("rules:\n - id: frozen\n   name: Frozen\n   script: "+filepath.ToSlash(script)+"\n   source: Public\n   schedule: 1h\n   item_key: url\n"), 0600)
	t.Setenv("LUA_SPIDER_RULES_FILE", rules)
	child := task.CrawlTask{RequestKey: "frozen", Target: "frozen", URL: "https://example.com"}
	if err := repository.CreateQueuedCrawlTaskWithOutbox(context.Background(), &child); err != nil {
		t.Fatal(err)
	}
	if child.RuleHash == "" {
		t.Fatal("no code snapshot at acceptance")
	}
	os.WriteFile(script, []byte(`error("replacement must not run")`), 0600)
	persist, err := ExecuteTask(context.Background(), child)
	if err != nil {
		t.Fatal(err)
	}
	if err = persist(db); err != nil {
		t.Fatal(err)
	}
	var result repository.CrawlResult
	db.First(&result, child.ID)
	var payload map[string]any
	if err = json.Unmarshal([]byte(result.Payload), &payload); err != nil {
		t.Fatal(err)
	}
	if len(result.Payload) == 0 {
		t.Fatal("missing old result")
	}
	// A broken/missing immutable snapshot is a failure, never a disk fallback.
	child.RuleHash = "absent"
	if _, err = ExecuteTask(context.Background(), child); err == nil {
		t.Fatal("missing snapshot fell back to replaced file")
	}
}
