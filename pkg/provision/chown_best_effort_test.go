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

package provision

import "testing"

func TestChownBestEffortRequested(t *testing.T) {
	cases := map[string]bool{
		"1": true,
		"":  false, "0": false, "true": false, "TRUE": false, "yes": false, " 1": false, "1 ": false, "11": false,
	}
	for value, want := range cases {
		getenv := func(key string) string {
			if key == ChownBestEffortEnv {
				return value
			}
			return ""
		}
		if got := ChownBestEffortRequested(getenv); got != want {
			t.Errorf("value %q: got %v, want %v", value, got, want)
		}
	}
	if ChownBestEffortRequested(func(string) string { return "" }) {
		t.Error("unset variable must not request best-effort")
	}
}
