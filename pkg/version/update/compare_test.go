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

package update

import (
	"testing"
	"time"
)

func TestCompare(t *testing.T) {
	tests := []struct {
		current, target string
		want            Class
	}{
		// nightly, by date
		{"nightly-20261023", "nightly-20261023", ClassSame},
		{"nightly-20261022", "nightly-20261023", ClassUpgrade},
		{"nightly-20261023", "nightly-20261022", ClassDowngrade},
		{"nightly-20260930", "nightly-20261001", ClassUpgrade}, // month boundary
		{"nightly-20261231", "nightly-20270101", ClassUpgrade}, // year boundary
		// same-day suffix: string order would call ".1" older than ".10"
		// and "nightly-20261023.1" newer than "nightly-20261024".
		{"nightly-20261023", "nightly-20261023.1", ClassUpgrade},
		{"nightly-20261023.2", "nightly-20261023.10", ClassUpgrade},
		{"nightly-20261023.1", "nightly-20261024", ClassUpgrade},
		{"nightly-20261023.1", "nightly-20261023", ClassDowngrade},
		// preview / stable, semver
		{"v0.3.0-preview.3", "v0.3.0-preview.4", ClassUpgrade},
		{"v0.3.0-preview.4", "v0.3.0-preview.3", ClassDowngrade},
		{"v0.3.0-preview.4", "v0.3.0-preview.4", ClassSame},
		{"v0.3.0-preview.9", "v0.3.0-preview.10", ClassUpgrade},
		{"v1.0.0", "v1.1.0", ClassUpgrade},
		{"v1.1.0", "v1.0.0", ClassDowngrade},
		{"1.0.0", "v1.0.0", ClassSame},
		// cross-channel with a nightly on both sides is impossible; with a
		// preview/stable side there is no date in the string.
		{"nightly-20261023", "v0.3.0-preview.4", ClassUnknown},
		{"v0.3.0-preview.4", "nightly-20261023", ClassUnknown},
		{"v0.3.0-preview.4", "v1.0.0", ClassUnknown},
		// git build (no version) without a date
		{"", "nightly-20261023", ClassUnknown},
		// invalid targets
		{"nightly-20261023", "", ClassUnknown},
		{"nightly-20261023", "dev", ClassUnknown},
		{"nightly-20261023", "nightly-latest", ClassUnknown},
		{"nightly-20261023", "nightly-20261399", ClassUnknown},
		{"nightly-20261023", "nightly-../../x", ClassUnknown},
		{"v1.0.0", "v1.0.1+build", ClassUnknown},
	}
	for _, tt := range tests {
		if got := Compare(tt.current, tt.target); got != tt.want {
			t.Errorf("Compare(%q, %q) = %q, want %q", tt.current, tt.target, got, tt.want)
		}
	}
}

func TestCompareRefs_CrossChannelByDate(t *testing.T) {
	day := func(s string) time.Time {
		d, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	tests := []struct {
		name            string
		current, target Ref
		want            Class
	}{
		{
			name:    "nightly to newer preview",
			current: Ref{Version: "nightly-20261020", Date: day("2026-10-20T04:00:00Z")},
			target:  Ref{Version: "v0.3.0-preview.4", Date: day("2026-10-22T18:00:00Z")},
			want:    ClassSwitchNewer,
		},
		{
			name:    "nightly to older preview",
			current: Ref{Version: "nightly-20261023", Date: day("2026-10-23T04:00:00Z")},
			target:  Ref{Version: "v0.3.0-preview.4", Date: day("2026-10-22T18:00:00Z")},
			want:    ClassSwitchOlder,
		},
		{
			name:    "nightly tag date used when no publish date",
			current: Ref{Version: "nightly-20261023"},
			target:  Ref{Version: "v0.3.0-preview.4", Date: day("2026-10-24T01:00:00Z")},
			want:    ClassSwitchNewer,
		},
		{
			name:    "preview to nightly by tag date",
			current: Ref{Version: "v0.3.0-preview.4", Date: day("2026-10-22T18:00:00Z")},
			target:  Ref{Version: "nightly-20261024"},
			want:    ClassSwitchNewer,
		},
		{
			name:    "same publish instant is not newer",
			current: Ref{Version: "v0.3.0-preview.4", Date: day("2026-10-22T18:00:00Z")},
			target:  Ref{Version: "v1.0.0", Date: day("2026-10-22T18:00:00Z")},
			want:    ClassSwitchOlder,
		},
		{
			name:    "git build with build time",
			current: Ref{Version: "", Date: day("2026-10-21T00:00:00Z")},
			target:  Ref{Version: "nightly-20261023"},
			want:    ClassSwitchNewer,
		},
		{
			name:    "git build newer than target",
			current: Ref{Version: "", Date: day("2026-10-25T00:00:00Z")},
			target:  Ref{Version: "nightly-20261023"},
			want:    ClassSwitchOlder,
		},
		{
			name:    "same channel ignores dates",
			current: Ref{Version: "nightly-20261022", Date: day("2027-01-01T00:00:00Z")},
			target:  Ref{Version: "nightly-20261023", Date: day("2026-01-01T00:00:00Z")},
			want:    ClassUpgrade,
		},
		{
			name:    "missing target date",
			current: Ref{Version: "nightly-20261023"},
			target:  Ref{Version: "v0.3.0-preview.4"},
			want:    ClassUnknown,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CompareRefs(tt.current, tt.target); got != tt.want {
				t.Fatalf("CompareRefs = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestClassIsDowngrade(t *testing.T) {
	for c, want := range map[Class]bool{
		ClassSame: false, ClassUpgrade: false, ClassSwitchNewer: false,
		ClassSwitchOlder: true, ClassDowngrade: true, ClassUnknown: false,
	} {
		if got := c.IsDowngrade(); got != want {
			t.Errorf("%q.IsDowngrade() = %v, want %v", c, got, want)
		}
	}
}

func TestIsReleaseTag(t *testing.T) {
	for tag, want := range map[string]bool{
		"nightly-20261023":   true,
		"nightly-20261023.1": true,
		"v0.3.0-preview.4":   true,
		"v1.0.0":             true,
		"1.0.0":              true,
		"":                   false,
		"dev":                false,
		"latest":             false,
		"nightly-2026102":    false,
		"nightly-20261023/x": false,
		"v1.0.0+meta":        false,
		"v1.0.0/../x":        false,
	} {
		if got := IsReleaseTag(tag); got != want {
			t.Errorf("IsReleaseTag(%q) = %v, want %v", tag, got, want)
		}
	}
}
