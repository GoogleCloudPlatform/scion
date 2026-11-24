// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package filterquery

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const asciiWhitespace = " \t\r\n"

func trimASCIISpace(s string) string {
	return strings.Trim(s, asciiWhitespace)
}

// Parse parses input according to the unified filter query dialect.
// known reports whether a lowercased key name is recognised by the target view.
// If no recognised key term appears in input, Parse returns pure-text mode with
// the trimmed input verbatim (no quote or '-' processing).
func Parse(input string, known func(key string) bool) (Query, *Error) {
	if len(input) > MaxInputBytes {
		offset := MaxInputBytes
		for offset > 0 && !utf8.RuneStart(input[offset]) {
			offset--
		}
		return Query{}, &Error{
			Code:    ErrCodeLimit,
			Message: fmt.Sprintf("filter query exceeds maximum length of %d bytes", MaxInputBytes),
			Offset:  offset,
			Length:  len(input) - offset,
		}
	}
	if !utf8.ValidString(input) {
		return Query{}, &Error{
			Code:    ErrCodeSyntax,
			Message: "filter query is not valid UTF-8",
			Offset:  0,
			Length:  len(input),
		}
	}

	trimmed := trimASCIISpace(input)
	if trimmed == "" {
		return Query{Mode: ModeText, Text: ""}, nil
	}

	if !hasKnownKeyTerm(input, known) {
		if utf8.RuneCountInString(trimmed) > MaxTextRunes {
			return Query{}, &Error{
				Code:    ErrCodeLimit,
				Message: fmt.Sprintf("search text exceeds maximum length of %d characters", MaxTextRunes),
				Offset:  0,
				Length:  len(input),
			}
		}
		return Query{Mode: ModeText, Text: trimmed}, nil
	}

	var (
		terms         []Term
		posTextTokens []string
		excludeText   []string
	)

	n := len(input)
	i := 0
	for i < n {
		for i < n && isSpace(input[i]) {
			i++
		}
		if i >= n {
			break
		}

		termStart := i
		negate := false
		if input[i] == '-' {
			negate = true
			i++
			if i >= n || isSpace(input[i]) {
				return Query{}, &Error{
					Code:    ErrCodeSyntax,
					Message: "expected term after '-'",
					Offset:  termStart,
					Length:  1,
				}
			}
		}

		if input[i] == '(' || input[i] == ')' {
			return Query{}, &Error{
				Code:    ErrCodeSyntax,
				Message: "parentheses are not supported",
				Offset:  i,
				Length:  1,
			}
		}

		// Quoted text term.
		if input[i] == '"' {
			val, next, fqErr := parseQuoted(input, i)
			if fqErr != nil {
				return Query{}, fqErr
			}
			i = next
			if i < n && !isSpace(input[i]) {
				return Query{}, &Error{
					Code:    ErrCodeSyntax,
					Message: "unexpected character after closing quote",
					Offset:  i,
					Length:  1,
				}
			}
			if negate {
				excludeText = append(excludeText, val)
			} else {
				posTextTokens = append(posTextTokens, val)
			}
			continue
		}

		// Check for known key:valuelist term.
		if key, colonPos, ok := scanKeyPrefix(input, i); ok && known != nil && known(strings.ToLower(key)) {
			lowerKey := strings.ToLower(key)
			i = colonPos + 1
			if i >= n || isSpace(input[i]) {
				return Query{}, &Error{
					Code:    ErrCodeSyntax,
					Message: fmt.Sprintf("expected value after %q", key+":"),
					Offset:  termStart,
					Length:  i - termStart,
				}
			}

			var values []string
			for {
				valStart := i
				if i >= n || isSpace(input[i]) {
					return Query{}, &Error{
						Code:    ErrCodeSyntax,
						Message: "expected value after ','",
						Offset:  valStart - 1,
						Length:  1,
					}
				}
				if input[i] == ',' {
					return Query{}, &Error{
						Code:    ErrCodeSyntax,
						Message: "empty value in comma-separated list",
						Offset:  i,
						Length:  1,
					}
				}
				if input[i] == '(' || input[i] == ')' {
					return Query{}, &Error{
						Code:    ErrCodeSyntax,
						Message: "parentheses are not supported",
						Offset:  i,
						Length:  1,
					}
				}

				if input[i] == '"' {
					val, next, fqErr := parseQuoted(input, i)
					if fqErr != nil {
						return Query{}, fqErr
					}
					i = next
					if i < n && !isSpace(input[i]) && input[i] != ',' {
						return Query{}, &Error{
							Code:    ErrCodeSyntax,
							Message: "unexpected character after closing quote",
							Offset:  i,
							Length:  1,
						}
					}
					values = append(values, val)
				} else {
					if opLen := comparisonPrefixLen(input[i:]); opLen > 0 {
						return Query{}, &Error{
							Code:    ErrCodeSyntax,
							Message: "comparison operators are not supported yet",
							Offset:  valStart,
							Length:  opLen,
						}
					}
					for i < n && !isSpace(input[i]) && input[i] != ',' && input[i] != '"' && input[i] != '(' && input[i] != ')' {
						i++
					}
					if i < n && (input[i] == '(' || input[i] == ')') {
						return Query{}, &Error{
							Code:    ErrCodeSyntax,
							Message: "parentheses are not supported",
							Offset:  i,
							Length:  1,
						}
					}
					if i < n && input[i] == '"' {
						return Query{}, &Error{
							Code:    ErrCodeSyntax,
							Message: "unexpected quote in unquoted value",
							Offset:  i,
							Length:  1,
						}
					}
					values = append(values, input[valStart:i])
				}

				if len(values) > MaxValuesPerKey {
					return Query{}, &Error{
						Code:    ErrCodeLimit,
						Message: fmt.Sprintf("key %q exceeds maximum of %d values", lowerKey, MaxValuesPerKey),
						Offset:  termStart,
						Length:  i - termStart,
					}
				}

				if i < n && input[i] == ',' {
					i++
					continue
				}
				break
			}

			terms = append(terms, Term{
				Key:    lowerKey,
				Values: values,
				Negate: negate,
				Offset: termStart,
				Length: i - termStart,
			})
			continue
		}

		// Bareword text term (including unknown key:value tokens).
		wordStart := i
		for i < n && !isSpace(input[i]) {
			if input[i] == '(' || input[i] == ')' {
				return Query{}, &Error{
					Code:    ErrCodeSyntax,
					Message: "parentheses are not supported",
					Offset:  i,
					Length:  1,
				}
			}
			i++
		}
		word := input[wordStart:i]
		if !negate && (word == "OR" || word == "AND") {
			return Query{}, &Error{
				Code:    ErrCodeSyntax,
				Message: "use key:a,b for alternatives; boolean operators OR and AND are not supported",
				Offset:  termStart,
				Length:  len(word),
			}
		}
		if negate {
			excludeText = append(excludeText, word)
		} else {
			posTextTokens = append(posTextTokens, word)
		}
	}

	if len(terms)+len(excludeText) > MaxTerms {
		return Query{}, &Error{
			Code:    ErrCodeLimit,
			Message: fmt.Sprintf("filter query exceeds maximum of %d terms", MaxTerms),
			Offset:  0,
			Length:  len(input),
		}
	}

	text := strings.Join(posTextTokens, " ")
	if utf8.RuneCountInString(text) > MaxTextRunes {
		return Query{}, &Error{
			Code:    ErrCodeLimit,
			Message: fmt.Sprintf("search text exceeds maximum length of %d characters", MaxTextRunes),
			Offset:  0,
			Length:  len(input),
		}
	}
	for _, ex := range excludeText {
		if utf8.RuneCountInString(ex) > MaxTextRunes {
			return Query{}, &Error{
				Code:    ErrCodeLimit,
				Message: fmt.Sprintf("excluded text exceeds maximum length of %d characters", MaxTextRunes),
				Offset:  0,
				Length:  len(input),
			}
		}
	}

	return Query{
		Mode:        ModeStructured,
		Text:        text,
		Terms:       terms,
		ExcludeText: excludeText,
	}, nil
}

