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

type anthropicSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type anthropicBlock struct {
	Type      string           `json:"type"`
	Text      string           `json:"text,omitempty"`
	Source    *anthropicSource `json:"source,omitempty"`
	ID        string           `json:"id,omitempty"`
	Name      string           `json:"name,omitempty"`
	Input     json.RawMessage  `json:"input,omitempty"`
	ToolUseID string           `json:"tool_use_id,omitempty"`
	Content   string           `json:"content,omitempty"`
	IsError   bool             `json:"is_error,omitempty"`
}

type anthropicMessage struct {
	Role    string           `json:"role"`
	Content []anthropicBlock `json:"content"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicRequest struct {
	Model     string             `json:"model"`
	System    string             `json:"system,omitempty"`
	Messages  []anthropicMessage `json:"messages"`
	Tools     []anthropicTool    `json:"tools,omitempty"`
	MaxTokens int                `json:"max_tokens"`
	Stream    bool               `json:"stream"`
}

func (provider *Provider) streamAnthropic(ctx context.Context, current *snapshot, model llm.ModelInfo, credential llm.Credential, request llm.Request, emit llm.Emit) (llm.Completion, error) {
	payload, err := provider.anthropicRequest(model, request)
	if err != nil {
		return llm.Completion{}, err
	}
	headers := map[string]string{"anthropic-version": "2023-06-01"}
	switch credential.Kind {
	case llm.CredentialAPIKey:
		headers["x-api-key"] = credential.APIKey
	case llm.CredentialOAuth:
		headers["Authorization"] = "Bearer " + credential.AccessToken
		headers["anthropic-beta"] = "oauth-2025-04-20"
	default:
		return llm.Completion{}, llm.ErrNoCredential
	}
	return provider.streamRequest(ctx, current.baseURL+"/v1/messages", payload, headers, func(body io.Reader) (llm.Completion, error) {
		return provider.consumeAnthropic(body, emit)
	})
}

func (provider *Provider) anthropicRequest(model llm.ModelInfo, request llm.Request) (anthropicRequest, error) {
	messages := make([]anthropicMessage, 0, len(request.Surface))
	appendBlocks := func(role string, blocks ...anthropicBlock) {
		if len(messages) > 0 && messages[len(messages)-1].Role == role {
			messages[len(messages)-1].Content = append(messages[len(messages)-1].Content, blocks...)
			return
		}
		messages = append(messages, anthropicMessage{Role: role, Content: blocks})
	}
	for _, node := range request.Surface {
		switch {
		case node.Message != nil:
			blocks := make([]anthropicBlock, 0, len(node.Message.Content))
			for _, block := range node.Message.Content {
				switch block.Type {
				case session.ContentText:
					blocks = append(blocks, anthropicBlock{Type: "text", Text: block.Text})
				case session.ContentImage:
					if node.Message.Role != session.RoleUser || block.Image == nil {
						return anthropicRequest{}, &llm.Error{Code: llm.ErrorInvalid, Provider: provider.id, Cause: errors.New("only user messages may contain images")}
					}
					blocks = append(blocks, anthropicBlock{Type: "image", Source: &anthropicSource{Type: "base64", MediaType: block.Image.MediaType, Data: block.Image.Data}})
				}
			}
			appendBlocks(string(node.Message.Role), blocks...)
		case node.Call != nil:
			appendBlocks("assistant", anthropicBlock{Type: "tool_use", ID: node.Call.ID, Name: node.Call.Name, Input: node.Call.Arguments})
		case node.Result != nil:
			appendBlocks("user", anthropicBlock{Type: "tool_result", ToolUseID: node.Result.CallID, Content: node.Result.Output, IsError: node.Result.IsError})
		}
	}
	tools := make([]anthropicTool, len(request.Tools))
	for index, tool := range request.Tools {
		tools[index] = anthropicTool{Name: tool.Name, Description: tool.Description, InputSchema: tool.Parameters}
	}
	maxTokens := request.MaxTokens
	if maxTokens == 0 {
		maxTokens = 8192
	}
	return anthropicRequest{Model: model.ID, System: request.System, Messages: messages, Tools: tools, MaxTokens: maxTokens, Stream: true}, nil
}

type anthropicEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message struct {
		Usage struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
	ContentBlock struct {
		Type  string          `json:"type"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	} `json:"content_block"`
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage struct {
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error any `json:"error"`
}

