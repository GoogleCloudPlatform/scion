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

package version

import "testing"

func TestCompareBuild(t *testing.T) {
	cases := []struct {
		name, hub, broker string
		want              BuildMatch
	}{
		{"equal tags", "v1.2.0", "v1.2.0", BuildMatchSame},
		{"equal commits", "1344d136", "1344d136", BuildMatchSame},
		{"different tags", "v1.3.0", "v1.2.0", BuildMatchDifferent},
		{"tag vs commit", "v1.2.0", "1344d136", BuildMatchDifferent},
		{"no v normalisation", "1.2.0", "v1.2.0", BuildMatchDifferent},
		{"no whitespace normalisation", "v1.2.0", "v1.2.0 ", BuildMatchDifferent},
		{"no case folding", "ABCDEF12", "abcdef12", BuildMatchDifferent},
		{"empty broker", "v1.2.0", "", BuildMatchUnknown},
		{"empty hub", "", "v1.2.0", BuildMatchUnknown},
		{"both empty", "", "", BuildMatchUnknown},
		{"unknown broker", "v1.2.0", "unknown", BuildMatchUnknown},
		{"unknown hub", "unknown", "v1.2.0", BuildMatchUnknown},
		{"both unknown", "unknown", "unknown", BuildMatchUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CompareBuild(tc.hub, tc.broker); got != tc.want {
				t.Errorf("CompareBuild(%q, %q) = %q, want %q", tc.hub, tc.broker, got, tc.want)
			}
		})
	}
}
