package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"go-lua-crawler/internal/agentbench"
	"os"
	"sort"
)

func exportCosts(trialsPath, pricingPath, output string) error {
	var prices agentbench.PriceBook
	if err := readJSON(pricingPath, &prices); err != nil {
		return err
	}
	if err := prices.Validate(); err != nil {
		return err
	}
	f, err := os.Open(trialsPath)
	if err != nil {
		return err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64<<10), 32<<20)
	latest := map[string]agentbench.Trial{}
	for scanner.Scan() {
		var row agentbench.Trial
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			return err
		}
		key := fmt.Sprintf("%s/%s/%s/%s/%d", row.Mode, row.Variant, row.EnvironmentID, row.CaseID, row.Repetition)
		latest[key] = row
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	groups := map[string][]agentbench.Trial{}
	for _, row := range latest {
		key := row.Mode + "/" + row.Variant + "/" + row.EnvironmentID
		groups[key] = append(groups[key], row)
	}
	keys := []string{}
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := []map[string]any{}
	for _, key := range keys {
		result = append(result, map[string]any{"group": key, "trials": len(groups[key]), "estimate": agentbench.EstimateCosts(groups[key], prices)})
	}
	raw, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(output, append(raw, '\n'), 0600)
}
