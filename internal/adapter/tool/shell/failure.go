package shell

import "github.com/jinyule/nano-harness/internal/core/session"

type toolFailure struct {
	text  string
	info  session.ToolError
	cause error
}

func (failure *toolFailure) Error() string                { return failure.text }
func (failure *toolFailure) Unwrap() error                { return failure.cause }
func (failure *toolFailure) ToolError() session.ToolError { return failure.info }

func aborted(cause error) error {
	return &toolFailure{text: "tool call aborted", info: session.ToolError{Name: "AbortError", Code: "ABORTED"}, cause: cause}
}
