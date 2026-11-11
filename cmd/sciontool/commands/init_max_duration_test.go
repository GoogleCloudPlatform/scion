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

package commands

import (
	"testing"
	"time"
)

// TestMaxDurationLimit pins how init reads SCION_MAX_DURATION: "0" is the
// value the hub and web send for "no duration limit", so it must set no
// timer, rather than an immediate one or an error.
func TestMaxDurationLimit(t *testing.T) {
	for _, tc := range []struct {
		in    string
		want  time.Duration
		limit bool
	}{
		{in: "0", limit: false},
		{in: "0s", limit: false},
		{in: "", limit: false},
		{in: "bad", limit: false},
		{in: "-5m", want: -5 * time.Minute, limit: false},
		{in: "90s", want: 90 * time.Second, limit: true},
		{in: "1h30m", want: 90 * time.Minute, limit: true},
	} {
		got, limit := maxDurationLimit(tc.in)
		if limit != tc.limit {
			t.Errorf("maxDurationLimit(%q) sets a limit = %v, want %v", tc.in, limit, tc.limit)
		}
		if tc.limit && got != tc.want {
			t.Errorf("maxDurationLimit(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
