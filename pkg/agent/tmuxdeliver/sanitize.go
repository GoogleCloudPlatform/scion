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

package tmuxdeliver

import (
	"strings"
	"unicode/utf8"
)

// Sanitize returns text with every terminal control character removed, so a
// bracketed paste of it stays one paste: the text cannot end paste mode early
// (ESC[201~) and have what follows read as typed keys (design v2.22 §5.3).
//
// It works on code points, in one pass:
//
//   - C0 controls (U+0000-U+001F) are removed, except \n and \t. That
//     includes ESC (U+001B) and \r.
//   - DEL (U+007F) is removed.
//   - C1 controls (U+0080-U+009F) are removed, including U+009B, the
//     8-bit CSI some terminal parsers accept as ESC[.
//   - Invalid UTF-8 is replaced with U+FFFD before anything else, so a stray
//     byte in 0x80-0x9F is never passed on as a raw C1 byte.
//
// Every bracketed-paste marker starts with ESC or the 8-bit CSI, and both are
// removed, so no marker can survive or be re-formed by joining the text
// around a removed character (ESC[20 DEL 1~ becomes "[201~", which is
// inert).
//
// Unicode bidi and isolate controls (U+202A-U+202E, U+2066-U+2069) are kept:
// they are not terminal control sequences. A test pins that choice.
//
// Printable characters, including all multi-byte UTF-8, are unchanged.
func Sanitize(text string) string {
	if isClean(text) {
		return text
	}
	var b strings.Builder
	b.Grow(len(text))
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		i += size
		if r == utf8.RuneError && size <= 1 {
			b.WriteRune(utf8.RuneError)
			continue
		}
		if removed(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// removed reports whether Sanitize drops r.
func removed(r rune) bool {
	switch {
	case r == '\n' || r == '\t':
		return false
	case r < 0x20:
		return true
	case r == 0x7f:
		return true
	case r >= 0x80 && r <= 0x9f:
		return true
	}
	return false
}

// isClean reports whether text is valid UTF-8 with nothing Sanitize would
// remove, so the common case returns text without copying it.
func isClean(text string) bool {
	if !utf8.ValidString(text) {
		return false
	}
	for _, r := range text {
		if removed(r) {
			return false
		}
	}
	return true
}
