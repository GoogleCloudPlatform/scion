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

package api

import (
	"fmt"
	"strconv"
)

// Count limits (ScionConfig.MaxTurns and MaxModelCalls) are optional typed
// values with three states (ptone/scion#5605):
//
//   - nil: not set. The agent inherits the limit from its template chain,
//     else from the hub and broker default limits.
//   - 0: no limit. It is an explicit value: it overrides an inherited
//     limit, and the default limits never fill it.
//   - n >= 1: a limit of n.
//
// On the wire and in YAML, an absent key or null is nil and 0 is "no
// limit". max_duration keeps its string form, which already has the same
// three states: "" (inherit), "0" (no limit) and a duration.

// IntPtr returns a pointer to a copy of n.
func IntPtr(n int) *int { return &n }

// CloneIntPtr returns a pointer to a copy of *p, or nil for nil, so a merged
// config never aliases a caller's value.
func CloneIntPtr(p *int) *int {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// IntPtrEqual reports whether a and b are both nil, or both non-nil with the
// same value.
func IntPtrEqual(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// CountLimit reports the effective count limit p sets: limited is true and n
// the limit when p is a positive value; nil (unset) and 0 (no limit) both
// give no limit. It is what the launch path enforces, after the default
// limits have filled a nil value.
func CountLimit(p *int) (n int, limited bool) {
	if p == nil || *p <= 0 {
		return 0, false
	}
	return *p, true
}

// IsUnlimitedCount reports whether p is an explicit "no limit" (0).
func IsUnlimitedCount(p *int) bool { return p != nil && *p == 0 }

// ValidateCountLimit rejects a negative count limit. nil, 0 (no limit) and
// positive values are valid.
func ValidateCountLimit(key string, p *int) error {
	if p != nil && *p < 0 {
		return fmt.Errorf("%s must be 0 (no limit) or a positive number, got %d", key, *p)
	}
	return nil
}

// TurnLimit returns the effective turn limit of c (see CountLimit).
func (c *ScionConfig) TurnLimit() (n int, limited bool) {
	if c == nil {
		return 0, false
	}
	return CountLimit(c.MaxTurns)
}

// ModelCallLimit returns the effective model-call limit of c (see
// CountLimit).
func (c *ScionConfig) ModelCallLimit() (n int, limited bool) {
	if c == nil {
		return 0, false
	}
	return CountLimit(c.MaxModelCalls)
}

// FormatCountLimit renders an optional count limit for messages and logs:
// "unset" for nil, "0" for no limit, else the number.
func FormatCountLimit(p *int) string {
	if p == nil {
		return "unset"
	}
	return strconv.Itoa(*p)
}
