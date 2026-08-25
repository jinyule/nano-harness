package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"

	"github.com/jinyule/nano-harness/internal/app/llm"
	"github.com/jinyule/nano-harness/internal/core/session"
)

type chatImageURL struct {
	URL string `json:"url"`
}

type chatPart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *chatImageURL `json:"image_url,omitempty"`
}

type chatFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function chatFunctionCall `json:"function"`
}

type chatMessage struct {
	Role       string         `json:"role"`
	Content    any            `json:"content,omitempty"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type chatTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type chatRequest struct {
	Model             string        `json:"model"`
	Messages          []chatMessage `json:"messages"`
	Tools             []chatTool    `json:"tools,omitempty"`
	ToolChoice        string        `json:"tool_choice,omitempty"`
	ParallelToolCalls bool          `json:"parallel_tool_calls"`
	MaxTokens         int           `json:"max_tokens,omitempty"`
	Stream            bool          `json:"stream"`
	StreamOptions     struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
}

func (provider *Provider) streamOpenRouter(ctx context.Context, current *snapshot, model llm.ModelInfo, credential llm.Credential, request llm.Request, emit llm.Emit) (llm.Completion, error) {
	if credential.Kind != llm.CredentialAPIKey {
		return llm.Completion{}, &llm.Error{Code: llm.ErrorUnauthorized, Provider: provider.id, Cause: errors.New("OpenRouter requires an API key or exchanged OAuth key")}
	}
	payload, err := provider.chatRequest(model, request)
	if err != nil {
		return llm.Completion{}, err
	}
	headers := map[string]string{"Authorization": "Bearer " + credential.APIKey}
	return provider.streamRequest(ctx, current.baseURL+"/chat/completions", payload, headers, func(body io.Reader) (llm.Completion, error) {
		return provider.consumeChat(body, emit)
	})
}

func (provider *Provider) chatRequest(model llm.ModelInfo, request llm.Request) (chatRequest, error) {
	messages := make([]chatMessage, 0, len(request.Surface)+1)
	if request.System != "" {
		messages = append(messages, chatMessage{Role: "system", Content: request.System})
	}
	for _, node := range request.Surface {
		switch {
		case node.Message != nil:
			parts := make([]chatPart, 0, len(node.Message.Content))
			hasImage := false
			for _, block := range node.Message.Content {
				switch block.Type {
				case session.ContentText:
					parts = append(parts, chatPart{Type: "text", Text: block.Text})
				case session.ContentImage:
					if node.Message.Role != session.RoleUser || block.Image == nil {
						return chatRequest{}, &llm.Error{Code: llm.ErrorInvalid, Provider: provider.id, Cause: errors.New("only user messages may contain images")}
					}
					hasImage = true
					parts = append(parts, chatPart{Type: "image_url", ImageURL: &chatImageURL{URL: "data:" + block.Image.MediaType + ";base64," + block.Image.Data}})
				}
			}
			var content any = session.Text(*node.Message)
			if hasImage {
				content = parts
			}
			messages = append(messages, chatMessage{Role: string(node.Message.Role), Content: content})
		case node.Call != nil:
			call := chatToolCall{ID: node.Call.ID, Type: "function", Function: chatFunctionCall{Name: node.Call.Name, Arguments: string(node.Call.Arguments)}}
			if len(messages) > 0 && messages[len(messages)-1].Role == "assistant" {
				messages[len(messages)-1].ToolCalls = append(messages[len(messages)-1].ToolCalls, call)
			} else {
				messages = append(messages, chatMessage{Role: "assistant", ToolCalls: []chatToolCall{call}})
			}
		case node.Result != nil:
			messages = append(messages, chatMessage{Role: "tool", Content: node.Result.Output, ToolCallID: node.Result.CallID})
		}
	}
	tools := make([]chatTool, len(request.Tools))
	for index, tool := range request.Tools {
		tools[index].Type = "function"
		tools[index].Function.Name = tool.Name
		tools[index].Function.Description = tool.Description
		tools[index].Function.Parameters = tool.Parameters
	}
	payload := chatRequest{
		Model: model.ID, Messages: messages, Tools: tools, ParallelToolCalls: true,
		MaxTokens: request.MaxTokens, Stream: true,
	}
	payload.StreamOptions.IncludeUsage = true
	if len(tools) > 0 {
		payload.ToolChoice = "auto"
	}
	return payload, nil
}

type chatEvent struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			Reasoning        string `json:"reasoning"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		PromptDetails    struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
	Error any `json:"error"`
}

