package tool

// IsBlank reports whether text is empty after ECMAScript trim: WhiteSpace
// (including U+FEFF) and LineTerminator, excluding U+0085. It preserves the
// caller's original text and does not use Go's broader Unicode whitespace set.
func IsBlank(text string) bool {
	for _, char := range text {
		switch char {
		case '\t', '\v', '\f', ' ', '\u00a0', '\ufeff', '\n', '\r', '\u2028', '\u2029',
			'\u1680', '\u2000', '\u2001', '\u2002', '\u2003', '\u2004', '\u2005', '\u2006',
			'\u2007', '\u2008', '\u2009', '\u200a', '\u202f', '\u205f', '\u3000':
			continue
		default:
			return false
		}
	}
	return true
}
