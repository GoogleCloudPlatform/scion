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
	"strings"
	"testing"
)

func TestValidateName(t *testing.T) {
	valid := []string{"my-plugin", "acme.tools", "lint3r", "a", "a.b", "0", strings.Repeat("a", 64), "a-b.c-d"}
	for _, n := range valid {
		if err := ValidateName(n); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", n, err)
		}
	}
	invalid := []string{"", "My-Plugin", "-start", "end-", ".start", "end.", "has--double", "too.many..dots",
		"a..b", "a--b", strings.Repeat("a", 65), "under_score", "spa ce", "slash/x", "é"}
	for _, n := range invalid {
		if err := ValidateName(n); err == nil {
			t.Errorf("ValidateName(%q) = nil, want error", n)
		}
	}
}
