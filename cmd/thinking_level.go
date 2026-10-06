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
	"strconv"
	"strings"
)

// thinkingLevelNames maps the --thinking-level shorthands to the fixed
// integer levels they stand for. Storage and the API stay integer; the names
// are input sugar only (ptone/scion#3016).
var thinkingLevelNames = map[string]int{
	"low":    25,
	"medium": 50,
	"high":   75,
	"max":    100,
}

// thinkingLevelFlagUsage is the --thinking-level help text.
const thinkingLevelFlagUsage = "Thinking level to inject into agent config: an integer 0-100, or low (25), medium (50), high (75), max (100)"

// parseThinkingLevel parses a --thinking-level value: an integer from 0 to
// 100, or a case-insensitive shorthand from thinkingLevelNames. Invalid
// values are usage errors.
func parseThinkingLevel(value string) (int, error) {
	v := strings.TrimSpace(value)
	if level, ok := thinkingLevelNames[strings.ToLower(v)]; ok {
		return level, nil
	}
	level, err := strconv.Atoi(v)
	if err != nil {
		return 0, newUsageError("invalid --thinking-level value %q: must be an integer from 0 to 100 or one of low, medium, high, max", value)
	}
	if level < 0 || level > 100 {
		return 0, newUsageError("invalid --thinking-level value %d: must be between 0 and 100 (or one of low, medium, high, max)", level)
	}
	return level, nil
}
