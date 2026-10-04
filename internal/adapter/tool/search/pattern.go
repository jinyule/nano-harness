package search

import (
	"errors"
	"regexp"
	"strings"
)

// pattern is one gitignore-style glob, the syntax ripgrep uses for --glob
// overrides and ignore files: '*' and '?' stop at '/', '**' spans
// directories, '[...]' is a class, '{a,b}' is alternation, a leading '!'
// negates, a trailing '/' matches directories only, and a pattern without an
// inner '/' matches the basename at any depth.
type pattern struct {
	expression *regexp.Regexp
	negated    bool
	directory  bool
}

func compilePattern(text string) (pattern, error) {
	var compiled pattern
	if strings.HasPrefix(text, "!") {
		compiled.negated, text = true, text[1:]
	}
	if trimmed, ok := strings.CutSuffix(text, "/"); ok {
		compiled.directory, text = true, trimmed
	}
	anchored := strings.Contains(text, "/")
	text = strings.TrimPrefix(text, "/")
	if text == "" {
		return pattern{}, errors.New("empty glob")
	}
	body, err := translate(text, false)
	if err != nil {
		return pattern{}, err
	}
	prefix := "^"
	if !anchored {
		prefix = "^(?:.*/)?"
	}
	compiled.expression, err = regexp.Compile(prefix + body + "$")
	if err != nil {
		return pattern{}, err
	}
	return compiled, nil
}

// match reports whether a slash-separated relative path matches, ignoring
// negation; directory-only patterns never match files.
func (compiled pattern) match(relative string, isDir bool) bool {
	return (isDir || !compiled.directory) && compiled.expression.MatchString(relative)
}

// translate converts glob syntax to an RE2 fragment. Inside an alternation
// group a comma ends the branch and nesting is rejected.
func translate(text string, inGroup bool) (string, error) {
	var out strings.Builder
	for index := 0; index < len(text); index++ {
		char := text[index]
		switch char {
		case '\\':
			if index+1 == len(text) {
				return "", errors.New("dangling escape")
			}
			index++
			out.WriteString(regexp.QuoteMeta(text[index : index+1]))
		case '*':
			if index+1 < len(text) && text[index+1] == '*' {
				atStart := index == 0 || text[index-1] == '/'
				atEnd := index+2 == len(text)
				switch {
				case atStart && atEnd:
					out.WriteString(".*")
				case atStart && text[index+2] == '/':
					out.WriteString("(?:.*/)?")
					index++
				default:
					out.WriteString("[^/]*")
				}
				index++
				continue
			}
			out.WriteString("[^/]*")
		case '?':
			out.WriteString("[^/]")
		case '[':
			end, class, err := translateClass(text, index)
			if err != nil {
				return "", err
			}
			out.WriteString(class)
			index = end
		case '{':
			if inGroup {
				return "", errors.New("nested alternation is not supported")
			}
			end := strings.IndexByte(text[index:], '}')
			if end < 0 {
				return "", errors.New("unclosed alternation group")
			}
			branches := strings.Split(text[index+1:index+end], ",")
			out.WriteString("(?:")
			for position, branch := range branches {
				translated, err := translate(branch, true)
				if err != nil {
					return "", err
				}
				if position > 0 {
					out.WriteByte('|')
				}
				out.WriteString(translated)
			}
			out.WriteByte(')')
			index += end
		default:
			out.WriteString(regexp.QuoteMeta(text[index : index+1]))
		}
	}
	return out.String(), nil
}

// translateClass converts the bracket expression starting at start and
// returns the index of its closing bracket. Classes never match '/'.
func translateClass(text string, start int) (int, string, error) {
	index := start + 1
	negated := index < len(text) && (text[index] == '!' || text[index] == '^')
	if negated {
		index++
	}
	var class strings.Builder
	class.WriteByte('[')
	if negated {
		class.WriteByte('^')
	}
	first := true
	for ; index < len(text); index++ {
		char := text[index]
		if char == ']' && !first {
			if negated {
				class.WriteByte('/')
			}
			class.WriteByte(']')
			return index, class.String(), nil
		}
		first = false
		if char == '\\' || char == '[' || char == ']' || char == '^' {
			class.WriteByte('\\')
		}
		class.WriteByte(char)
	}
	return 0, "", errors.New("unclosed character class")
}
