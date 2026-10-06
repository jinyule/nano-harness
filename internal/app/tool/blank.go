package tool

import "github.com/jinyule/nano-harness/internal/core/text"

// IsBlank reports whether value is empty after ECMAScript trim: WhiteSpace
// (including U+FEFF) and LineTerminator, excluding U+0085. It preserves the
// caller's original text and does not use Go's broader Unicode whitespace set.
func IsBlank(value string) bool {
	for _, char := range value {
		if !text.IsSpace(char) {
			return false
		}
	}
	return true
}
