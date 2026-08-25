package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/jinyule/nano-harness/internal/app/llm"
	"github.com/jinyule/nano-harness/internal/core/session"
)

type responsesContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

type responsesInput struct {
	Type      string             `json:"type,omitempty"`
	Role      string             `json:"role,omitempty"`
	Content   []responsesContent `json:"content,omitempty"`
	CallID    string             `json:"call_id,omitempty"`
	Name      string             `json:"name,omitempty"`
	Arguments string             `json:"arguments,omitempty"`
	Output    string             `json:"output,omitempty"`
}

type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type responsesRequest struct {
	Model             string           `json:"model"`
	Instructions      string           `json:"instructions,omitempty"`
	Input             []responsesInput `json:"input"`
	Tools             []responsesTool  `json:"tools,omitempty"`
	ToolChoice        string           `json:"tool_choice,omitempty"`
	ParallelToolCalls bool             `json:"parallel_tool_calls"`
	MaxOutputTokens   int              `json:"max_output_tokens,omitempty"`
	Stream            bool             `json:"stream"`
	Store             bool             `json:"store"`
}

func (provider *Provider) streamResponses(ctx context.Context, current *snapshot, model llm.ModelInfo, credential llm.Credential, request llm.Request, emit llm.Emit) (llm.Completion, error) {
	payload, err := provider.responsesRequest(model, request)
	if err != nil {
		return llm.Completion{}, err
	}
	endpoint := current.baseURL + "/v1/responses"
	headers := map[string]string{}
	switch credential.Kind {
	case llm.CredentialAPIKey:
		headers["Authorization"] = "Bearer " + credential.APIKey
	case llm.CredentialOAuth:
		if credential.AccountID == "" {
			return llm.Completion{}, &llm.Error{Code: llm.ErrorUnauthorized, Provider: provider.id, Cause: errors.New("ChatGPT account ID is missing")}
		}
		endpoint = provider.auth.chatGPTBaseURL + "/backend-api/codex/responses"
		headers["Authorization"] = "Bearer " + credential.AccessToken
		headers["ChatGPT-Account-Id"] = credential.AccountID
		headers["Originator"] = "codex_cli_rs"
		headers["OpenAI-Beta"] = "responses=experimental"
	default:
		return llm.Completion{}, llm.ErrNoCredential
	}
	return provider.streamRequest(ctx, endpoint, payload, headers, func(body io.Reader) (llm.Completion, error) {
		return provider.consumeResponses(body, emit)
	})
}

func (provider *Provider) responsesRequest(model llm.ModelInfo, request llm.Request) (responsesRequest, error) {
	input := make([]responsesInput, 0, len(request.Surface))
	for _, node := range request.Surface {
		switch {
		case node.Message != nil:
			content := make([]responsesContent, 0, len(node.Message.Content))
			for _, block := range node.Message.Content {
				switch block.Type {
				case session.ContentText:
					typeName := "input_text"
					if node.Message.Role == session.RoleAssistant {
						typeName = "output_text"
					}
					content = append(content, responsesContent{Type: typeName, Text: block.Text})
				case session.ContentImage:
					if node.Message.Role != session.RoleUser || block.Image == nil {
						return responsesRequest{}, &llm.Error{Code: llm.ErrorInvalid, Provider: provider.id, Cause: errors.New("only user messages may contain images")}
					}
					content = append(content, responsesContent{Type: "input_image", ImageURL: "data:" + block.Image.MediaType + ";base64," + block.Image.Data})
				}
			}
			if len(content) == 0 {
				continue
			}
			input = append(input, responsesInput{Role: string(node.Message.Role), Content: content})
		case node.Call != nil:
			input = append(input, responsesInput{Type: "function_call", CallID: node.Call.ID, Name: node.Call.Name, Arguments: string(node.Call.Arguments)})
		case node.Result != nil:
			input = append(input, responsesInput{Type: "function_call_output", CallID: node.Result.CallID, Output: node.Result.Output})
		}
	}
	tools := make([]responsesTool, len(request.Tools))
	for index, tool := range request.Tools {
		tools[index] = responsesTool{Type: "function", Name: tool.Name, Description: tool.Description, Parameters: tool.Parameters}
	}
	payload := responsesRequest{
		Model: model.ID, Instructions: request.System, Input: input, Tools: tools,
		ParallelToolCalls: true, MaxOutputTokens: request.MaxTokens, Stream: true, Store: false,
	}
	if len(tools) > 0 {
		payload.ToolChoice = "auto"
	}
	return payload, nil
}

