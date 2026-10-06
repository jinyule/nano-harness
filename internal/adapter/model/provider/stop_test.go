package provider

import (
	"errors"
	"strings"
	"testing"

	"github.com/jinyule/nano-harness/internal/app/llm"
	"github.com/jinyule/nano-harness/internal/core/session"
)

func TestProvider_OutputLimitMapsToMaxTokens(t *testing.T) {
	emitOK := func(session.AssistantChunk) error { return nil }
	for _, test := range []struct {
		id, stream string
	}{
		{"openai", sse(`{"type":"response.output_text.delta","delta":"partial"}`) + sse(`{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":3,"output_tokens":2}}}`)},
		{"anthropic", sse(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"partial"}}`) + sse(`{"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":2}}`) + sse(`{"type":"message_stop"}`)},
		{"openrouter", sse(`{"choices":[{"delta":{"content":"partial"},"finish_reason":"length"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`)},
	} {
		t.Run(test.id, func(t *testing.T) {
			provider := &Provider{id: test.id}
			var completion llm.Completion
			var err error
			switch test.id {
			case "openai":
				completion, err = provider.consumeResponses(strings.NewReader(test.stream), emitOK)
			case "anthropic":
				completion, err = provider.consumeAnthropic(strings.NewReader(test.stream), emitOK)
			case "openrouter":
				completion, err = provider.consumeChat(strings.NewReader(test.stream), emitOK)
			}
			if err != nil || completion.Stop != "max_tokens" || completion.Usage == nil || completion.Usage.OutputTokens != 2 {
				t.Fatalf("output limit = %+v, %v", completion, err)
			}
		})
	}
}

func TestProvider_OutputLimitRetainsChunksWithoutExecutingPartialCalls(t *testing.T) {
	for _, test := range []struct{ id, stream string }{
		{"openai", sse(`{"type":"response.reasoning_text.delta","delta":"thinking"}`, `{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"c","name":"tool"}}`, `{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{"}`, `{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}`)},
		{"anthropic", sse(`{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"thinking"}}`, `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"c","name":"tool","input":{}}}`, `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{"}}`, `{"type":"message_delta","delta":{"stop_reason":"max_tokens"}}`, `{"type":"message_stop"}`)},
		{"openrouter", sse(`{"choices":[{"delta":{"reasoning":"thinking","tool_calls":[{"index":0,"id":"c","function":{"name":"tool","arguments":"{"}}]},"finish_reason":"length"}]}`)},
	} {
		t.Run(test.id, func(t *testing.T) {
			provider := &Provider{id: test.id}
			var chunks []session.AssistantChunk
			emit := func(chunk session.AssistantChunk) error { chunks = append(chunks, chunk); return nil }
			var completion llm.Completion
			var err error
			switch test.id {
			case "openai":
				completion, err = provider.consumeResponses(strings.NewReader(test.stream), emit)
			case "anthropic":
				completion, err = provider.consumeAnthropic(strings.NewReader(test.stream), emit)
			case "openrouter":
				completion, err = provider.consumeChat(strings.NewReader(test.stream), emit)
			}
			if err != nil || completion.Stop != llm.StopMaxTokens || len(completion.Calls) != 0 || session.Text(completion.Message) != "" || len(chunks) < 2 || chunks[0].Kind != session.ChunkReasoning {
				t.Fatalf("truncated proposal = %+v, chunks=%+v, %v", completion, chunks, err)
			}
		})
	}
}

func TestResponses_RejectsOtherIncompleteReasons(t *testing.T) {
	for _, response := range []string{`{}`, `{"status":"incomplete","incomplete_details":{"reason":"content_filter"}}`, `{"status":"completed","incomplete_details":{"reason":"max_output_tokens"}}`, `{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"error":{}}`} {
		provider := &Provider{id: "openai"}
		_, err := provider.consumeResponses(strings.NewReader(sse(`{"type":"response.incomplete","response":`+response+`}`)), func(session.AssistantChunk) error { return nil })
		var failure *llm.Error
		if !errors.As(err, &failure) || failure.Code != llm.ErrorInvalid {
			t.Fatalf("incomplete %s = %v", response, err)
		}
	}
}
