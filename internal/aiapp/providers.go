// Package aiapp contains the application contracts and adapters of the task Agent.
// Adapters perform one attempt; the durable Run owns retry decisions and budgets.
package aiapp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go-lua-crawler/internal/programrunner"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type ProviderConfig struct {
	BaseURL string
	APIKey  string
	Model   string
	Timeout time.Duration
	Stream  bool
}

// Message is one conversation turn. CacheControl marks the last message whose
// prefix the provider may cache: everything up to and including it is stable
// across rounds, so the provider can reuse its attention state instead of
// re-reading it. Providers that ignore the field are unaffected; providers that
// honour it turn a repeated 17k-token prefix into a cache hit, which is the
// difference between cached_prompt_tokens=0 and a large majority cached.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content,omitempty"`
	// Opaque provider continuation for thinking-mode tool calls. Preserve it
	// through response decoding, durable history and the next provider request.
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
	CacheControl     any        `json:"cache_control,omitempty"`
}
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}
type ToolDefinition struct {
	Type     string             `json:"type"`
	Function FunctionDefinition `json:"function"`
}
type FunctionDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

type ThinkingConfig struct {
	Type string `json:"type"`
}

type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type ChatRequest struct {
	Model               string           `json:"model,omitempty"`
	Messages            []Message        `json:"messages"`
	Tools               []ToolDefinition `json:"tools,omitempty"`
	MaxCompletionTokens int              `json:"max_completion_tokens,omitempty"`
	// ReasoningEffort is forwarded verbatim when configured. Providers that
	// support a thinking budget accept it and use it to spend more search on the
	// same prompt; providers that do not support it ignore the field. It is never
	// defaulted here, so an unset value leaves the provider's own behaviour.
	ReasoningEffort string          `json:"reasoning_effort,omitempty"`
	Thinking        *ThinkingConfig `json:"thinking,omitempty"`
	Stream          bool            `json:"stream,omitempty"`
	StreamOptions   *StreamOptions  `json:"stream_options,omitempty"`
}

// Usage is the provider's token accounting. Gateways expose the cached-prefix
// count under different names: the legacy flat "cached_prompt_tokens" and the
// current OpenAI nesting "prompt_tokens_details.cached_tokens". Only the nested
// form was observed from this gateway, so reading just the flat field reported
// cached=0 on every round and made a working cache look absent.
//
// Cached is the value to use: it resolves whichever form the response carried.
type Usage struct {
	PromptTokens           int64              `json:"prompt_tokens"`
	CachedPromptTokens     int64              `json:"cached_prompt_tokens,omitempty"`
	PromptTokensDetail     *PromptDetails     `json:"prompt_tokens_details,omitempty"`
	CompletionTokens       int64              `json:"completion_tokens,omitempty"`
	CompletionTokensDetail *CompletionDetails `json:"completion_tokens_details,omitempty"`
	TotalTokens            int64              `json:"total_tokens"`
}

// Reasoning tokens are a provider-reported subset of completion tokens. They
// are retained for diagnostics and must never be added to TotalTokens again.
type CompletionDetails struct {
	ReasoningTokens *int64 `json:"reasoning_tokens,omitempty"`
}

// PromptDetails is the current OpenAI shape for prompt-token breakdown.
type PromptDetails struct {
	CachedTokens int64 `json:"cached_tokens"`
}

// Cached reports the cached prefix tokens, preferring the nested form that the
// gateway actually sends and falling back to the legacy flat field.
func (u Usage) Cached() int64 {
	if u.PromptTokensDetail != nil && u.PromptTokensDetail.CachedTokens > 0 {
		return u.PromptTokensDetail.CachedTokens
	}
	return u.CachedPromptTokens
}

type PromptContextProfile struct {
	PromptHash      string `json:"prompt_hash,omitempty"`
	ToolSchemaHash  string `json:"tool_schema_hash,omitempty"`
	RequestBytes    int    `json:"request_bytes"`
	ToolSchemaBytes int    `json:"tool_schema_bytes"`
	Compressed      bool   `json:"compressed"`
}

