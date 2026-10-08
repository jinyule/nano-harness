package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/jinyule/nano-harness/internal/app/llm"
	"github.com/jinyule/nano-harness/internal/core/session"
)

func TestProvider_ArgumentLimitsKeepCallsRecoverable(t *testing.T) {
	provider := &Provider{id: "test"}
	for _, size := range []int{131_111, session.MaxArgumentsBytes, session.MaxArgumentsBytes + 1} {
		prefix, suffix := `{"value":"`, `"}`
		arguments := prefix + strings.Repeat("x", size-len(prefix)-len(suffix)) + suffix
		for _, transport := range []string{"responses-deltas", "responses-done", "anthropic-deltas", "anthropic-input", "chat-deltas"} {
			t.Run(fmt.Sprintf("%s/%d", transport, size), func(t *testing.T) {
				var consume func(io.Reader, llm.Emit) (llm.Completion, error)
				var stream string
				split := size / 2
				completedArguments := arguments
				if size > session.MaxArgumentsBytes {
					completedArguments = "{}"
				}
				switch transport {
				case "responses-deltas":
					consume = provider.consumeResponses
					stream = sse(
						`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"c","name":"t"}}`,
						fmt.Sprintf(`{"type":"response.function_call_arguments.delta","output_index":0,"delta":%q}`, arguments[:split]),
						fmt.Sprintf(`{"type":"response.function_call_arguments.delta","output_index":0,"delta":%q}`, arguments[split:]),
						fmt.Sprintf(`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","call_id":"c","name":"t","arguments":%q}}`, completedArguments),
						`{"type":"response.completed"}`,
					)
				case "responses-done":
					consume = provider.consumeResponses
					stream = sse(fmt.Sprintf(`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","call_id":"c","name":"t","arguments":%q}}`, arguments), `{"type":"response.completed"}`)
				case "anthropic-deltas":
					consume = provider.consumeAnthropic
					stream = sse(
						`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"c","name":"t","input":{}}}`,
						fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":%q}}`, arguments[:split]),
						fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":%q}}`, arguments[split:]),
						`{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`, `{"type":"message_stop"}`,
					)
				case "anthropic-input":
					consume = provider.consumeAnthropic
					stream = sse(fmt.Sprintf(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"c","name":"t","input":%s}}`, arguments), `{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`, `{"type":"message_stop"}`)
				case "chat-deltas":
					consume = provider.consumeChat
					stream = sse(
						fmt.Sprintf(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c","function":{"name":"t","arguments":%q}}]}}]}`, arguments[:split]),
						fmt.Sprintf(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":%q}}]},"finish_reason":"tool_calls"}]}`, arguments[split:]),
					)
				}
				// A valid companion call must survive the first call's overflow.
				switch transport {
				case "responses-deltas", "responses-done":
					stream = strings.Replace(stream, "data: {\"type\":\"response.completed\"}", sse(`{"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","call_id":"next","name":"t","arguments":"{}"}}`)+"data: {\"type\":\"response.completed\"}", 1)
				case "anthropic-deltas", "anthropic-input":
					stream = strings.Replace(stream, "data: {\"type\":\"message_stop\"}", sse(`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"next","name":"t","input":{}}}`)+"data: {\"type\":\"message_stop\"}", 1)
				case "chat-deltas":
					stream += sse(`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"next","function":{"name":"t","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
				}
				completion, err := consume(strings.NewReader(stream), func(chunk session.AssistantChunk) error {
					return (session.Record{Type: session.RecordAssistantChunk, Turn: 1, Step: 1, Chunk: &chunk}).Validate()
				})
				if err != nil || len(completion.Calls) != 2 {
					t.Fatalf("call ended as provider failure: calls=%d, err=%v", len(completion.Calls), err)
				}
				call := completion.Calls[0]
				if next := completion.Calls[1]; next.ID != "next" || next.ArgumentsOmitted || string(next.Arguments) != "{}" {
					t.Fatal("overflow affected the valid companion call")
				}
				if call.ID != "c" || call.Name != "t" {
					t.Fatalf("call identity lost: %s/%s", call.ID, call.Name)
				}
				if size <= session.MaxArgumentsBytes {
					if string(call.Arguments) != arguments {
						t.Fatal("valid arguments changed")
					}
				} else {
					encoded, _ := json.Marshal(call)
					if string(call.Arguments) != "{}" || !strings.Contains(string(encoded), `"arguments_omitted":true`) {
						t.Fatalf("oversized call was not explicitly omitted: arguments=%d bytes", len(call.Arguments))
					}
				}
			})
		}
	}
}
