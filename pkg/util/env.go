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
	"strconv"
	"strings"
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

// ParseBool parses s as a boolean. Leading and trailing whitespace is
// stripped (file-mounted secrets often end in a newline) and matching is
// case-insensitive. It accepts every spelling strconv.ParseBool understands
// (1, t, true, 0, f, false) plus yes/y/on as true and no/n/off as false.
// ok is false when s is empty or not a recognized spelling.
func ParseBool(s string) (value, ok bool) {
	v := strings.ToLower(strings.TrimSpace(s))
	if v == "" {
		return false, false
	}
	if b, err := strconv.ParseBool(v); err == nil {
		return b, true
	}
	switch v {
	case "yes", "y", "on":
		return true, true
	case "no", "n", "off":
		return false, true
	}
	return false, false
}

// LookupBoolEnv parses the named environment variable with ParseBool.
// ok is false when the variable is unset, empty, or not a recognized
// spelling. Callers that want to warn about a typo can check
// os.Getenv(key) for a non-empty value when ok is false.
func LookupBoolEnv(key string) (value, ok bool) {
	return ParseBool(os.Getenv(key))
}

// ParseBoolEnv returns the boolean value of the named environment variable
// (see ParseBool for the accepted spellings), or defaultVal when the
// variable is unset, empty, or not a recognized spelling.
func ParseBoolEnv(key string, defaultVal bool) bool {
	if v, ok := LookupBoolEnv(key); ok {
		return v
	}
	return defaultVal
}
