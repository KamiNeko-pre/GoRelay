package repository

import (
	"fmt"
	"os"
	"strings"

	"gorelay/internal/analysis"

	yaml "go.yaml.in/yaml/v3"
)

type ruleDefinitionsFile struct {
	Rules []analysis.RuleDefinition `yaml:"rules"`
}

// ListRuleDefinitions loads display metadata from a small repository config.
// LUA_SPIDER_RULES_FILE is useful for tests and alternate local environments;
// the normal server starts from the repository root.
func ListRuleDefinitions() ([]analysis.RuleDefinition, error) {
	path := strings.TrimSpace(os.Getenv("LUA_SPIDER_RULES_FILE"))
	if path == "" {
		path = "configs/rules.yaml"
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read rule definitions %q: %w", path, err)
	}
	var file ruleDefinitionsFile
	if err := yaml.Unmarshal(content, &file); err != nil {
		return nil, fmt.Errorf("parse rule definitions %q: %w", path, err)
	}
	for index := range file.Rules {
		rule := &file.Rules[index]
		rule.ID = strings.TrimSpace(rule.ID)
		rule.Name = strings.TrimSpace(rule.Name)
		rule.Source = strings.TrimSpace(rule.Source)
		rule.Schedule = strings.TrimSpace(rule.Schedule)
		rule.ItemKey = strings.TrimSpace(rule.ItemKey)
		if rule.ID == "" || rule.Name == "" || rule.Source == "" || rule.Schedule == "" || rule.ItemKey == "" {
			return nil, fmt.Errorf("rule definition at index %d is missing a required field", index)
		}
		if rule.MinItems < 0 {
			return nil, fmt.Errorf("rule %q has a negative min_items", rule.ID)
		}
	}
	if len(file.Rules) == 0 {
		return nil, fmt.Errorf("rule definitions %q contains no rules", path)
	}
	return file.Rules, nil
}