type responsesEvent struct {
	Type        string `json:"type"`
	Delta       string `json:"delta"`
	OutputIndex int    `json:"output_index"`
	Response    struct {
		Status string `json:"status"`
		Error  any    `json:"error"`
		Usage  struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
			InputDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"input_tokens_details"`
		} `json:"usage"`
	} `json:"response"`
	Item struct {
		Type      string `json:"type"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"item"`
}

type responsesAccumulator struct {
	text      strings.Builder
	reasoning strings.Builder
	calls     map[int]*session.ToolCall
	completed bool
	usage     *session.TokenUsage
}

func (provider *Provider) consumeResponses(body io.Reader, emit llm.Emit) (llm.Completion, error) {
	state := responsesAccumulator{calls: map[int]*session.ToolCall{}}
	err := scanSSE(body, provider.id, func(data []byte) error {
		var event responsesEvent
		if err := json.Unmarshal(data, &event); err != nil {
			return &llm.Error{Code: llm.ErrorProtocol, Provider: provider.id, Cause: err}
		}
		switch event.Type {
		case "response.output_text.delta":
			if err := appendBounded(&state.text, event.Delta, session.MaxTextBytes, "assistant text", provider.id); err != nil {
				return err
			}
			return emit(session.AssistantChunk{Kind: session.ChunkText, Text: event.Delta})
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			if err := appendBounded(&state.reasoning, event.Delta, session.MaxTextBytes, "assistant reasoning", provider.id); err != nil {
				return err
			}
			return emit(session.AssistantChunk{Kind: session.ChunkReasoning, Text: event.Delta})
		case "response.output_item.added":
			if event.Item.Type == "function_call" {
				if event.OutputIndex < 0 || event.OutputIndex >= maxProviderToolCalls {
					return &llm.Error{Code: llm.ErrorProtocol, Provider: provider.id, Cause: errors.New("tool index exceeds limit")}
				}
				state.calls[event.OutputIndex] = &session.ToolCall{ID: event.Item.CallID, Name: event.Item.Name, Arguments: json.RawMessage{}}
				return emit(session.AssistantChunk{Kind: session.ChunkTool, Index: event.OutputIndex, CallID: event.Item.CallID, Name: event.Item.Name})
			}
		case "response.function_call_arguments.delta":
			if event.OutputIndex < 0 || event.OutputIndex >= maxProviderToolCalls {
				return &llm.Error{Code: llm.ErrorProtocol, Provider: provider.id, Cause: errors.New("tool index exceeds limit")}
			}
			call := state.calls[event.OutputIndex]
			if call == nil {
				call = &session.ToolCall{}
				state.calls[event.OutputIndex] = call
			}
			if len(call.Arguments)+len(event.Delta) > session.MaxArgumentsBytes {
				return &llm.Error{Code: llm.ErrorProtocol, Provider: provider.id, Cause: errors.New("tool arguments exceed size limit")}
			}
			call.Arguments = append(call.Arguments, event.Delta...)
			return emit(session.AssistantChunk{Kind: session.ChunkTool, Index: event.OutputIndex, Arguments: event.Delta})
		case "response.output_item.done":
			if event.Item.Type == "function_call" {
				if event.OutputIndex < 0 || event.OutputIndex >= maxProviderToolCalls {
					return &llm.Error{Code: llm.ErrorProtocol, Provider: provider.id, Cause: errors.New("tool index exceeds limit")}
				}
				call := state.calls[event.OutputIndex]
				if call == nil {
					call = &session.ToolCall{}
					state.calls[event.OutputIndex] = call
				}
				call.ID, call.Name = event.Item.CallID, event.Item.Name
				if event.Item.Arguments != "" {
					call.Arguments = json.RawMessage(event.Item.Arguments)
				}
			}
		case "response.completed":
			state.completed = true
			state.usage = &session.TokenUsage{
				InputTokens: event.Response.Usage.InputTokens, OutputTokens: event.Response.Usage.OutputTokens,
				CacheReadTokens: event.Response.Usage.InputDetails.CachedTokens,
			}
		case "response.failed", "response.incomplete", "error":
			return &llm.Error{Code: llm.ErrorInvalid, Provider: provider.id, Cause: fmt.Errorf("provider event %s", event.Type)}
		}
		return nil
	})
	if err != nil {
		return llm.Completion{}, err
	}
	if !state.completed {
		return llm.Completion{}, &llm.Error{Code: llm.ErrorProtocol, Provider: provider.id, Cause: errors.New("stream ended before response.completed")}
	}
	calls := make([]session.ToolCall, 0, len(state.calls))
	for index := 0; index <= maxIndex(state.calls); index++ {
		if call := state.calls[index]; call != nil {
			if err := validToolCall(*call); err != nil {
				return llm.Completion{}, &llm.Error{Code: llm.ErrorProtocol, Provider: provider.id, Cause: err}
			}
			calls = append(calls, *call)
		}
	}
	if state.text.Len() == 0 && len(calls) == 0 {
		return llm.Completion{}, &llm.Error{Code: llm.ErrorEmptyResponse, Provider: provider.id}
	}
	return llm.Completion{Message: assistantMessage(state.text.String()), Calls: calls, Usage: state.usage, Stop: "completed"}, nil
}

func maxIndex(values map[int]*session.ToolCall) int {
	maximum := -1
	for index := range values {
		if index > maximum {
			maximum = index
		}
	}
	return maximum
}
