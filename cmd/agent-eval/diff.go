package main

import (
	"encoding/json"
	"fmt"
	"go-lua-crawler/internal/agentbench"
	"go-lua-crawler/internal/aiapp"
	"os"
	"strings"
)

func exportBusinessDiff(before, after, keyFields, goldPath, output string) error {
	if before == "" || after == "" || keyFields == "" || output == "" {
		return fmt.Errorf("diff requires before/after/keys/output")
	}
	read := func(path string) (aiapp.TaskResult, error) {
		raw, err := os.ReadFile(path)
		if err != nil {
			return aiapp.TaskResult{}, err
		}
		var value struct {
			Records []aiapp.MergedRecord `json:"records"`
			Result  *aiapp.TaskResult    `json:"result"`
		}
		if err = json.Unmarshal(raw, &value); err != nil {
			return aiapp.TaskResult{}, err
		}
		if value.Result != nil {
			return *value.Result, nil
		}
		if value.Records == nil {
			return aiapp.TaskResult{}, fmt.Errorf("task result records required: %s", path)
		}
		return aiapp.TaskResult{Records: value.Records}, nil
	}
	a, err := read(before)
	if err != nil {
		return err
	}
	b, err := read(after)
	if err != nil {
		return err
	}
	keys := strings.Split(keyFields, ",")
	for n := range keys {
		keys[n] = strings.TrimSpace(keys[n])
	}
	changes, err := agentbench.DiffBusinessRecords(a.Records, b.Records, keys)
	if err != nil {
		return err
	}
	var score *agentbench.ChangeScore
	if goldPath != "" {
		var gold []agentbench.BusinessChange
		if err = readJSON(goldPath, &gold); err != nil {
			return err
		}
		s := agentbench.ScoreBusinessChanges(changes, gold)
		score = &s
	}
	raw, err := json.MarshalIndent(map[string]any{"key_fields": keys, "changes": changes, "score": score, "gold_supplied": goldPath != ""}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(output, append(raw, '\n'), 0600)
}
