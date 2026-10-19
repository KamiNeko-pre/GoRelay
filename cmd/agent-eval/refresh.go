package main

import (
	"bufio"
	"context"
	"encoding/json"
	"go-lua-crawler/internal/agentbench"
	"os"
)

func refreshTrials(input, endpoint, output string) error {
	f, err := os.Open(input)
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
	rows, err = agentbench.RefreshAccounting(context.Background(), agentbench.ClientConfig{Endpoint: endpoint, Token: os.Getenv("GORELAY_ADMIN_TOKEN")}, rows)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	encoder := json.NewEncoder(file)
	for _, row := range rows {
		if err := encoder.Encode(row); err != nil {
			return err
		}
	}
	return file.Sync()
}
