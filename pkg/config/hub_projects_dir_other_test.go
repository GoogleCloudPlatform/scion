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

//go:build !windows

package config

import (
	"os"
	"path/filepath"
	"testing"
)

// Outside Windows isUnderHubProjectsDir keeps its behaviour: a path below
// ~/.scion/projects is below it, the projects directory itself and a
// sibling are not, and an unknown home directory (GetGlobalDir error)
// reports "not below".
func TestHubProjectsDir_UnixBehaviour(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	projects := filepath.Join(home, ".scion", "projects")
	ws := filepath.Join(projects, "proj")
	sibling := filepath.Join(home, ".scion", "projects-sibling", "proj")
	for _, d := range []string{ws, sibling} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for p, want := range map[string]bool{ws: true, filepath.Join(projects, "not-yet"): true, projects: false, sibling: false} {
		if got := isUnderHubProjectsDir(p); got != want {
			t.Errorf("isUnderHubProjectsDir(%s) = %v, want %v", p, got, want)
		}
	}
	t.Setenv("HOME", "")
	if isUnderHubProjectsDir(ws) {
		t.Error("with no home directory, a path is reported below the projects directory")
	}
}
