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

package state

import "testing"

func TestSuspendReason_AutoWakes(t *testing.T) {
	cases := []struct {
		reason SuspendReason
		want   bool
	}{
		{SuspendReasonIdle, true},
		{SuspendReasonStalled, true},
		{SuspendReasonUser, false},
		// A row suspended before suspend_reason existed has none and
		// never auto-wakes.
		{"", false},
		{"max_lifetime", false},
		{"bogus", false},
	}
	for _, tc := range cases {
		if got := tc.reason.AutoWakes(); got != tc.want {
			t.Errorf("SuspendReason(%q).AutoWakes() = %v, want %v", tc.reason, got, tc.want)
		}
	}
}

func TestSuspendReason_IsValid(t *testing.T) {
	for _, r := range []SuspendReason{"", SuspendReasonUser, SuspendReasonIdle, SuspendReasonStalled} {
		if !r.IsValid() {
			t.Errorf("SuspendReason(%q).IsValid() = false, want true", r)
		}
	}
	if SuspendReason("bogus").IsValid() {
		t.Error(`SuspendReason("bogus").IsValid() = true, want false`)
	}
}
