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

package util

import (
	"fmt"
	"os"
)

// ExpandEnv replaces ${var} or $var in the string according to the values
// of the current environment variables. It warns to stderr if a variable is unset.
// It returns the expanded string and a boolean indicating if any warning was printed.
func ExpandEnv(s string) (string, bool) {
	warned := false
	expanded := os.Expand(s, func(key string) string {
		val, ok := os.LookupEnv(key)
		if !ok {
			fmt.Fprintf(os.Stderr, "Warning: environment variable %q is not set\n", key)
			warned = true
			return ""
		}
		return val
	})
	return expanded, warned
}

// FirstNonEmpty returns the first non-empty string from the given slice.
func FirstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

