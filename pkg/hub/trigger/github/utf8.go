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

package github

import (
	"strings"
	"unicode/utf8"
)

// truncateUTF8 returns s normalized to valid UTF-8 and truncated to at most
// maxBytes bytes without splitting a multi-byte UTF-8 rune.
func truncateUTF8(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "\uFFFD")
	}
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// truncateUTF8Tail returns the trailing suffix of s normalized to valid UTF-8
// and at most maxBytes bytes without splitting a multi-byte UTF-8 rune. When a
// line break exists before the final non-empty line in that suffix, it cuts
// cleanly after the first '\n' so diff hunks start on a line boundary while
// preserving the commented line at the end.
func truncateUTF8Tail(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "\uFFFD")
	}
	if len(s) <= maxBytes {
		return s
	}
	start := len(s) - maxBytes
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	tail := s[start:]
	if nl := strings.IndexByte(strings.TrimRight(tail, "\r\n"), '\n'); nl >= 0 {
		return tail[nl+1:]
	}
	return tail
}

// normalizeNewlines converts CRLF and lone CR line endings to LF.
func normalizeNewlines(s string) string {
	if !strings.ContainsRune(s, '\r') {
		return s
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}
