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

package agentplugin

import (
	"fmt"
	"strings"
)

// MaxNameLength is the longest permitted plugin name (spec §5.5).
const MaxNameLength = 64

// ValidateName checks a plugin name against spec §5.5: 1-64 characters from
// [a-z0-9.-], alphanumeric first and last character, and no "--" or "..".
//
// It is hand-written rather than using the schema's regular expression,
// whose lookahead does not compile in RE2.
func ValidateName(name string) error {
	if name == "" {
		return fmt.Errorf("name is empty")
	}
	if len(name) > MaxNameLength {
		return fmt.Errorf("name %q is longer than %d characters", name, MaxNameLength)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !isLowerAlnum(c) && c != '.' && c != '-' {
			return fmt.Errorf("name %q may contain only lowercase letters, digits, '.' and '-'", name)
		}
	}
	if !isLowerAlnum(name[0]) || !isLowerAlnum(name[len(name)-1]) {
		return fmt.Errorf("name %q must start and end with a lowercase letter or digit", name)
	}
	if strings.Contains(name, "--") || strings.Contains(name, "..") {
		return fmt.Errorf("name %q must not contain \"--\" or \"..\"", name)
	}
	return nil
}

func isLowerAlnum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
}
