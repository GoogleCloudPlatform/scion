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

import "testing"

// A hub_name the schema rejects is warned about at startup, not fatal
// (ptone/scion#2073).
func TestWarnNonConformingHubName(t *testing.T) {
	cases := map[string]bool{
		"":         false, // unset: falls back to the hostname
		"prod-hub": false,
		"Prod.Hub": true,
	}
	for name, want := range cases {
		if got := warnNonConformingHubName(name); got != want {
			t.Errorf("warnNonConformingHubName(%q) = %v, want %v", name, got, want)
		}
	}
}
