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

package hub

import "testing"

func TestIsValidExitReason(t *testing.T) {
	tests := []struct {
		reason string
		want   bool
	}{
		// Valid: empty means "no reason given"
		{"", true},
		// Valid terminal activities
		{"crashed", true},
		{"limits_exceeded", true},
		// Invalid: non-terminal activities
		{"working", false},
		{"thinking", false},
		{"executing", false},
		{"waiting_for_input", false},
		{"completed", false},
		{"blocked", false},
		{"stalled", false},
		{"offline", false},
		// Invalid: arbitrary strings
		{"random_string", false},
		{"error", false},
		{"stopped", false},
	}

	for _, tc := range tests {
		t.Run("reason="+tc.reason, func(t *testing.T) {
			got := isValidExitReason(tc.reason)
			if got != tc.want {
				t.Errorf("isValidExitReason(%q) = %v, want %v", tc.reason, got, tc.want)
			}
		})
	}
}
