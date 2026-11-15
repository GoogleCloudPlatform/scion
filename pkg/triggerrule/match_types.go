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

package triggerrule

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Match-types bounds (§4.2, §7.3).
const (
	MinMatchTypes     = 1
	MaxMatchTypes     = 16
	MaxMatchTypeBytes = 128
)

var matchTypePatternRE = regexp.MustCompile(`^[a-z0-9_]+(\.[a-z0-9_]+)*(\.\*)?$`)

// ValidateMatchTypes validates a trigger's match_types array against §4.2:
// 1–16 entries, each <= 128 bytes and matching ^[a-z0-9_]+(\.[a-z0-9_]+)*(\.\*)?$.
func ValidateMatchTypes(patterns []string) error {
	if len(patterns) < MinMatchTypes || len(patterns) > MaxMatchTypes {
		return fmt.Errorf("match_types must contain %d..%d entries (got %d)", MinMatchTypes, MaxMatchTypes, len(patterns))
	}
	for i, p := range patterns {
		if err := ValidateMatchTypePattern(p); err != nil {
			return fmt.Errorf("match_types[%d]: %w", i, err)
		}
	}
	return nil
}

// ValidateMatchTypePattern checks a single match_types pattern entry.
func ValidateMatchTypePattern(pattern string) error {
	if pattern == "" {
		return errors.New("pattern must not be empty")
	}
	if len(pattern) > MaxMatchTypeBytes {
		return fmt.Errorf("pattern length %d exceeds %d bytes", len(pattern), MaxMatchTypeBytes)
	}
	if !matchTypePatternRE.MatchString(pattern) {
		return fmt.Errorf("invalid event type pattern %q", pattern)
	}
	return nil
}

// MatchTypes reports whether eventType matches at least one pattern in patterns
// using exact match or trailing ".*" dot-bounded prefix match (§7.3).
func MatchTypes(patterns []string, eventType string) bool {
	if eventType == "" {
		return false
	}
	for _, p := range patterns {
		if matchSingleType(p, eventType) {
			return true
		}
	}
	return false
}

func matchSingleType(pattern, eventType string) bool {
	if prefix, isWildcard := strings.CutSuffix(pattern, ".*"); isWildcard {
		dotPrefix := prefix + "."
		return strings.HasPrefix(eventType, dotPrefix) && len(eventType) > len(dotPrefix)
	}
	return pattern == eventType
}
