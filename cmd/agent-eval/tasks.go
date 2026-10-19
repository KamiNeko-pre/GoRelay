package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"go-lua-crawler/internal/agentbench"
	"os"
	"path/filepath"
	"strings"
)

func readJSON(path string, out any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}
func writeSummary(path string, rows []agentbench.Trial) error {
	data, err := json.MarshalIndent(agentbench.Summarize(rows), "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0600)
}
func runTasks(endpoint, manifestPath, goldPath, batch, variant, output string, repetitions int) error {
	var manifest agentbench.Manifest
	if err := readJSON(manifestPath, &manifest); err != nil {
		return err
	}
	gold := map[string]agentbench.GoldCase{}
	if goldPath != "" {
		if err := readJSON(goldPath, &gold); err != nil {
			return err
		}
	}
	if output == "" {
		return fmt.Errorf("tasks mode requires an output directory")
	}
	if err := os.MkdirAll(output, 0700); err != nil {
		return err
	}
	metadata, err := json.MarshalIndent(map[string]any{"manifest": manifest, "batch": batch, "variant": variant, "repetitions": repetitions, "endpoint": endpoint}, "", "  ")
	if err != nil {
		return err
	}
	metadataPath := filepath.Join(output, "experiment.json")
	if prior, err := os.ReadFile(metadataPath); err == nil {
		if string(prior) != string(metadata) {
			return fmt.Errorf("experiment metadata differs; use a new output directory")
		}
	} else if !os.IsNotExist(err) {
		return err
	} else if err = os.WriteFile(metadataPath, metadata, 0600); err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(output, "trials.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	encoder := json.NewEncoder(file)
	rows, err := agentbench.Execute(context.Background(), agentbench.ClientConfig{Endpoint: endpoint, Token: os.Getenv("GORELAY_ADMIN_TOKEN"), Batch: batch, Variant: variant, Repetitions: repetitions}, manifest, gold, func(row agentbench.Trial) error {
		if err := encoder.Encode(row); err != nil {
			return err
		}
		return file.Sync()
	})
	if err != nil {
		if len(rows) > 0 {
			if summaryErr := summarizeTrials(filepath.Join(output, "trials.jsonl"), filepath.Join(output, "summary.json")); summaryErr != nil {
				return fmt.Errorf("%w; failed to summarize persisted attempt: %v", err, summaryErr)
			}
		}
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := summarizeTrials(filepath.Join(output, "trials.jsonl"), filepath.Join(output, "summary.json")); err != nil {
		return err
	}
	if len(rows) > 0 && strings.HasPrefix(rows[len(rows)-1].Status, "blocked_") {
		return fmt.Errorf("experiment stopped at %s; written as unscored, not Agent success", rows[len(rows)-1].Status)
	}
	return nil
}
func summarizeTrials(path, output string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64<<10), 32<<20)
	rows := []agentbench.Trial{}
	for scanner.Scan() {
		var row agentbench.Trial
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			return err
		}
		rows = append(rows, row)
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return writeSummary(output, rows)
}
