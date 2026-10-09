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
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
)

// CloneDepthFull is the clone_depth value that requests a full clone
// (no --depth flag).
const CloneDepthFull = "full"

// CloneDepth is the clone_depth setting on a profile or template: "full"
// for a full clone, or a positive integer N for a clone of depth N. The
// empty value means "not set", which keeps the default shallow clone.
//
// It decodes from either a JSON/YAML string ("full", "50") or a bare
// integer (50), so `clone_depth: 50` works in YAML and JSON alike.
type CloneDepth string

// UnmarshalJSON accepts a JSON string or a JSON integer.
func (d *CloneDepth) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if bytes.Equal(b, []byte("null")) {
		*d = ""
		return nil
	}
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*d = CloneDepth(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("clone_depth: want \"full\" or a positive integer, got %s", string(b))
	}
	*d = CloneDepth(n.String())
	return nil
}

// GitDepth converts the setting to a GitCloneConfig.Depth value. ok is
// false when the setting is empty (not set). "full" yields 0 (full clone);
// a positive integer N yields N. Any other value is an error: 0 and
// negative numbers are rejected so that "full" is the only way to ask for
// a full clone. The accepted forms match the schemas exactly: lowercase
// "full" or ^[1-9][0-9]*$ (no sign, spaces or leading zeros).
func (d CloneDepth) GitDepth() (depth int, ok bool, err error) {
	s := string(d)
	if s == "" {
		return 0, false, nil
	}
	if s == CloneDepthFull {
		return 0, true, nil
	}
	if !isPositiveDecimal(s) {
		return 0, false, fmt.Errorf("invalid clone_depth %q: want %q or a positive integer", s, CloneDepthFull)
	}
	n, convErr := strconv.Atoi(s)
	if convErr != nil {
		return 0, false, fmt.Errorf("invalid clone_depth %q: %w", s, convErr)
	}
	return n, true, nil
}

// isPositiveDecimal reports whether s matches ^[1-9][0-9]*$, the same
// form the settings and agent schemas accept.
func isPositiveDecimal(s string) bool {
	if s == "" || s[0] < '1' || s[0] > '9' {
		return false
	}
	for i := 1; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// Validate reports whether the setting is empty, "full" or a positive
// integer.
func (d CloneDepth) Validate() error {
	_, _, err := d.GitDepth()
	return err
}
