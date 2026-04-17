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

package workflow

import (
	"fmt"
	"regexp"
	"strings"
)

// validKeyRe matches conservative identifier keys: [a-zA-Z_][a-zA-Z0-9_]*
var validKeyRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// ValidateInputFlag rejects strings that are not in key=value form.
// It does not parse the value — quack owns value semantics.
//
// Rules:
//   - Must contain at least one '='.
//   - Key (before the first '=') must be non-empty and match [a-zA-Z_][a-zA-Z0-9_]*.
//   - Value (after the first '=') may be anything, including empty or further '='.
func ValidateInputFlag(s string) error {
	if s == "" {
		return fmt.Errorf("input flag must not be empty")
	}
	idx := strings.IndexByte(s, '=')
	if idx == -1 {
		return fmt.Errorf("input flag %q is missing '=': expected key=value", s)
	}
	key := s[:idx]
	if key == "" {
		return fmt.Errorf("input flag %q has an empty key: expected key=value", s)
	}
	if !validKeyRe.MatchString(key) {
		return fmt.Errorf("input flag key %q must match [a-zA-Z_][a-zA-Z0-9_]*", key)
	}
	return nil
}

// ValidateInputFlags validates a slice of input flags, returning the first
// failure wrapped with its index.
func ValidateInputFlags(ss []string) error {
	for i, s := range ss {
		if err := ValidateInputFlag(s); err != nil {
			return fmt.Errorf("--input[%d]: %w", i, err)
		}
	}
	return nil
}
