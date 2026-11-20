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

package harness

import "testing"

func TestHonoursSettingsArgs(t *testing.T) {
	if !HonoursSettingsArgs("generic") {
		t.Errorf("HonoursSettingsArgs(%q) = false, want true", "generic")
	}

	names := AllHarnessNames()
	if len(names) == 0 {
		t.Fatal("AllHarnessNames() returned empty slice")
	}
	for _, name := range names {
		want := name == "generic"
		if got := HonoursSettingsArgs(name); got != want {
			t.Errorf("HonoursSettingsArgs(%q) = %v, want %v", name, got, want)
		}
	}

	if HonoursSettingsArgs("custom-harness") {
		t.Errorf("HonoursSettingsArgs(%q) = true, want false", "custom-harness")
	}
}
