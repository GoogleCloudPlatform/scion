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

// Validate checks q against the schema for server-side execution:
//   - enforces term, value, and text limits
//   - rejects unknown or client-only keys with ErrCodeUnsupportedKey
//   - validates enum/flag-set values (case-insensitive) with closest-match hints
//   - enforces mutually exclusive flag-set pairs
//   - rejects empty label keys (e.g. label:=v)
func (s Schema) Validate(q Query) *Error {
	if utf8.RuneCountInString(q.Text) > MaxTextRunes {
		return &Error{
			Code:    ErrCodeLimit,
			Message: fmt.Sprintf("search text exceeds maximum length of %d characters", MaxTextRunes),
			Offset:  0,
			Length:  len(q.Text),
		}
	}
	for _, ex := range q.ExcludeText {
		if utf8.RuneCountInString(ex) > MaxTextRunes {
			return &Error{
				Code:    ErrCodeLimit,
				Message: fmt.Sprintf("excluded text exceeds maximum length of %d characters", MaxTextRunes),
				Offset:  0,
				Length:  len(ex),
			}
		}
	}
	if len(q.Terms)+len(q.ExcludeText) > MaxTerms {
		return &Error{
			Code:    ErrCodeLimit,
			Message: fmt.Sprintf("filter query exceeds maximum of %d terms", MaxTerms),
			Offset:  0,
			Length:  0,
		}
	}

	// Track positive flag-set values per key for mutual-exclusion checks.
	posFlags := make(map[string]map[string]Term)

	for _, term := range q.Terms {
		keyDef, ok := s.LookupKey(term.Key)
		if !ok {
			return &Error{
				Code:    ErrCodeUnsupportedKey,
				Message: fmt.Sprintf("key %q is not supported by this endpoint", term.Key),
				Offset:  term.Offset,
				Length:  term.Length,
			}
		}
		if keyDef.Exec == ExecClient {
			return &Error{
				Code:    ErrCodeUnsupportedKey,
				Message: fmt.Sprintf("key %q is not supported by this endpoint", keyDef.Name),
				Offset:  term.Offset,
				Length:  term.Length,
			}
		}
		if len(term.Values) == 0 {
			return &Error{
				Code:    ErrCodeSyntax,
				Message: fmt.Sprintf("expected value after %q", term.Key+":"),
				Offset:  term.Offset,
				Length:  term.Length,
			}
		}
		if len(term.Values) > MaxValuesPerKey {
			return &Error{
				Code:    ErrCodeLimit,
				Message: fmt.Sprintf("key %q exceeds maximum of %d values", keyDef.Name, MaxValuesPerKey),
				Offset:  term.Offset,
				Length:  term.Length,
			}
		}

		switch keyDef.Kind {
		case KindEnum, KindFlagSet:
			for _, v := range term.Values {
				lower := strings.ToLower(v)
				if !containsString(keyDef.Enum, lower) {
					msg := fmt.Sprintf("invalid value %q for %s", v, keyDef.Name)
					if suggestion := closestMatch(lower, keyDef.Enum); suggestion != "" {
						msg = fmt.Sprintf("%s (did you mean %q?)", msg, suggestion)
					} else if len(keyDef.Enum) > 0 {
						msg = fmt.Sprintf("%s (expected one of: %s)", msg, strings.Join(keyDef.Enum, ", "))
					}
					return &Error{
						Code:    ErrCodeValue,
						Message: msg,
						Offset:  term.Offset,
						Length:  term.Length,
					}
				}
				if keyDef.Kind == KindFlagSet && !term.Negate {
					if posFlags[keyDef.Name] == nil {
						posFlags[keyDef.Name] = make(map[string]Term)
					}
					posFlags[keyDef.Name][lower] = term
				}
			}
		case KindLabel:
			for _, v := range term.Values {
				if v == "" || strings.HasPrefix(v, "=") {
					return &Error{
						Code:    ErrCodeValue,
						Message: fmt.Sprintf("invalid label filter %q: label key must not be empty", v),
						Offset:  term.Offset,
						Length:  term.Length,
					}
				}
			}
		}
	}

	// Check mutually exclusive flag pairs.
	for _, keyDef := range s.Keys {
		if len(keyDef.Exclusive) == 0 {
			continue
		}
		seen := posFlags[keyDef.Name]
		if len(seen) < 2 {
			continue
		}
		for _, group := range keyDef.Exclusive {
			var matched []string
			var lastTerm Term
			for _, flag := range group {
				if t, ok := seen[strings.ToLower(flag)]; ok {
					matched = append(matched, flag)
					lastTerm = t
				}
			}
			if len(matched) >= 2 {
				return &Error{
					Code:    ErrCodeValue,
					Message: fmt.Sprintf("%s:%s and %s:%s are mutually exclusive", keyDef.Name, matched[0], keyDef.Name, matched[1]),
					Offset:  lastTerm.Offset,
					Length:  lastTerm.Length,
				}
			}
		}
	}

	return nil
}

func containsString(list []string, target string) bool {
	for _, s := range list {
		if strings.ToLower(s) == target {
			return true
		}
	}
	return false
}

// closestMatch returns the closest candidate within edit distance <= 2 (and
// at most half the candidate length + 1), or "" if none is close enough.
func closestMatch(input string, candidates []string) string {
	best := ""
	bestDist := 3
	for _, c := range candidates {
		d := levenshtein(input, strings.ToLower(c))
		if d < bestDist {
			bestDist = d
			best = c
		}
	}
	return best
}

func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	la, lb := len(ra), len(rb)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	prev := make([]int, lb+1)
	curr := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		curr[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			curr[j] = min(
				prev[j]+1,
				curr[j-1]+1,
				prev[j-1]+cost,
			)
		}
		prev, curr = curr, prev
	}
	return prev[lb]
}
