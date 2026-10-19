package aiapp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/engine"
)

type RunnerRequest struct {
	Program        engine.ProgramRequest `json:"program"`
	Limits         engine.ProgramLimits  `json:"limits"`
	AllowedDomains []string              `json:"allowed_domains,omitempty"`
}
type RunnerError struct {
	Code       string                `json:"code"`
	Message    string                `json:"message"`
	Limits     *engine.ProgramLimits `json:"limits,omitempty"`
	ObservedIO *acquisition.Stats    `json:"observed_io,omitempty"`
}

func (e *RunnerError) Error() string { return e.Code + ": " + e.Message }

type RunnerResponse struct {
	Result *engine.ProgramResult `json:"result,omitempty"`
	Error  *RunnerError          `json:"error,omitempty"`
}
type RunnerClient struct {
	endpoint string
	token    string
	client   *http.Client
}

func NewRunnerClient(endpoint, token string) (*RunnerClient, error) {
	endpoint = strings.TrimRight(endpoint, "/")
	if err := configuredURL(endpoint); err != nil {
		return nil, err
	}
	if token == "" {
		return nil, fmt.Errorf("runner token is not configured")
	}
	return &RunnerClient{endpoint, token, providerClient(30 * time.Second)}, nil
}
func (c *RunnerClient) Execute(ctx context.Context, in RunnerRequest) (engine.ProgramResult, error) {
	var out RunnerResponse
	// The runner may return a useful classified error with a non-2xx status.
	payload, err := json.Marshal(in)
	if err != nil {
		return engine.ProgramResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/execute", strings.NewReader(string(payload)))
	if err != nil {
		return engine.ProgramResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	client := *c.client
	if in.Limits.Timeout > 0 {
		client.Timeout = in.Limits.Timeout + 5*time.Second
	}
	resp, err := client.Do(req)
	if err != nil {
		return engine.ProgramResult{}, err
	}
	defer resp.Body.Close()
	decoder := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, RunnerResponseByteLimit(in.Limits)))
	if err = decoder.Decode(&out); err != nil {
		return engine.ProgramResult{}, fmt.Errorf("invalid runner response: %w", err)
	}
	// A complete trusted runner result contains its network ledger even when
	// Lua failed before or after IO. Transport loss alone cannot prove zero IO.
	if out.Result != nil {
		out.Result.HTTPUsageKnown = true
	}
	if out.Error != nil {
		out.Error.Limits = &in.Limits
		if out.Result != nil {
			out.Error.ObservedIO = &out.Result.Stats
			return *out.Result, out.Error
		}
		return engine.ProgramResult{}, out.Error
	}
	if resp.StatusCode != 200 || out.Result == nil {
		return engine.ProgramResult{}, fmt.Errorf("runner HTTP status %d without a result", resp.StatusCode)
	}
	return *out.Result, nil
}

func RunPublicProgram(ctx context.Context, in RunnerRequest) (RunnerResponse, error) {
	limits := in.Limits
	if limits.Timeout <= 0 {
		limits.Timeout = 20 * time.Second
	}
	fetcher, err := acquisition.NewPublicFetcher(acquisition.Policy{AllowedDomains: in.AllowedDomains, MaxRequests: limits.MaxRequests, MaxBytes: limits.MaxBytes, Timeout: limits.Timeout})
	if err != nil {
		return RunnerResponse{}, err
	}
	defer fetcher.Close()
	result, err := engine.RunProgram(ctx, in.Program, limits, fetcher.Fetch)
	// Production stats include actual HTTP attempts/redirect hops, even on failure.
	result.Stats = fetcher.Stats()
	result.HTTPUsageKnown = true
	if err != nil {
		return RunnerResponse{Result: &result}, err
	}
	return RunnerResponse{Result: &result}, nil
}
