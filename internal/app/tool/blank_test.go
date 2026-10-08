package tool

import "testing"

func TestIsBlank_ECMAScriptWhiteSpaceAndLineTerminators(t *testing.T) {
	blank := "\t\v\f \u00a0\ufeff\n\r\u2028\u2029\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u202f\u205f\u3000"
	if !IsBlank("") || !IsBlank(blank) {
		t.Fatal("empty or combined whitespace rejected")
	}
	for _, char := range blank {
		if !IsBlank(string(char)) {
			t.Errorf("U+%04X is ECMAScript whitespace", char)
		}
	}
	for _, char := range []rune{'\u0085', '\u180e', '\u200b', '\u2060', 0, 'x', '\ufffd'} {
		if IsBlank(string(char)) || IsBlank(blank+string(char)+blank) {
			t.Errorf("U+%04X is not ECMAScript whitespace", char)
		}
	}
}
