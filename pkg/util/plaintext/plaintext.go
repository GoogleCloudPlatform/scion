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

// Package plaintext turns text another principal wrote into short,
// single-line plain text for display fields such as a notification excerpt.
package plaintext

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxScanBytes bounds how much input Clean reads.
const MaxScanBytes = 16 << 10

// Clean returns s as single-line plain text of at most maxRunes runes.
// Invalid UTF-8 is dropped. Whitespace of any kind, the line and paragraph
// separators U+2028 and U+2029 included, becomes a single space between
// words. Every other control (Cc) and format (Cf) character, such as ESC,
// NUL, bidirectional overrides and isolates and zero-width characters, is
// removed. The result is trimmed. A longer result is cut on a rune
// boundary and ends with "…", which counts toward maxRunes.
func Clean(s string, maxRunes int) string {
	if maxRunes < 1 {
		return ""
	}
	if len(s) > MaxScanBytes {
		s = s[:MaxScanBytes]
	}
	out := make([]rune, 0, min(len(s), maxRunes+1))
	space := false
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		switch {
		case r == utf8.RuneError && size <= 1:
			// Invalid UTF-8.
		case isBreak(r):
			space = len(out) > 0
		case IsUnsafe(r):
			// Cc and Cf that are not whitespace.
		default:
			if space {
				out = append(out, ' ')
				space = false
			}
			out = append(out, r)
		}
		if len(out) > maxRunes {
			break
		}
	}
	if len(out) <= maxRunes {
		return string(out)
	}
	return strings.TrimRightFunc(string(out[:maxRunes-1]), unicode.IsSpace) + "…"
}

// isBreak reports whether r separates words: Unicode white space, and the
// line (Zl) and paragraph (Zp) separators. unicode.IsSpace already includes
// U+2028 and U+2029; the explicit Zl and Zp test keeps them breaks even if
// that ever changes.
func isBreak(r rune) bool {
	return unicode.IsSpace(r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r)
}

// IsUnsafe reports whether r can change how surrounding text is displayed
// rather than being displayed itself: control (Cc), format (Cf), line
// separator (Zl) and paragraph separator (Zp) characters. It is the same set
// as the hub's display-unsafe runes.
func IsUnsafe(r rune) bool {
	return unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r)
}