// hasKnownKeyTerm scans input for any unquoted [ "-" ] key ":" where known(lower(key)) is true.
func hasKnownKeyTerm(input string, known func(key string) bool) bool {
	if known == nil {
		return false
	}
	n := len(input)
	i := 0
	for i < n {
		for i < n && isSpace(input[i]) {
			i++
		}
		if i >= n {
			break
		}
		start := i
		if input[start] == '-' {
			start++
		}
		if start < n && input[start] == '"' {
			// Skip quoted string (optionally preceded by '-', not a key term).
			i = start + 1
			for i < n {
				if input[i] == '\\' && i+1 < n {
					i += 2
					continue
				}
				if input[i] == '"' {
					i++
					break
				}
				i++
			}
			continue
		}
		if key, _, ok := scanKeyPrefix(input, start); ok && known(strings.ToLower(key)) {
			return true
		}
		for i < n && !isSpace(input[i]) {
			i++
		}
	}
	return false
}

// scanKeyPrefix checks whether input[pos:] starts with ASCII letter { letter | digit | "_" } ":".
func scanKeyPrefix(input string, pos int) (key string, colonPos int, ok bool) {
	n := len(input)
	if pos >= n || !isASCIILetter(input[pos]) {
		return "", 0, false
	}
	j := pos + 1
	for j < n && isKeyChar(input[j]) {
		j++
	}
	if j < n && input[j] == ':' {
		return input[pos:j], j, true
	}
	return "", 0, false
}

// parseQuoted parses a double-quoted string starting at pos (where input[pos] == '"').
// Only \" and \\ are valid escapes.
func parseQuoted(input string, pos int) (string, int, *Error) {
	var b strings.Builder
	n := len(input)
	i := pos + 1
	for i < n {
		ch := input[i]
		if ch == '"' {
			return b.String(), i + 1, nil
		}
		if ch == '\\' {
			if i+1 >= n {
				return "", 0, &Error{
					Code:    ErrCodeSyntax,
					Message: "unterminated quote",
					Offset:  pos,
					Length:  n - pos,
				}
			}
			next := input[i+1]
			if next != '"' && next != '\\' {
				return "", 0, &Error{
					Code:    ErrCodeSyntax,
					Message: fmt.Sprintf("invalid escape sequence '\\%c'; only \\\" and \\\\ are allowed", next),
					Offset:  i,
					Length:  2,
				}
			}
			b.WriteByte(next)
			i += 2
			continue
		}
		b.WriteByte(ch)
		i++
	}
	return "", 0, &Error{
		Code:    ErrCodeSyntax,
		Message: "unterminated quote",
		Offset:  pos,
		Length:  n - pos,
	}
}

func comparisonPrefixLen(s string) int {
	if strings.HasPrefix(s, ">=") || strings.HasPrefix(s, "<=") {
		return 2
	}
	if strings.HasPrefix(s, ">") || strings.HasPrefix(s, "<") {
		return 1
	}
	return 0
}

func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

func isASCIILetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func isKeyChar(b byte) bool {
	return isASCIILetter(b) || (b >= '0' && b <= '9') || b == '_'
}
