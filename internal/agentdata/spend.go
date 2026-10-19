package agentdata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"math/big"
)

var ErrSpendAdmission = errors.New("request spend admission rejected")

// Prices are supplied by the host, in millionths of the named currency.
// Input bounds depend on the declared tokenizer/framing assumption. They are
// reservation limits, never a claim about the provider's eventual invoice.
type SpendRate struct {
	Input  *int64 `json:"input_per_million_microunits"`
	Output *int64 `json:"output_per_million_microunits"`
}
type SpendPolicy struct {
	MaxTokens     int64                `json:"max_total_tokens"`
	MaxMicrounits int64                `json:"max_microunits"`
	Currency      string               `json:"currency"`
	PriceDate     string               `json:"price_date"`
	InputOverhead int64                `json:"input_overhead_tokens"`
	Models        map[string]SpendRate `json:"models"`
	SearchPerCall *int64               `json:"search_per_call_microunits,omitempty"`
}
type SpendBudget struct {
	Policy             SpendPolicy `json:"policy"`
	ReservedTokens     int64       `json:"reserved_tokens"`
	ReservedMicrounits int64       `json:"reserved_microunits"`
}
type SpendQuote struct {
	Stage        string `json:"stage"`
	Model        string `json:"model,omitempty"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	Basis        string `json:"basis"`
}
type SpendCharge struct {
	Quote      SpendQuote `json:"quote"`
	Microunits int64      `json:"reserved_microunits"`
	Currency   string     `json:"currency"`
	PriceDate  string     `json:"price_date"`
}

func (p SpendPolicy) Validate() error {
	if p.MaxTokens <= 0 || p.MaxMicrounits <= 0 || p.Currency == "" || p.PriceDate == "" || p.InputOverhead < 0 {
		return fmt.Errorf("%w: limits/currency/price date/overhead must be explicit", ErrSpendAdmission)
	}
	for name, rate := range p.Models {
		if name == "" || rate.Input == nil || *rate.Input < 0 || (rate.Output != nil && *rate.Output < 0) {
			return fmt.Errorf("%w: invalid or missing model rate", ErrSpendAdmission)
		}
	}
	if p.SearchPerCall != nil && *p.SearchPerCall < 0 {
		return fmt.Errorf("%w: negative search rate", ErrSpendAdmission)
	}
	return nil
}

func SpendOf(raw string) (*SpendBudget, error) {
	var envelope struct {
		Spend *SpendBudget `json:"spend"`
	}
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		return nil, err
	}
	return envelope.Spend, nil
}

func (p SpendPolicy) Price(q SpendQuote) (int64, error) {
	if err := p.Validate(); err != nil {
		return 0, err
	}
	if q.InputTokens < 0 || q.OutputTokens < 0 || q.Basis == "" {
		return 0, fmt.Errorf("%w: missing trustworthy request quote", ErrSpendAdmission)
	}
	if q.Stage == "search" {
		if p.SearchPerCall == nil {
			return 0, fmt.Errorf("%w: search rate missing", ErrSpendAdmission)
		}
		return *p.SearchPerCall, nil
	}
	rate, ok := p.Models[q.Model]
	if !ok || rate.Input == nil || (q.OutputTokens > 0 && rate.Output == nil) {
		return 0, fmt.Errorf("%w: model rate missing", ErrSpendAdmission)
	}
	amount := new(big.Int).Mul(big.NewInt(q.InputTokens), big.NewInt(*rate.Input))
	if q.OutputTokens > 0 {
		amount.Add(amount, new(big.Int).Mul(big.NewInt(q.OutputTokens), big.NewInt(*rate.Output)))
	}
	amount.Add(amount, big.NewInt(999999)).Div(amount, big.NewInt(1000000))
	if !amount.IsInt64() {
		return 0, fmt.Errorf("%w: price overflow", ErrSpendAdmission)
	}
	return amount.Int64(), nil
}

// Called under the same Run row lock as the operation reservation. Unknown
// JSON fields are preserved so async index charges cannot erase loop state.
// Under a host-frozen unlimited experiment the counters still accumulate
// (attempts remain accountable) but reservation admission is not rejected.
func reserveSpend(raw string, quote *SpendQuote) (next, charge string, err error) {
	var envelope map[string]json.RawMessage
	if err = json.Unmarshal([]byte(raw), &envelope); err != nil {
		return "", "", err
	}
	var unlimited bool
	if ub, ok := envelope["unlimited"]; ok && string(ub) != "null" {
		_ = json.Unmarshal(ub, &unlimited)
	}
	budget, err := SpendOf(raw)
	if err != nil {
		return "", "", err
	}
	if budget == nil {
		return raw, "", nil
	}
	if quote == nil {
		return "", "", fmt.Errorf("%w: external provider cannot bound this request", ErrSpendAdmission)
	}
	micro, err := budget.Policy.Price(*quote)
	if err != nil {
		return "", "", err
	}
	tokens := new(big.Int).Add(big.NewInt(quote.InputTokens), big.NewInt(quote.OutputTokens))
	if !tokens.IsInt64() || budget.ReservedTokens < 0 || budget.ReservedMicrounits < 0 {
		return "", "", fmt.Errorf("%w: corrupted reservation state", ErrSpendAdmission)
	}
	if !unlimited {
		if budget.ReservedTokens > budget.Policy.MaxTokens || tokens.Int64() > budget.Policy.MaxTokens-budget.ReservedTokens || budget.ReservedMicrounits > budget.Policy.MaxMicrounits || micro > budget.Policy.MaxMicrounits-budget.ReservedMicrounits {
			return "", "", fmt.Errorf("%w: token or monetary reservation limit exhausted", ErrSpendAdmission)
		}
	}
	budget.ReservedTokens += tokens.Int64()
	budget.ReservedMicrounits += micro
	envelope["spend"], err = json.Marshal(budget)
	if err != nil {
		return "", "", err
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return "", "", err
	}
	billed, err := json.Marshal(SpendCharge{Quote: *quote, Microunits: micro, Currency: budget.Policy.Currency, PriceDate: budget.Policy.PriceDate})
	return string(encoded), string(billed), err
}

// Background index work may continue after a successful/partial delivery,
// but cancellation and the original frozen spend limits still apply.
func (s *Store) ReserveIndexSpend(ctx context.Context, id, key string, quote *SpendQuote) (Operation, bool, error) {
	var op Operation
	fresh := false
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var r Run
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&r, "id = ?", id).Error; err != nil {
			return err
		}
		if r.Cancelled {
			return ErrConflict
		}
		err := tx.Where("run_id = ? AND `key` = ?", id, key).First(&op).Error
		if err == nil {
			if op.InputHash != Hash(quote) || op.Kind != "index_embedding" {
				return ErrConflict
			}
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		next, cost, err := reserveSpend(r.BudgetJSON, quote)
		if err != nil {
			return err
		}
		if cost == "" {
			return nil
		}
		if err = tx.Model(&Run{}).Where("id = ?", id).Update("budget_json", next).Error; err != nil {
			return err
		}
		op = Operation{RunID: id, Key: key, InputHash: Hash(quote), Kind: "index_embedding", Stage: "reserved", CostJSON: cost}
		if err = tx.Create(&op).Error; err != nil {
			return err
		}
		fresh = true
		return nil
	})
	return op, fresh, err
}

func (s *Store) CompleteIndexSpend(ctx context.Context, id uint64, stage string) error {
	if stage != "succeeded" && stage != "failed" {
		return ErrConflict
	}
	q := s.DB.WithContext(ctx).Model(&Operation{}).Where("id = ? AND kind = ? AND stage = ?", id, "index_embedding", "reserved").Update("stage", stage)
	if q.Error != nil {
		return q.Error
	}
	if q.RowsAffected != 1 {
		return ErrConflict
	}
	return nil
}