type anthropicAccumulator struct {
	text      strings.Builder
	reasoning strings.Builder
	calls     map[int]*session.ToolCall
	usage     session.TokenUsage
	stop      string
	completed bool
}

func (provider *Provider) consumeAnthropic(body io.Reader, emit llm.Emit) (llm.Completion, error) {
	state := anthropicAccumulator{calls: map[int]*session.ToolCall{}}
	err := scanSSE(body, provider.id, func(data []byte) error {
		var event anthropicEvent
		if err := json.Unmarshal(data, &event); err != nil {
			return &llm.Error{Code: llm.ErrorProtocol, Provider: provider.id, Cause: err}
		}
		switch event.Type {
		case "message_start":
			state.usage.InputTokens = event.Message.Usage.InputTokens
			state.usage.OutputTokens = event.Message.Usage.OutputTokens
			state.usage.CacheReadTokens = event.Message.Usage.CacheReadInputTokens
			state.usage.CacheWriteTokens = event.Message.Usage.CacheCreationInputTokens
		case "content_block_start":
			if event.ContentBlock.Type == "tool_use" {
				if event.Index < 0 || event.Index >= maxProviderToolCalls || len(state.calls) >= maxProviderToolCalls {
					return &llm.Error{Code: llm.ErrorProtocol, Provider: provider.id, Cause: errors.New("too many tool calls")}
				}
				arguments := event.ContentBlock.Input
				if string(arguments) == "{}" {
					arguments = nil
				}
				state.calls[event.Index] = &session.ToolCall{ID: event.ContentBlock.ID, Name: event.ContentBlock.Name, Arguments: arguments}
				return emit(session.AssistantChunk{Kind: session.ChunkTool, Index: event.Index, CallID: event.ContentBlock.ID, Name: event.ContentBlock.Name})
			}
		case "content_block_delta":
			switch event.Delta.Type {
			case "text_delta":
				if err := appendBounded(&state.text, event.Delta.Text, session.MaxTextBytes, "assistant text", provider.id); err != nil {
					return err
				}
				return emit(session.AssistantChunk{Kind: session.ChunkText, Text: event.Delta.Text})
			case "thinking_delta":
				if err := appendBounded(&state.reasoning, event.Delta.Thinking, session.MaxTextBytes, "assistant reasoning", provider.id); err != nil {
					return err
				}
				return emit(session.AssistantChunk{Kind: session.ChunkReasoning, Text: event.Delta.Thinking})
			case "input_json_delta":
				call := state.calls[event.Index]
				if call == nil {
					return &llm.Error{Code: llm.ErrorProtocol, Provider: provider.id, Cause: errors.New("tool delta arrived before tool start")}
				}
				if len(call.Arguments)+len(event.Delta.PartialJSON) > session.MaxArgumentsBytes {
					return &llm.Error{Code: llm.ErrorProtocol, Provider: provider.id, Cause: errors.New("tool arguments exceed size limit")}
				}
				call.Arguments = append(call.Arguments, event.Delta.PartialJSON...)
				return emit(session.AssistantChunk{Kind: session.ChunkTool, Index: event.Index, Arguments: event.Delta.PartialJSON})
			}
		case "message_delta":
			state.stop = event.Delta.StopReason
			state.usage.OutputTokens = event.Usage.OutputTokens
		case "message_stop":
			state.completed = true
		case "error":
			return &llm.Error{Code: llm.ErrorInvalid, Provider: provider.id}
		}
		return nil
	})
	if err != nil {
		return llm.Completion{}, err
	}
	if !state.completed {
		return llm.Completion{}, &llm.Error{Code: llm.ErrorProtocol, Provider: provider.id, Cause: errors.New("stream ended before message_stop")}
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
	if state.text.Len() == 0 && len(calls) == 0 {
		return llm.Completion{}, &llm.Error{Code: llm.ErrorEmptyResponse, Provider: provider.id}
	}
	return llm.Completion{Message: assistantMessage(state.text.String()), Calls: calls, Usage: &state.usage, Stop: state.stop}, nil
}