type chatAccumulator struct {
	text      strings.Builder
	reasoning strings.Builder
	calls     map[int]*session.ToolCall
	usage     *session.TokenUsage
	stop      string
}

func (provider *Provider) consumeChat(body io.Reader, emit llm.Emit) (llm.Completion, error) {
	state := chatAccumulator{calls: map[int]*session.ToolCall{}}
	err := scanSSE(body, provider.id, func(data []byte) error {
		var event chatEvent
		if err := json.Unmarshal(data, &event); err != nil {
			return &llm.Error{Code: llm.ErrorProtocol, Provider: provider.id, Cause: err}
		}
		if event.Error != nil {
			return &llm.Error{Code: llm.ErrorInvalid, Provider: provider.id}
		}
		if event.Usage != nil {
			state.usage = &session.TokenUsage{InputTokens: event.Usage.PromptTokens, OutputTokens: event.Usage.CompletionTokens, CacheReadTokens: event.Usage.PromptDetails.CachedTokens}
		}
		for _, choice := range event.Choices {
			if choice.Delta.Content != "" {
				if err := appendBounded(&state.text, choice.Delta.Content, session.MaxTextBytes, "assistant text", provider.id); err != nil {
					return err
				}
				if err := emit(session.AssistantChunk{Kind: session.ChunkText, Text: choice.Delta.Content}); err != nil {
					return err
				}
			}
			reasoning := choice.Delta.Reasoning
			if reasoning == "" {
				reasoning = choice.Delta.ReasoningContent
			}
			if reasoning != "" {
				if err := appendBounded(&state.reasoning, reasoning, session.MaxTextBytes, "assistant reasoning", provider.id); err != nil {
					return err
				}
				if err := emit(session.AssistantChunk{Kind: session.ChunkReasoning, Text: reasoning}); err != nil {
					return err
				}
			}
			for _, delta := range choice.Delta.ToolCalls {
				call := state.calls[delta.Index]
				if call == nil {
					if len(state.calls) >= maxProviderToolCalls {
						return &llm.Error{Code: llm.ErrorProtocol, Provider: provider.id, Cause: errors.New("too many tool calls")}
					}
					call = &session.ToolCall{}
					state.calls[delta.Index] = call
				}
				if delta.ID != "" {
					call.ID = delta.ID
				}
				if delta.Function.Name != "" {
					call.Name = delta.Function.Name
				}
				if len(call.Arguments)+len(delta.Function.Arguments) > session.MaxArgumentsBytes {
					return &llm.Error{Code: llm.ErrorProtocol, Provider: provider.id, Cause: errors.New("tool arguments exceed size limit")}
				}
				call.Arguments = append(call.Arguments, delta.Function.Arguments...)
				if err := emit(session.AssistantChunk{Kind: session.ChunkTool, Index: delta.Index, CallID: delta.ID, Name: delta.Function.Name, Arguments: delta.Function.Arguments}); err != nil {
					return err
				}
			}
			if choice.FinishReason != "" {
				state.stop = choice.FinishReason
			}
		}
		return nil
	})
	if err != nil {
		return llm.Completion{}, err
	}
	indexes := make([]int, 0, len(state.calls))
	for index := range state.calls {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	calls := make([]session.ToolCall, 0, len(indexes))
	for _, index := range indexes {
		call := *state.calls[index]
		if len(call.Arguments) == 0 {
			call.Arguments = json.RawMessage(`{}`)
		}
		if err := validToolCall(call); err != nil {
			return llm.Completion{}, &llm.Error{Code: llm.ErrorProtocol, Provider: provider.id, Cause: err}
		}
		calls = append(calls, call)
	}
	if state.stop == "" {
		return llm.Completion{}, &llm.Error{Code: llm.ErrorProtocol, Provider: provider.id, Cause: errors.New("stream ended without finish reason")}
	}
	if state.text.Len() == 0 && len(calls) == 0 {
		return llm.Completion{}, &llm.Error{Code: llm.ErrorEmptyResponse, Provider: provider.id}
	}
	return llm.Completion{Message: assistantMessage(state.text.String()), Calls: calls, Usage: state.usage, Stop: state.stop}, nil
}
