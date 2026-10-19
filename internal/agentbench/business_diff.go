package agentbench

import (
	"encoding/json"
	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/recorddiff"
)

type BusinessChange = recorddiff.BusinessChange
type ChangeScore struct {
	TP int `json:"tp"`
	FP int `json:"fp"`
	FN int `json:"fn"`
}

func DiffBusinessRecords(before, after []aiapp.MergedRecord, keys []string) ([]BusinessChange, error) {
	a := make([]map[string]any, 0, len(before))
	b := make([]map[string]any, 0, len(after))
	for _, r := range before {
		a = append(a, r.Fields)
	}
	for _, r := range after {
		b = append(b, r.Fields)
	}
	return recorddiff.Diff(a, b, keys)
}
func ScoreBusinessChanges(actual, gold []BusinessChange) ChangeScore {
	signature := func(c BusinessChange) string { raw, _ := json.Marshal(c); return string(raw) }
	wanted := map[string]int{}
	for _, c := range gold {
		wanted[signature(c)]++
	}
	score := ChangeScore{}
	for _, c := range actual {
		key := signature(c)
		if wanted[key] > 0 {
			score.TP++
			wanted[key]--
		} else {
			score.FP++
		}
	}
	for _, n := range wanted {
		score.FN += n
	}
	return score
}
