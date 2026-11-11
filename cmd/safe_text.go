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

package cmd

import (
	"strings"
	"unicode"
)

// safeText returns s for one cell or line of terminal output: every
// control character (C0 and C1, DEL, newline and tab included), the line
// and paragraph separators U+2028 and U+2029, and every format character
// (Unicode category Cf, which holds the bidirectional embedding, override,
// isolate and mark characters) except the zero-width joiner and
// non-joiner are replaced with U+FFFD.
// Text written by other principals, such as an artifact title or a file
// path in a bundle, then cannot carry terminal escape sequences, break a
// table row, or reorder what is shown.
func safeText(s string) string {
	if strings.IndexFunc(s, unsafeTextRune) < 0 {
		return s
	}
	return strings.Map(func(r rune) rune {
		if unsafeTextRune(r) {
			return unicode.ReplacementChar
		}
		return r
	}, s)
}

// unsafeTextRune reports whether r is replaced by safeText.
func unsafeTextRune(r rune) bool {
	switch {
	case unicode.IsControl(r):
		return true
	case r == '\u2028' || r == '\u2029':
		return true
	case r == '\u200c' || r == '\u200d':
		// Zero-width non-joiner and joiner: emoji sequences and several
		// scripts need them, and they cannot reorder text.
		return false
	case unicode.Is(unicode.Cf, r):
		// Format characters: the bidirectional embeddings, overrides,
		// isolates and marks (U+202A to U+202E, U+2066 to U+2069,
		// U+200E, U+200F, U+061C), zero-width spaces and U+FEFF.
		return true
	}
	return false
}
