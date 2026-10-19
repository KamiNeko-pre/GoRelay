package agentbench

import (
	"encoding/json"
	"fmt"
	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/knowledge"
	"math"
)

type ModelPrice struct {
	InputPerMillion  *float64 `json:"input_per_million"`
	OutputPerMillion *float64 `json:"output_per_million"`
}
type PriceBook struct {
	Currency      string                `json:"currency"`
	Date          string                `json:"date"`
	Models        map[string]ModelPrice `json:"models"`
	SearchPerCall *float64              `json:"search_per_call"`
}
type CostEstimate struct {
	Currency          string   `json:"currency"`
	PriceDate         string   `json:"price_date"`
	KnownSubtotal     float64  `json:"known_subtotal"`
	UnknownComponents int      `json:"unknown_components"`
	PendingIndexRuns  int      `json:"pending_index_runs"`
	Total             *float64 `json:"estimated_total"`
	PerCorrectTask    *float64 `json:"estimated_cost_per_correct_task"`
	Basis             string   `json:"basis"`
}

func (book PriceBook) Validate() error {
	if book.Currency == "" || book.Date == "" {
		return fmt.Errorf("price currency/date required")
	}
	rates := []*float64{book.SearchPerCall}
	for _, model := range book.Models {
		rates = append(rates, model.InputPerMillion, model.OutputPerMillion)
	}
	for _, rate := range rates {
		if rate != nil && (*rate < 0 || math.IsNaN(*rate) || math.IsInf(*rate, 0)) {
			return fmt.Errorf("invalid price rate")
		}
	}
	return nil
}
func EstimateCosts(rows []Trial, book PriceBook) CostEstimate {
	result := CostEstimate{Currency: book.Currency, PriceDate: book.Date, Basis: "reported token usage at configured rates; excludes provider cache discounts/tax, not a billing statement"}
	correct, scored := 0, 0
	add := func(model string, input, output int64, known bool) {
		price, ok := book.Models[model]
		if !known || !ok || price.InputPerMillion == nil || (output > 0 && price.OutputPerMillion == nil) {
			result.UnknownComponents++
			return
		}
		result.KnownSubtotal += float64(input) * *price.InputPerMillion / 1e6
		if output > 0 {
			result.KnownSubtotal += float64(output) * *price.OutputPerMillion / 1e6
		}
	}
	for _, row := range rows {
		if row.InformationAccounting != nil {
			row.Accounting = row.InformationAccounting.RunAccounting
		}
		if row.Correct != nil {
			scored++
			if *row.Correct && row.Status == "completed" {
				correct++
			}
		}
		for _, call := range modelCalls(row) {
			if call.Usage == nil {
				add(call.Model, 0, 0, false)
			} else {
				add(call.Model, call.Usage.PromptTokens, call.Usage.CompletionTokens, true)
			}
		}
		for i := 0; i < searchAttempts(row); i++ {
			if book.SearchPerCall == nil {
				result.UnknownComponents++
			} else {
				result.KnownSubtotal += *book.SearchPerCall
			}
		}
		for _, op := range row.Operations {
			switch op.Kind {
			case "search_knowledge":
				var out aiapp.ToolResult
				var set knowledge.EvidenceSet
				_ = json.Unmarshal([]byte(op.ResultJSON), &out)
				_ = json.Unmarshal(out.Data, &set)
				if set.Metrics.EmbeddingCalls > 0 && set.Metrics.Usage != nil {
					add(set.Metrics.Model, set.Metrics.Usage.PromptTokens, 0, true)
				} else {
					result.UnknownComponents++
				}
			}
		}
		if row.InformationAccounting != nil {
			for _, retrieval := range row.InformationAccounting.Retrievals {
				if retrieval.Metrics == nil {
					result.UnknownComponents++
					continue
				}
				m := retrieval.Metrics
				if m.EmbeddingCalls > 0 {
					if m.Usage == nil {
						add(m.Model, 0, 0, false)
					} else {
						add(m.Model, m.Usage.PromptTokens, 0, true)
					}
				}
			}
		}
		if row.Accounting == nil {
			result.UnknownComponents++
			continue
		}
		if !row.Accounting.IndexSettled {
			result.PendingIndexRuns++
		}
		for _, attempt := range row.Accounting.IndexAttempts {
			var metrics knowledge.IndexMetrics
			if json.Unmarshal([]byte(attempt.MetricsJSON), &metrics) != nil {
				result.UnknownComponents++
				continue
			}
			if metrics.EmbeddingCalls > 0 {
				if metrics.Usage == nil {
					add(metrics.Model, 0, 0, false)
				} else {
					add(metrics.Model, metrics.Usage.PromptTokens, 0, true)
				}
			}
		}
	}
	if result.UnknownComponents == 0 && result.PendingIndexRuns == 0 {
		total := result.KnownSubtotal
		result.Total = &total
		if correct > 0 && scored == len(rows) {
			value := total / float64(correct)
			result.PerCorrectTask = &value
		}
	}
	return result
}
