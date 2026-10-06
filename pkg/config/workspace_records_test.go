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

package config

import "testing"

// TestWorkspaceRecordPaths_SingleCleanPathElement checks that a single clean
// path element is a non-empty component containing no path separator and no
// NUL byte, for the slugs and project IDs named in workspace records and
// project config directories.
func TestWorkspaceRecordPaths_SingleCleanPathElement(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const id = "7c1d2e3f-4a5b-4c6d-8e9f-0a1b2c3d4e5f"

	for _, s := range []string{"proj", "my-project", id} {
		if !isSinglePathElement(s) {
			t.Errorf("isSinglePathElement(%q) = false, want true", s)
		}
		if _, err := HubWorkspaceRecordPath(s); err != nil {
			t.Errorf("HubWorkspaceRecordPath(%q): %v", s, err)
		}
		if _, err := BrokerWorkspaceRecordPath(s); err != nil {
			t.Errorf("BrokerWorkspaceRecordPath(%q): %v", s, err)
		}
		if _, ok := ConfinedProjectConfigRoot(s, id); !ok {
			t.Errorf("ConfinedProjectConfigRoot(%q, id) not ok", s)
		}
	}

	for _, s := range []string{"", ".", "..", "a/b", `a\b`, "a\x00b", "\x00", "proj\x00"} {
		if isSinglePathElement(s) {
			t.Errorf("isSinglePathElement(%q) = true, want false", s)
		}
		if _, err := HubWorkspaceRecordPath(s); err == nil {
			t.Errorf("HubWorkspaceRecordPath(%q): want error", s)
		}
		if _, err := BrokerWorkspaceRecordPath(s); err == nil {
			t.Errorf("BrokerWorkspaceRecordPath(%q): want error", s)
		}
		if _, ok := ConfinedProjectConfigRoot(s, id); ok {
			t.Errorf("ConfinedProjectConfigRoot(%q, id) ok, want not ok", s)
		}
		if _, ok := ConfinedProjectConfigRoot("proj", s); ok {
			t.Errorf("ConfinedProjectConfigRoot(proj, %q) ok, want not ok", s)
		}
	}
}
