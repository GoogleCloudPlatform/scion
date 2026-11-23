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
	"slices"
	"sort"
	"strings"
)

// Canonical returns a deterministic string serialization of q.
// It folds key case, sorts terms by key/negation/values, and sorts and
// deduplicates each term's values. Cursor binding and URL normalization use it.
func Canonical(q Query) string {
	if q.Mode == ModeText {
		return strings.TrimSpace(q.Text)
	}

	type normTerm struct {
		key    string
		negate bool
		values []string
		joined string
	}

	terms := make([]normTerm, 0, len(q.Terms))
	for _, t := range q.Terms {
		key := strings.ToLower(t.Key)
		vals := make([]string, len(t.Values))
		for i, v := range t.Values {
			if key == "label" {
				vals[i] = v
			} else {
				vals[i] = strings.ToLower(v)
			}
		}
		sort.Strings(vals)
		vals = slices.Compact(vals)
		encoded := make([]string, len(vals))
		for i, v := range vals {
			encoded[i] = formatValue(v)
		}
		terms = append(terms, normTerm{
			key:    key,
			negate: t.Negate,
			values: vals,
			joined: strings.Join(encoded, ","),
		})
	}

	sort.Slice(terms, func(i, j int) bool {
		if terms[i].key != terms[j].key {
			return terms[i].key < terms[j].key
		}
		if terms[i].negate != terms[j].negate {
			return !terms[i].negate
		}
		return terms[i].joined < terms[j].joined
	})

	parts := make([]string, 0, len(terms)+1+len(q.ExcludeText))
	for _, t := range terms {
		prefix := ""
		if t.negate {
			prefix = "-"
		}
		parts = append(parts, prefix+t.key+":"+t.joined)
	}

	if q.Text != "" {
		parts = append(parts, formatText(q.Text))
	}

	if len(q.ExcludeText) > 0 {
		excl := append([]string(nil), q.ExcludeText...)
		sort.Strings(excl)
		excl = slices.Compact(excl)
		for _, ex := range excl {
			parts = append(parts, "-"+formatText(ex))
		}
	}

	return strings.Join(parts, " ")
}

func formatValue(v string) string {
	if needsQuotingValue(v) {
		return quoteString(v)
	}
	return v
}

func formatText(t string) string {
	if needsQuotingText(t) {
		return quoteString(t)
	}
	return t
}

func needsQuotingValue(v string) bool {
	if v == "" {
		return true
	}
	if comparisonPrefixLen(v) > 0 {
		return true
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if isSpace(c) || c == ',' || c == '"' || c == '\\' || c == '(' || c == ')' {
			return true
		}
	}
	return false
}

func needsQuotingText(t string) bool {
	if t == "" || t == "OR" || t == "AND" || strings.HasPrefix(t, "-") {
		return true
	}
	if _, _, ok := scanKeyPrefix(t, 0); ok {
		return true
	}
	for i := 0; i < len(t); i++ {
		c := t[i]
		if isSpace(c) || c == '"' || c == '\\' || c == '(' || c == ')' {
			return true
		}
	}
	return false
}

func quoteString(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '"' || c == '\\' {
			b.WriteByte('\\')
		}
		b.WriteByte(c)
	}
	b.WriteByte('"')
	return b.String()
}
