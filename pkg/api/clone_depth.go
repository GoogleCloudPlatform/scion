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
	"math"
	"strconv"

	"gopkg.in/yaml.v3"
)

// CloneDepthFull is the clone_depth value that requests a full clone
// (no --depth flag).
const CloneDepthFull = "full"

// MaxCloneDepth is the largest numeric clone_depth. The bound keeps every
// accepted value inside int range and matches the schemas, which allow at
// most 9 digits ("maximum": 999999999).
const MaxCloneDepth = 999999999

// CloneDepth is the clone_depth setting on a profile or template: "full"
// for a full clone, or an integer N from 1 to MaxCloneDepth for a clone of
// depth N. The empty value means "not set", which keeps the default
// shallow clone.
//
// It decodes from either a string ("full", "50") or a bare number (50), so
// `clone_depth: 50` works in YAML and JSON alike. Every decoder (JSON,
// YAML and the settings loader) turns the raw value into a Go value the
// way yaml.v3 does, which is how the schema validator reads both JSON and
// YAML, and then applies CloneDepthFromValue. So the schemas and GitDepth
// see the same value: 5.0, 1e2 and YAML 0x10 are "5", "100" and "16", and
// a string (even a tagged one such as !!binary) is kept as decoded.
type CloneDepth string

// UnmarshalJSON accepts a JSON string or a JSON number.
func (d *CloneDepth) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	var v interface{}
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		v = s
	} else if err := yaml.Unmarshal(b, &v); err != nil {
		return fmt.Errorf("clone_depth: want \"full\" or a positive integer, got %s", string(b))
	}
	cd, err := CloneDepthFromValue(v)
	if err != nil {
		return err
	}
	*d = cd
	return nil
}

// UnmarshalYAML accepts a YAML string or a YAML number.
func (d *CloneDepth) UnmarshalYAML(node *yaml.Node) error {
	var v interface{}
	if err := node.Decode(&v); err != nil {
		return err
	}
	cd, err := CloneDepthFromValue(v)
	if err != nil {
		return err
	}
	*d = cd
	return nil
}

// CloneDepthFromValue converts a clone_depth value decoded by yaml.v3 (or
// by a loader that produces the same Go types) to a CloneDepth. nil is
// unset; a string is kept as is; a whole-valued number becomes decimal
// text. Any other scalar (a fraction, NaN, an infinity, a bool) becomes
// its text form, which GitDepth rejects. A map or list is an error.
func CloneDepthFromValue(v interface{}) (CloneDepth, error) {
	switch n := v.(type) {
	case nil:
		return "", nil
	case string:
		return CloneDepth(n), nil
	case int:
		return CloneDepth(strconv.Itoa(n)), nil
	case int64:
		return CloneDepth(strconv.FormatInt(n, 10)), nil
	case uint64:
		return CloneDepth(strconv.FormatUint(n, 10)), nil
	case float64:
		// Whole values within int64 range print exactly; anything
		// larger is far above MaxCloneDepth and keeps its %v text.
		if n == math.Trunc(n) && math.Abs(n) < 1e18 {
			return CloneDepth(strconv.FormatInt(int64(n), 10)), nil
		}
		return CloneDepth(fmt.Sprint(n)), nil
	case bool:
		return CloneDepth(strconv.FormatBool(n)), nil
	default:
		return "", fmt.Errorf("clone_depth: want \"full\" or a positive integer, got %T", v)
	}
}

// GitDepth converts the setting to a GitCloneConfig.Depth value. ok is
// false when the setting is empty (not set). "full" yields 0 (full clone);
// a positive integer N yields N. Any other value is an error: 0 and
// negative numbers are rejected so that "full" is the only way to ask for
// a full clone. The accepted forms match the schemas exactly: lowercase
// "full" or ^[1-9][0-9]{0,8}$ (no sign, spaces or leading zeros, at most
// MaxCloneDepth).
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

// isPositiveDecimal reports whether s matches ^[1-9][0-9]{0,8}$, the same
// form the settings and agent schemas accept.
func isPositiveDecimal(s string) bool {
	if s == "" || len(s) > len(strconv.Itoa(MaxCloneDepth)) || s[0] < '1' || s[0] > '9' {
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
