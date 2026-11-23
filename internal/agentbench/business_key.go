package agentbench

import "go-lua-crawler/internal/recorddiff"

func BusinessKey(row map[string]any, fields []string) (string, error) {
	return recorddiff.BusinessKey(row, fields)
}
