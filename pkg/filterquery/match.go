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

import "strings"

// Match evaluates q against row in memory.
//
//   - Pure-text mode: case-insensitive substring match against any string in row.Text().
//   - Structured mode: all terms must match (AND); comma-separated values within a term
//     match any value (OR); leading '-' negates the term. Label matching is exact-case;
//     other keys match case-insensitively. Positive Text must substring-match row.Text();
//     no ExcludeText may substring-match row.Text().
func Match(q Query, row Row) bool {
	if q.Mode == ModeText {
		if q.Text == "" {
			return true
		}
		return matchesAnyText(row.Text(), q.Text)
	}

	for _, term := range q.Terms {
		matched := termMatchesRow(term, row)
		if term.Negate {
			if matched {
				return false
			}
		} else {
			if !matched {
				return false
			}
		}
	}

	if q.Text != "" && !matchesAnyText(row.Text(), q.Text) {
		return false
	}

	for _, ex := range q.ExcludeText {
		if matchesAnyText(row.Text(), ex) {
			return false
		}
	}

	return true
}

func termMatchesRow(term Term, row Row) bool {
	key := strings.ToLower(term.Key)
	rowVals := row.Values(key)
	if len(rowVals) == 0 {
		return false
	}
	exactCase := key == "label"
	for _, want := range term.Values {
		if exactCase {
			for _, rv := range rowVals {
				if rv == want {
					return true
				}
			}
		} else {
			wantLower := strings.ToLower(want)
			for _, rv := range rowVals {
				if strings.ToLower(rv) == wantLower {
					return true
				}
			}
		}
	}
	return false
}

func matchesAnyText(haystacks []string, needle string) bool {
	needleLower := strings.ToLower(needle)
	for _, h := range haystacks {
		if strings.Contains(strings.ToLower(h), needleLower) {
			return true
		}
	}
	return false
}
