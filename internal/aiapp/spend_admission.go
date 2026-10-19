package aiapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go-lua-crawler/internal/agentdata"
	"gorm.io/gorm"
	"os"
)

func (s *Service) hasSavedOperation(ctx context.Context, runID, key string) (bool, error) {
	var op agentdata.Operation
	err := s.Data.DB.WithContext(ctx).Where("run_id = ? AND `key` = ?", runID, key).First(&op).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	return err == nil, err
}

func spendPolicyFromEnv() (*agentdata.SpendBudget, error) {
	path := os.Getenv("GORELAY_SPEND_POLICY_PATH")
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var policy agentdata.SpendPolicy
	if err = json.Unmarshal(raw, &policy); err != nil {
		return nil, err
	}
	if err = policy.Validate(); err != nil {
		return nil, err
	}
	return &agentdata.SpendBudget{Policy: policy}, nil
}

func (p *ModelProvider) QuoteChat(in ChatRequest, overhead int64) (agentdata.SpendQuote, error) {
	in.Model = p.config.Model
	raw, err := json.Marshal(in)
	if err != nil {
		return agentdata.SpendQuote{}, err
	}
	if in.MaxCompletionTokens <= 0 {
		return agentdata.SpendQuote{}, fmt.Errorf("%w: output token bound is required", agentdata.ErrSpendAdmission)
	}
	return agentdata.SpendQuote{Stage: "decision", Model: p.config.Model, InputTokens: int64(len(raw)) + overhead, OutputTokens: int64(in.MaxCompletionTokens), Basis: "UTF-8 request bytes plus host-declared tokenizer/framing overhead; output uses API max_completion_tokens"}, nil
}
func (p *ModelProvider) QuoteEmbedding(texts []string, overhead int64) (agentdata.SpendQuote, error) {
	raw, err := json.Marshal(map[string]any{"model": p.config.Model, "input": texts, "encoding_format": "float"})
	if err != nil {
		return agentdata.SpendQuote{}, err
	}
	return agentdata.SpendQuote{Stage: "embedding", Model: p.config.Model, InputTokens: int64(len(raw)) + overhead, Basis: "UTF-8 request bytes plus host-declared tokenizer/framing overhead"}, nil
}
func (a EmbeddingAdapter) QuoteEmbedding(texts []string, overhead int64) (agentdata.SpendQuote, error) {
	if p, ok := a.Provider.(interface {
		QuoteEmbedding([]string, int64) (agentdata.SpendQuote, error)
	}); ok {
		return p.QuoteEmbedding(texts, overhead)
	}
	return agentdata.SpendQuote{}, fmt.Errorf("%w: embedding transport cannot quote its input", agentdata.ErrSpendAdmission)
}

func (p *BraveSearch) QuoteSearch() (agentdata.SpendQuote, error) {
	return agentdata.SpendQuote{Stage: "search", Basis: "host-configured price per search API request"}, nil
}

func quoteModel(provider ChatProvider, request ChatRequest, budget *agentdata.SpendBudget) (*agentdata.SpendQuote, error) {
	if budget == nil {
		return nil, nil
	}
	p, ok := provider.(interface {
		QuoteChat(ChatRequest, int64) (agentdata.SpendQuote, error)
	})
	if !ok {
		return nil, fmt.Errorf("%w: model transport has unbounded internal usage (CLI is unsupported by strict admission)", agentdata.ErrSpendAdmission)
	}
	quote, err := p.QuoteChat(request, budget.Policy.InputOverhead)
	return &quote, err
}

func (s *Service) quoteTool(name, args string, budget *agentdata.SpendBudget) (*agentdata.SpendQuote, error) {
	if budget == nil {
		return nil, nil
	}
	switch name {
	case "discover_sources":
		provider, ok := s.Search.(interface {
			QuoteSearch() (agentdata.SpendQuote, error)
		})
		if !ok {
			return nil, fmt.Errorf("%w: search transport has unbounded internal usage", agentdata.ErrSpendAdmission)
		}
		quote, err := provider.QuoteSearch()
		return &quote, err
	case "search_knowledge":
		var a struct {
			Query string `json:"query"`
		}
		if err := decodeArgs(args, &a); err != nil {
			return nil, err
		}
		if s.Retriever == nil {
			return nil, fmt.Errorf("retrieval is not configured")
		}
		p, ok := s.Retriever.Embed.(interface {
			QuoteEmbedding([]string, int64) (agentdata.SpendQuote, error)
		})
		if !ok {
			return nil, fmt.Errorf("%w: retrieval embedding cannot quote its input", agentdata.ErrSpendAdmission)
		}
		quote, err := p.QuoteEmbedding([]string{a.Query}, budget.Policy.InputOverhead)
		quote.Stage = "retrieval_embedding"
		return &quote, err
	}
	return nil, nil
}