type ChatResponse struct {
	DeliveryRecoveryPhase string `json:"delivery_recovery_phase,omitempty"` // Host records the phase actually sent to this request.
	// Host measurements of the actual request; never populated by model output.
	ContextProfile *PromptContextProfile `json:"context_profile,omitempty"`
	// Host-owned snapshot marker, never taken from provider output. It records
	// which terminal children were visible BEFORE this model request.
	ObservedChildren string       `json:"observed_children,omitempty"`
	Failure          *FailureInfo `json:"failure,omitempty"`
	Message          Message      `json:"message"`
	FinishReason   string `json:"finish_reason"`
	Model          string `json:"model"`
	Usage          *Usage `json:"usage"` // nil means not reported, never zero-cost evidence.
	RequestID      string `json:"request_id,omitempty"`
	DurationMS     int64  `json:"duration_ms"`
	TTFTMS         int64  `json:"ttft_ms,omitempty"`
	DecodeMS       int64  `json:"decode_ms,omitempty"`
	ReasoningChars int    `json:"reasoning_chars,omitempty"`
	ContentChars   int    `json:"content_chars,omitempty"`
	ToolArgsChars  int    `json:"tool_args_chars,omitempty"`
}
type EmbeddingResponse struct {
	Vectors    [][]float32 `json:"vectors"`
	Model      string      `json:"model"`
	Usage      *Usage      `json:"usage"`
	RequestID  string      `json:"request_id,omitempty"`
	DurationMS int64       `json:"duration_ms"`
}
type ChatProvider interface {
	Chat(context.Context, ChatRequest) (ChatResponse, error)
}
type EmbeddingProvider interface {
	Embed(context.Context, []string) (EmbeddingResponse, error)
}

type ModelProvider struct {
	routeMu        sync.Mutex
	preferredModel string
	config         ProviderConfig
	client         *http.Client
}

func configuredURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u == nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("provider endpoint must be an HTTP(S) URL without credentials/query/fragment")
	}
	return nil
}

func NewModelProvider(c ProviderConfig) (*ModelProvider, error) {
	c.BaseURL = strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	if err := configuredURL(c.BaseURL); err != nil {
		return nil, err
	}
	if strings.TrimSpace(c.APIKey) == "" || strings.TrimSpace(c.Model) == "" {
		return nil, fmt.Errorf("provider API key and model are not configured")
	}
	return &ModelProvider{config: c, client: providerClient(c.Timeout)}, nil
}

func providerClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return fmt.Errorf("provider redirects are disabled") }}
}

type ProviderError struct {
	Code           string `json:"code,omitempty"`
	Type           string `json:"type,omitempty"`
	Param          string `json:"param,omitempty"`
	Diagnostic     string `json:"diagnostic,omitempty"`
	Status         int
	Retryable      bool
	RetryAfter     string
	QuotaExhausted bool
}

// Decode failure means the HTTP response was received; it is not evidence of
// an unknown network outcome. Keep only decoder metadata, never the raw body.
type providerDecodeError struct{ cause error }

func (e *providerDecodeError) Error() string {
	return fmt.Sprintf("invalid provider JSON: %v", e.cause)
}
func (e *providerDecodeError) Unwrap() error { return e.cause }

func (e *ProviderError) Unwrap() error {
	if e.QuotaExhausted {
		return ErrProviderQuota
	}
	return nil
}

func (e *ProviderError) Error() string {
	message := fmt.Sprintf("provider HTTP status %d (retryable=%t)", e.Status, e.Retryable)
	if e.Code != "" {
		message += " code=" + e.Code
	}
	if e.Param != "" {
		message += " param=" + e.Param
	}
	if e.Diagnostic != "" {
		message += ": " + e.Diagnostic
	}
	return message
}

// Preserve structured protocol diagnostics; never retain a raw error body.
// Authentication headers are redacted before any diagnostic crosses this boundary.
func providerJSON(ctx context.Context, client *http.Client, method, endpoint string, headers map[string]string, input, output any) (string, error) {
	var body io.Reader
	if input != nil {
		payload, err := json.Marshal(input)
		if err != nil {
			return "", err
		}
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	id := resp.Header.Get("X-Request-ID")
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return id, decodeProviderError(resp, headers)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return id, err
	}
	if err = json.Unmarshal(data, output); err != nil {
		return id, &providerDecodeError{cause: err}
	}
	return id, nil
}

type providerWireResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Usage *Usage `json:"usage"`
}

