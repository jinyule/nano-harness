// Package text defines ECMAScript string operations shared by tool inputs,
// model-visible text, and durable session validation.
package text

import "strings"

// IsSpace reports membership in ECMAScript WhiteSpace and LineTerminator:
// U+FEFF is included and U+0085 is excluded.
func IsSpace(char rune) bool {
	switch char {
	case '\t', '\v', '\f', ' ', '\u00a0', '\ufeff', '\n', '\r', '\u2028', '\u2029',
		'\u1680', '\u2000', '\u2001', '\u2002', '\u2003', '\u2004', '\u2005', '\u2006',
		'\u2007', '\u2008', '\u2009', '\u200a', '\u202f', '\u205f', '\u3000':
		return true
	default:
		return false
	}
}

// TrimSpace removes leading and trailing ECMAScript whitespace.
func TrimSpace(value string) string { return strings.TrimFunc(value, IsSpace) }
