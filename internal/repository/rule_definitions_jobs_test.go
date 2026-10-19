package repository

import (
	"os"
	"path/filepath"
	"testing"
)

func TestJobBoardRulesAreRegisteredWithExistingScripts(t *testing.T) {
	rulesPath, err := filepath.Abs(filepath.Join("..", "..", "configs", "rules.yaml"))
	if err != nil {
		t.Fatalf("resolve rules path: %v", err)
	}
	t.Setenv("LUA_SPIDER_RULES_FILE", rulesPath)

	rules, err := ListRuleDefinitions()
	if err != nil {
		t.Fatalf("load rule definitions: %v", err)
	}

	wantScripts := map[string]string{
		"nowcoder_jobs":   "scripts/jobs/nowcoder.lua",
		"lever_jobs":      "scripts/jobs/lever.lua",
		"greenhouse_jobs": "scripts/jobs/greenhouse.lua",
		"ashby_jobs":      "scripts/jobs/ashby.lua",
	}
	found := make(map[string]string)
	foundURLs := make(map[string]string)
	for _, rule := range rules {
		if _, wanted := wantScripts[rule.ID]; wanted {
			found[rule.ID] = filepath.ToSlash(rule.Script)
			foundURLs[rule.ID] = rule.URL
		}
	}

	for id, script := range wantScripts {
		if found[id] != script {
			t.Errorf("rule %q script = %q, want %q", id, found[id], script)
		}
		scriptPath := filepath.Join(filepath.Dir(rulesPath), "..", script)
		if _, err := os.Stat(scriptPath); err != nil {
			t.Errorf("rule %q script %q: %v", id, scriptPath, err)
		}
	}
	if got, want := foundURLs["greenhouse_jobs"], "https://job-boards.greenhouse.io/embed/job_board?for=airtable"; got != want {
		t.Errorf("Greenhouse default URL = %q, want %q", got, want)
	}
	if got, want := foundURLs["lever_jobs"], "https://api.lever.co/v0/postings/palantir?mode=json&limit=20"; got != want {
		t.Errorf("Lever default URL = %q, want %q", got, want)
	}
}
