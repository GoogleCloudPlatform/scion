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
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// MatchTier constants match web/src/utils/palette-match.ts (0 best to 3 worst).
const (
	MatchTierExact       = 0
	MatchTierPrefix      = 1
	MatchTierSubstring   = 2
	MatchTierSubsequence = 3
)

// NormalizeMatchText NFC-normalizes and lowercases input, then recomposes NFC
// to close over uppercase letters with combining marks whose lowercase has a
// precomposed form (parity with web/src/utils/palette-match.ts).
func NormalizeMatchText(input string) string {
	return norm.NFC.String(strings.ToLower(norm.NFC.String(input)))
}

// NormalizeQuery trims whitespace, applies NormalizeMatchText, and collapses
// every internal run of whitespace to a single ASCII space.
func NormalizeQuery(input string) string {
	normed := NormalizeMatchText(strings.TrimSpace(input))
	if normed == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(normed))
	inSpace := false
	for _, r := range normed {
		if unicode.IsSpace(r) {
			if !inSpace {
				b.WriteByte(' ')
				inSpace = true
			}
			continue
		}
		inSpace = false
		b.WriteRune(r)
	}
	return b.String()
}

func matchRunesEqual(q, f rune) bool {
	return q == f || (q == ' ' && (f == '-' || f == '_'))
}

func matchesRunesAt(field []rune, at int, query []rune, qStart, qEnd int) bool {
	n := qEnd - qStart
	if at+n > len(field) {
		return false
	}
	for k := 0; k < n; k++ {
		if !matchRunesEqual(query[qStart+k], field[at+k]) {
			return false
		}
	}
	return true
}

func indexOfRuneRange(field, query []rune, qStart, qEnd int) int {
	n := qEnd - qStart
	last := len(field) - n
	for i := 0; i <= last; i++ {
		if matchesRunesAt(field, i, query, qStart, qEnd) {
			return i
		}
	}
	return -1
}

func isRuneSubsequence(needle, haystack []rune) bool {
	i := 0
	for j := 0; j < len(haystack) && i < len(needle); j++ {
		if matchRunesEqual(needle[i], haystack[j]) {
			i++
		}
	}
	return i == len(needle)
}

func containsEveryWord(query, field []rune) bool {
	start := 0
	for start < len(query) {
		end := start
		for end < len(query) && query[end] != ' ' {
			end++
		}
		if end > start && indexOfRuneRange(field, query, start, end) == -1 {
			return false
		}
		start = end + 1
	}
	return true
}

// ClassifyMatch classifies how well field matches query, mirroring
// web/src/utils/palette-match.ts classifyMatch. query must already be
// normalized via NormalizeQuery (or be empty); field is normalized here.
// Returns (tier, true) on match (0=exact, 1=prefix, 2=substring, 3=subsequence
// or all multi-word tokens present), or (-1, false) on no match.
func ClassifyMatch(query, field string) (int, bool) {
	if query == "" {
		return MatchTierExact, true
	}
	normalizedField := NormalizeMatchText(field)
	if !strings.ContainsRune(query, ' ') {
		if normalizedField == query {
			return MatchTierExact, true
		}
		if strings.HasPrefix(normalizedField, query) {
			return MatchTierPrefix, true
		}
		if strings.Contains(normalizedField, query) {
			return MatchTierSubstring, true
		}
		if isRuneSubsequence([]rune(query), []rune(normalizedField)) {
			return MatchTierSubsequence, true
		}
		return -1, false
	}

	qRunes := []rune(query)
	fRunes := []rune(normalizedField)
	idx := indexOfRuneRange(fRunes, qRunes, 0, len(qRunes))
	if idx == 0 {
		if len(fRunes) == len(qRunes) {
			return MatchTierExact, true
		}
		return MatchTierPrefix, true
	}
	if idx != -1 {
		return MatchTierSubstring, true
	}
	if isRuneSubsequence(qRunes, fRunes) {
		return MatchTierSubsequence, true
	}
	if containsEveryWord(qRunes, fRunes) {
		return MatchTierSubsequence, true
	}
	return -1, false
}
