package aiapp

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/eino-contrib/jsonschema"
)

// NewEinoChatProviderModel adapts only the chat protocol. Accounting, completion
// acceptance and dependency policy stay with the supplied ChatProvider/host.
// Defaults are copied per request. Eino supplies messages and dynamic tools.
func NewEinoChatProviderModel(provider ChatProvider, defaults ChatRequest) model.BaseModel[*schema.Message] {
	return &einoProviderModel{provider: provider, defaults: defaults}
}

type einoProviderModel struct {
	provider ChatProvider
	defaults ChatRequest
}

func (m *einoProviderModel) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	if m.provider == nil {
		return nil, fmt.Errorf("chat provider is not configured")
	}
	request := m.defaults
	request.Messages = ChatMessages(input)
	options := model.GetCommonOptions(nil, opts...)
	var err error
	if options.Tools != nil {
		request.Tools, err = ToolDefinitionsFromEino(options.Tools)
		if err != nil {
			return nil, err
		}
	}
	if options.Model != nil {
		request.Model = *options.Model
	}
	if options.MaxTokens != nil {
		request.MaxCompletionTokens = *options.MaxTokens
	}
	response, err := m.provider.Chat(ctx, request)
	if err != nil {
		return nil, err
	}
	return EinoMessageFromChatResponse(response), nil
}
func (m *einoProviderModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	response, err := m.Generate(ctx, input, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{response}), nil
}

// EinoMessageFromChatResponse retains provider usage without estimating unknown
// usage as zero. The original ChatResponse remains the host's durable receipt.
func EinoMessageFromChatResponse(response ChatResponse) *schema.Message {
	out := toEinoMessage(response.Message)
	out.ResponseMeta = &schema.ResponseMeta{FinishReason: response.FinishReason}
	if response.Usage != nil {
		u := response.Usage
		out.ResponseMeta.Usage = &schema.TokenUsage{PromptTokens: int(u.PromptTokens), CompletionTokens: int(u.CompletionTokens), TotalTokens: int(u.TotalTokens)}
		out.ResponseMeta.Usage.PromptTokenDetails.CachedTokens = int(u.Cached())
		if u.CompletionTokensDetail != nil && u.CompletionTokensDetail.ReasoningTokens != nil {
			out.ResponseMeta.Usage.CompletionTokensDetails.ReasoningTokens = int(*u.CompletionTokensDetail.ReasoningTokens)
		}
	}
	return out
}

// EinoMessages preserves tool pairing, opaque reasoning continuation and cache
// metadata. Nested provider metadata is JSON encoded so checkpoint gob stays
// independent of arbitrary unregistered provider-specific interface types.
func EinoMessages(messages []Message) []*schema.Message { return toEinoMessages(messages) }

// ChatMessages converts Eino history to the existing provider protocol.
func ChatMessages(messages []*schema.Message) []Message { return fromEinoMessages(messages) }

// EinoToolInfos preserves the real JSON schemas, including oneOf and limits.
func EinoToolInfos(defs []ToolDefinition) ([]*schema.ToolInfo, error) { return toolInfos(defs) }

// ToolDefinitionsFromEino converts Eino's current filtered tool list.
func ToolDefinitionsFromEino(infos []*schema.ToolInfo) ([]ToolDefinition, error) {
	return fromToolInfos(infos)
}

func toolInfos(defs []ToolDefinition) ([]*schema.ToolInfo, error) {
	out := make([]*schema.ToolInfo, 0, len(defs))
	for _, d := range defs {
		var params jsonschema.Schema
		if err := json.Unmarshal(d.Function.Parameters, &params); err != nil {
			return nil, err
		}
		out = append(out, &schema.ToolInfo{Name: d.Function.Name, Desc: d.Function.Description, ParamsOneOf: schema.NewParamsOneOfByJSONSchema(&params)})
	}
	return out, nil
}
func fromToolInfos(infos []*schema.ToolInfo) ([]ToolDefinition, error) {
	out := make([]ToolDefinition, 0, len(infos))
	for _, info := range infos {
		params, err := info.ToJSONSchema()
		if err != nil {
			return nil, err
		}
		raw, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		out = append(out, ToolDefinition{Type: "function", Function: FunctionDefinition{Name: info.Name, Description: info.Desc, Parameters: raw}})
	}
	return out, nil
}
func toEinoMessages(messages []Message) []*schema.Message {
	out := make([]*schema.Message, 0, len(messages))
	for _, m := range messages {
		out = append(out, toEinoMessage(m))
	}
	return out
}
func toEinoMessage(m Message) *schema.Message {
	out := &schema.Message{Role: schema.RoleType(m.Role), Content: m.Content, ReasoningContent: m.ReasoningContent, ToolCallID: m.ToolCallID}
	if m.CacheControl != nil {
		raw, _ := json.Marshal(m.CacheControl)
		out.Extra = map[string]any{"gorelay_cache_control": string(raw)}
	}
	for _, c := range m.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, schema.ToolCall{ID: c.ID, Type: c.Type, Function: schema.FunctionCall{Name: c.Function.Name, Arguments: c.Function.Arguments}})
	}
	return out
}
func fromEinoMessages(messages []*schema.Message) []Message {
	out := make([]Message, 0, len(messages))
	for _, m := range messages {
		if m == nil {
			continue
		}
		v := Message{Role: string(m.Role), Content: m.Content, ReasoningContent: m.ReasoningContent, ToolCallID: m.ToolCallID}
		if m.Extra != nil {
			if raw, ok := m.Extra["gorelay_cache_control"].(string); ok {
				_ = json.Unmarshal([]byte(raw), &v.CacheControl)
			}
		}
		for _, c := range m.ToolCalls {
			v.ToolCalls = append(v.ToolCalls, ToolCall{ID: c.ID, Type: c.Type, Function: FunctionCall{Name: c.Function.Name, Arguments: c.Function.Arguments}})
		}
		out = append(out, v)
	}
	return out
}
