package text

import "testing"

func TestTrimSpace_ECMAScriptWhiteSpaceAndLineTerminators(t *testing.T) {
	blank := "\t\v\f \u00a0\ufeff\n\r\u2028\u2029\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u202f\u205f\u3000"
	if TrimSpace(blank) != "" || TrimSpace("") != "" {
		t.Fatal("empty or combined whitespace preserved")
	}
	for _, char := range blank {
		if !IsSpace(char) || TrimSpace(string(char)+"x"+string(char)) != "x" {
			t.Errorf("U+%04X is ECMAScript whitespace", char)
		}
	}
	for _, char := range []rune{'\u0085', '\u180e', '\u200b', '\u2060', 0, 'x', '\ufffd'} {
		value := string(char)
		if IsSpace(char) || TrimSpace(blank+value+blank) != value {
			t.Errorf("U+%04X is not ECMAScript whitespace", char)
		}
	}
}

func TestQuote_MatchesJSONStringify(t *testing.T) {
	for value, want := range map[string]string{
		"":                    `""`,
		"plain <&> 中文😀":       `"plain <&> 中文😀"`,
		"q\"b\\":              `"q\"b\\"`,
		"\b\f\n\r\t":          `"\b\f\n\r\t"`,
		"\x00\x01\x1f\x7f":    `"\u0000\u0001\u001f` + "\x7f" + `"`,
		"line\u2028sep\u2029": "\"line\u2028sep\u2029\"",
		"bad\xffbyte":         "\"bad\ufffdbyte\"",
	} {
		if got := Quote(value); got != want {
			t.Errorf("Quote(%q) = %q, want %q", value, got, want)
		}
	}
}
