package aiapp

import (
	"encoding/json"
	"fmt"
	"go-lua-crawler/internal/agentdata"
)

// Called under the Run row lock, in the same transaction as finalizing the
// reserved operation. Unknown outcomes and pre-upgrade reservations stay held.
func settleHTTPBudget(raw string, op agentdata.Operation, observed *HTTPObservation) (string, error) {
	if observed == nil || !observed.Known || op.ResourcesJSON == "" {
		return raw, nil
	}
	var reserved CallCost
	if err := json.Unmarshal([]byte(op.ResourcesJSON), &reserved); err != nil {
		return "", err
	}
	if reserved.HTTPRequests == 0 && reserved.HTTPBytes == 0 {
		return raw, nil
	}
	if observed.Requests < 0 || observed.Bytes < 0 {
		return "", fmt.Errorf("negative observed HTTP usage")
	}
	var budget BudgetSnapshot
	if err := json.Unmarshal([]byte(raw), &budget); err != nil {
		return "", err
	}
	if budget.Spent.HTTPRequests < reserved.HTTPRequests || budget.Spent.HTTPBytes < reserved.HTTPBytes {
		return "", fmt.Errorf("HTTP reservation exceeds held usage")
	}
	// The extra byte used to detect overflow is real usage too. Do not clamp it.
	budget.Spent.HTTPRequests = budget.Spent.HTTPRequests - reserved.HTTPRequests + int64(observed.Requests)
	budget.Spent.HTTPBytes = budget.Spent.HTTPBytes - reserved.HTTPBytes + observed.Bytes
	next, err := json.Marshal(budget)
	return string(next), err
}
