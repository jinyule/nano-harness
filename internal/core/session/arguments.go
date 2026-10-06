package session

import "encoding/json"

// LimitArguments normalizes arguments to their durable JSON representation
// and replaces oversized arguments with an explicit omission.
// An omitted call retains its identity for a recoverable error result and
// cannot regain executable arguments from a later provider completion.
func (call ToolCall) LimitArguments() ToolCall {
	if !call.ArgumentsOmitted && len(call.Arguments) <= MaxArgumentsBytes {
		// Match the durable JSON representation, including HTML escaping.
		// Malformed input remains intact for the strict call/schema validator.
		if encoded, err := json.Marshal(call.Arguments); err == nil {
			call.Arguments = encoded
		}
	}
	if call.ArgumentsOmitted || len(call.Arguments) > MaxArgumentsBytes {
		call.Arguments = json.RawMessage(`{}`)
		call.ArgumentsOmitted = true
	}
	return call
}

// AppendArguments retains a streamed delta within the call's argument budget.
// It returns false after overflow; callers must stop emitting argument chunks
// while continuing to consume the bounded provider stream.
func (call *ToolCall) AppendArguments(delta string) bool {
	if call.ArgumentsOmitted || len(call.Arguments)+len(delta) > MaxArgumentsBytes {
		call.ArgumentsOmitted = true
		*call = call.LimitArguments()
		return false
	}
	call.Arguments = append(call.Arguments, delta...)
	return true
}
