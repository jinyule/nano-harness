package search

import "github.com/jinyule/nano-harness/internal/core/session"

// searchFailure keeps the existing diagnostic separate from its cause and code.
type searchFailure struct {
	text  string
	code  string
	cause error
}

func (failure *searchFailure) Error() string { return failure.text }
func (failure *searchFailure) Unwrap() error { return failure.cause }
func (failure *searchFailure) ToolError() session.ToolError {
	return session.ToolError{Name: "SearchError", Code: failure.code}
}

func searchError(code string, cause error) error {
	return &searchFailure{text: cause.Error(), code: code, cause: cause}
}