func decodeProviderError(resp *http.Response, headers map[string]string) error {
	remote := &ProviderError{Status: resp.StatusCode, QuotaExhausted: resp.StatusCode == http.StatusPaymentRequired, Retryable: resp.StatusCode == 408 || resp.StatusCode == 409 || resp.StatusCode == 429 || resp.StatusCode >= 500, RetryAfter: resp.Header.Get("Retry-After")}
	var failure struct {
		Error struct {
			Code    string `json:"code"`
			Type    string `json:"type"`
			Param   string `json:"param"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.NewDecoder(resp.Body).Decode(&failure) == nil {
		remote.Code = providerDiagnostic(failure.Error.Code, headers)
		remote.Type = providerDiagnostic(failure.Error.Type, headers)
		remote.Param = providerDiagnostic(failure.Error.Param, headers)
		remote.QuotaExhausted = remote.QuotaExhausted || quotaErrorCode(remote.Code) || remote.Type == "insufficient_quota"
		remote.Diagnostic = providerDiagnostic(failure.Error.Message, headers)
		if remote.QuotaExhausted {
			remote.Retryable = false
		}
	}
	return remote
}

func providerChatStream(ctx context.Context, client *http.Client, endpoint string, headers map[string]string, input any, start time.Time) (id string, wire providerWireResponse, ttftMS int64, err error) {
	payload, err := json.Marshal(input)
	if err != nil {
		return "", wire, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", wire, 0, err
	}
	req.Header.Set("Accept", "text/event-stream, application/json")
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", wire, 0, err
	}
	defer resp.Body.Close()
	id = resp.Header.Get("X-Request-ID")
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return id, wire, 0, decodeProviderError(resp, headers)
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if !strings.Contains(ct, "text/event-stream") {
		data, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return id, wire, 0, readErr
		}
		if err = json.Unmarshal(data, &wire); err != nil {
			return id, wire, 0, &providerDecodeError{cause: err}
		}
		return id, wire, 0, nil
	}

	var role string
	var contentBuf, reasoningBuf strings.Builder
	var finishReason string
	toolCalls := map[int]*ToolCall{}
	maxToolIdx := -1
	sawTTFT := false

	type streamChunk struct {
		Model   string `json:"model"`
		Usage   *Usage `json:"usage"`
		Choices []struct {
			Delta struct {
				Role             string `json:"role"`
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		raw := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if raw == "" || raw == "[DONE]" {
			continue
		}
		var chunk streamChunk
		if decErr := json.Unmarshal([]byte(raw), &chunk); decErr != nil {
			return id, wire, ttftMS, &providerDecodeError{cause: decErr}
		}
		if chunk.Model != "" {
			wire.Model = chunk.Model
		}
		if chunk.Usage != nil {
			wire.Usage = chunk.Usage
		}
		for _, ch := range chunk.Choices {
			if ch.FinishReason != "" {
				finishReason = ch.FinishReason
			}
			d := ch.Delta
			if d.Role != "" {
				role = d.Role
			}
			if d.Content != "" {
				if !sawTTFT {
					sawTTFT = true
					ttftMS = time.Since(start).Milliseconds()
				}
				contentBuf.WriteString(d.Content)
			}
			if d.ReasoningContent != "" {
				if !sawTTFT {
					sawTTFT = true
					ttftMS = time.Since(start).Milliseconds()
				}
				reasoningBuf.WriteString(d.ReasoningContent)
			}
			for _, tcDelta := range d.ToolCalls {
				if !sawTTFT {
					sawTTFT = true
					ttftMS = time.Since(start).Milliseconds()
				}
				tc, ok := toolCalls[tcDelta.Index]
				if !ok {
					tc = &ToolCall{}
					toolCalls[tcDelta.Index] = tc
					if tcDelta.Index > maxToolIdx {
						maxToolIdx = tcDelta.Index
					}
				}
				if tcDelta.ID != "" && tc.ID == "" {
					tc.ID = tcDelta.ID
				}
				if tcDelta.Type != "" && tc.Type == "" {
					tc.Type = tcDelta.Type
				}
				if tcDelta.Function.Name != "" && tc.Function.Name == "" {
					tc.Function.Name = tcDelta.Function.Name
				}
				if tcDelta.Function.Arguments != "" {
					tc.Function.Arguments += tcDelta.Function.Arguments
				}
			}
		}
	}
	if scanErr := scanner.Err(); scanErr != nil {
		return id, wire, ttftMS, scanErr
	}
	if role == "" {
		role = "assistant"
	}
	msg := Message{
		Role:             role,
		Content:          contentBuf.String(),
		ReasoningContent: reasoningBuf.String(),
	}
	for i := 0; i <= maxToolIdx; i++ {
		if tc, ok := toolCalls[i]; ok {
			msg.ToolCalls = append(msg.ToolCalls, *tc)
		}
	}
	wire.Choices = []struct {
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	}{{Message: msg, FinishReason: finishReason}}
	return id, wire, ttftMS, nil
}

func populateChatResponseMetrics(out *ChatResponse) {
	out.ReasoningChars = len(out.Message.ReasoningContent)
	out.ContentChars = len(out.Message.Content)
	argsChars := 0
	for _, tc := range out.Message.ToolCalls {
		argsChars += len(tc.Function.Arguments)
	}
	out.ToolArgsChars = argsChars
	if out.TTFTMS > 0 && out.DurationMS >= out.TTFTMS {
		out.DecodeMS = out.DurationMS - out.TTFTMS
	}
}
// https://developers.openai.com/api/docs/guides/error-codes
func quotaErrorCode(code string) bool {
	switch code {
	case "insufficient_quota", "credit_balance_exhausted", "project_spend_limit_exceeded", "organization_spend_limit_exceeded", "organization_usage_limit_exceeded":
		return true
	}
	return false
}

func (p *ModelProvider) chatOnModel(ctx context.Context, in ChatRequest, model string) (ChatResponse, error) {
	start := time.Now()
	out := ChatResponse{}
	if len(in.Messages) == 0 {
		return out, fmt.Errorf("chat messages are required")
	}
	in.Model = model
	// OpenAI-compatible tool endpoints accept an object at the schema root,
	// but some reject root unions. Keep the full schemas for local argument
	// validation; send their alternatives as instructions without changing the
	// properties or the tool-call argument shape.
	in.Tools = append([]ToolDefinition(nil), in.Tools...)
	for i := range in.Tools {
		var parameters map[string]json.RawMessage
		if err := json.Unmarshal(in.Tools[i].Function.Parameters, &parameters); err != nil {
			return out, fmt.Errorf("tool %s parameters: %w", in.Tools[i].Function.Name, err)
		}
		changed := false
		for _, key := range []string{"oneOf", "anyOf", "allOf", "enum", "const", "not"} {
			if constraint, exists := parameters[key]; exists {
				in.Tools[i].Function.Description += " Argument constraint (checked by the service): " + key + "=" + string(constraint) + "."
				delete(parameters, key)
				changed = true
			}
		}
		if changed {
			in.Tools[i].Function.Parameters, _ = json.Marshal(parameters)
		}
	}
	var wire providerWireResponse
	client := p.client
	if p.config.Timeout <= 0 {
		// A healthy reasoning request may occupy the remaining task window.
		// The default transport timeout belongs to short dependency requests;
		// it must not silently replace the Agent's persisted task deadline.
		copy := *client
		copy.Timeout = 0
		client = &copy
		if _, bounded := ctx.Deadline(); !bounded {
			timeout, err := programrunner.ConfiguredRunTimeout()
			if err != nil {
				return out, err
			}
			// Zero is the configured absence of a task timebox, not an
			// already-expired deadline. Preserve cancellation from the caller.
			if timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, timeout)
				defer cancel()
			}
		}
	}
	var id string
	var err error
	if p.config.Stream || in.Stream {
		in.Stream = true
		if in.StreamOptions == nil {
			in.StreamOptions = &StreamOptions{IncludeUsage: true}
		}
		id, wire, out.TTFTMS, err = providerChatStream(ctx, client, p.config.BaseURL+"/chat/completions", map[string]string{"Authorization": "Bearer " + p.config.APIKey}, in, start)
	} else {
		id, err = providerJSON(ctx, client, http.MethodPost, p.config.BaseURL+"/chat/completions", map[string]string{"Authorization": "Bearer " + p.config.APIKey}, in, &wire)
	}
	out.RequestID = id
	out.DurationMS = time.Since(start).Milliseconds()
	out.Model = wire.Model
	out.Usage = wire.Usage
	if err != nil {
		var decoded *providerDecodeError
		if errors.As(err, &decoded) {
			return out, invalidProposal("response_json", decoded.Error())
		}
		return out, err
	}
	if len(wire.Choices) != 1 {
		return out, invalidProposal("choices", fmt.Sprintf("expected one chat choice, got %d", len(wire.Choices)))
	}
	out.Message = wire.Choices[0].Message
	out.FinishReason = wire.Choices[0].FinishReason
	populateChatResponseMetrics(&out)
	if out.Message.Role == "" {
		out.Message.Role = "assistant"
	}
	if out.Message.Role != "assistant" {
		return out, invalidProposal("message.role", "expected an assistant decision")
	}
	if out.FinishReason == "length" {
		return out, invalidProposal("finish_reason", "provider reported finish_reason=length; no tool proposal dispatched")
	}
	for index, call := range out.Message.ToolCalls {
		if call.Type != "" && call.Type != "function" || strings.TrimSpace(call.Function.Name) == "" {
			return out, invalidProposal(fmt.Sprintf("tool_calls[%d].function", index), "expected a named function call")
		}
	}
	// Argument validation/authorization belongs to ExecuteReadOnlyCall, not this transport.
	return out, nil
}

func (p *ModelProvider) Embed(ctx context.Context, texts []string) (EmbeddingResponse, error) {
	start := time.Now()
	out := EmbeddingResponse{}
	if len(texts) == 0 {
		return out, fmt.Errorf("embedding batch is empty")
	}
	for _, s := range texts {
		if strings.TrimSpace(s) == "" {
			return out, fmt.Errorf("embedding input is empty")
		}
	}
	in := struct {
		Model          string   `json:"model"`
		Input          []string `json:"input"`
		EncodingFormat string   `json:"encoding_format"`
	}{p.config.Model, texts, "float"}
	var wire struct {
		Model string `json:"model"`
		Data  []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
		Usage *Usage `json:"usage"`
	}
	id, err := providerJSON(ctx, p.client, http.MethodPost, p.config.BaseURL+"/embeddings", map[string]string{"Authorization": "Bearer " + p.config.APIKey}, in, &wire)
	out.RequestID = id
	out.DurationMS = time.Since(start).Milliseconds()
	out.Model = wire.Model
	out.Usage = wire.Usage
	if err != nil {
		return out, err
	}
	if len(wire.Data) != len(texts) {
		return out, fmt.Errorf("embedding count does not match input")
	}
	out.Vectors = make([][]float32, len(texts))
	dimension := 0
	for _, entry := range wire.Data {
		if entry.Index < 0 || entry.Index >= len(texts) || out.Vectors[entry.Index] != nil || len(entry.Embedding) == 0 {
			return out, fmt.Errorf("invalid embedding index or empty vector")
		}
		if dimension == 0 {
			dimension = len(entry.Embedding)
		}
		if len(entry.Embedding) != dimension {
			return out, fmt.Errorf("inconsistent embedding dimensions")
		}
		out.Vectors[entry.Index] = entry.Embedding
	}
	return out, nil
}

type SearchConfig struct {
	Endpoint string
	APIKey   string
	Timeout  time.Duration
}
type SearchCandidate struct {
	URL       string `json:"url"`
	Title     string `json:"title"`
	Snippet   string `json:"snippet,omitempty"`
	Discovery string `json:"discovery"` // discovery metadata, not verified acquisition evidence.
}
type SearchResponse struct {
	Candidates []SearchCandidate `json:"candidates"`
	DurationMS int64             `json:"duration_ms"`
	Model      string            `json:"model,omitempty"`
	RequestID  string            `json:"request_id,omitempty"`
	Usage      *Usage            `json:"usage,omitempty"`
}
type SearchProvider interface {
	Search(context.Context, string, int) (SearchResponse, error)
}
type BraveSearch struct {
	config SearchConfig
	client *http.Client
}

func NewBraveSearch(c SearchConfig) (*BraveSearch, error) {
	if c.Endpoint == "" {
		c.Endpoint = "https://api.search.brave.com/res/v1/web/search"
	}
	if err := configuredURL(c.Endpoint); err != nil {
		return nil, err
	}
	if strings.TrimSpace(c.APIKey) == "" {
		return nil, fmt.Errorf("search API key is not configured")
	}
	return &BraveSearch{c, providerClient(c.Timeout)}, nil
}
func (p *BraveSearch) Search(ctx context.Context, query string, count int) (SearchResponse, error) {
	start := time.Now()
	out := SearchResponse{Candidates: []SearchCandidate{}}
	if strings.TrimSpace(query) == "" || count < 1 || count > 20 {
		return out, fmt.Errorf("search needs a query and count between 1 and 20")
	}
	u, _ := url.Parse(p.config.Endpoint)
	q := u.Query()
	q.Set("q", query)
	q.Set("count", fmt.Sprint(count))
	u.RawQuery = q.Encode()
	var wire struct {
		Web struct {
			Results []struct {
				URL         string `json:"url"`
				Title       string `json:"title"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	_, err := providerJSON(ctx, p.client, http.MethodGet, u.String(), map[string]string{"X-Subscription-Token": p.config.APIKey}, nil, &wire)
	out.DurationMS = time.Since(start).Milliseconds()
	if err != nil {
		return out, err
	}
	seen := map[string]bool{}
	for _, r := range wire.Web.Results {
		u, e := url.Parse(r.URL)
		if e != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || seen[r.URL] {
			continue
		}
		seen[r.URL] = true
		out.Candidates = append(out.Candidates, SearchCandidate{r.URL, r.Title, r.Description, "brave_search"})
		if len(out.Candidates) == count {
			break
		}
	}
	return out, nil
}

func providerDiagnostic(message string, headers map[string]string) string {
	for _, value := range headers {
		message = strings.ReplaceAll(message, value, "[redacted]")
		if strings.HasPrefix(value, "Bearer ") {
			message = strings.ReplaceAll(message, strings.TrimPrefix(value, "Bearer "), "[redacted]")
		}
	}
	return strings.TrimSpace(message)
}

func (p *ModelProvider) Chat(ctx context.Context, in ChatRequest) (ChatResponse, error) {
	p.routeMu.Lock()
	selected := p.preferredModel
	p.routeMu.Unlock()
	if selected == "" {
		selected = p.config.Model
	}
	out, err := p.chatOnModel(ctx, in, selected)
	if err == nil {
		p.rememberModel(selected)
		return out, nil
	}
	if ctx.Err() != nil || !channelFailure(err) {
		return out, err
	}
	failures := []error{err}
	var catalog struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if _, listErr := providerJSON(ctx, p.client, http.MethodGet, p.config.BaseURL+"/models", map[string]string{"Authorization": "Bearer " + p.config.APIKey}, nil, &catalog); listErr != nil {
		return out, errors.Join(err, listErr)
	}
	tried := map[string]bool{selected: true}
	for _, candidate := range catalog.Data {
		if tried[candidate.ID] || !isCompatibleChannel(p.config.Model, candidate.ID) {
			continue
		}
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		tried[candidate.ID] = true
		next, nextErr := p.chatOnModel(ctx, in, candidate.ID)
		if nextErr == nil {
			p.rememberModel(candidate.ID)
			return next, nil
		}
		failures = append(failures, nextErr)
		if !channelFailure(nextErr) {
			return next, nextErr
		}
	}
	return out, errors.Join(failures...)
}

func (p *ModelProvider) rememberModel(model string) {
	p.routeMu.Lock()
	p.preferredModel = model
	p.routeMu.Unlock()
}

func isCompatibleChannel(target, candidate string) bool {
	candidate = strings.TrimSpace(candidate)
	if strings.HasSuffix(candidate, ":batch") {
		return false
	}
	tf := modelFamily(target)
	cf := modelFamily(candidate)
	if tf == cf {
		return true
	}
	tLow := strings.ToLower(target)
	cLow := strings.ToLower(candidate)
	if (strings.Contains(tLow, "deepseek") || strings.Contains(tLow, "ds")) && strings.Contains(tLow, "flash") {
		return strings.Contains(cLow, "deepseek") && strings.Contains(cLow, "flash")
	}
	return false
}

func modelFamily(id string) string {
	id = strings.TrimSpace(id)
	return strings.ToLower(id[strings.LastIndex(id, "/")+1:])
}

func channelFailure(err error) bool {
	var remote *ProviderError
	if errors.As(err, &remote) {
		return remote.QuotaExhausted || remote.Retryable
	}
	var transport *url.Error
	return errors.As(err, &transport)
}
