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

import (
	"os"
	"path/filepath"
	"testing"
)

// `scion broker provide --project <p>` registers a local path only for the
// project it names (ptone/scion#2839): registering the CWD's unrelated
// project, often the global ~/.scion, made the broker provision p's agents
// there, where a delete cannot safely remove them.
func TestLocalPathForProvidedProject(t *testing.T) {
	const target, other = "11111111-aaaa-aaaa-aaaa-111111111111", "22222222-bbbb-bbbb-bbbb-222222222222"
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeSettings := func(scionDir, projectID string) {
		t.Helper()
		if err := os.MkdirAll(scionDir, 0o755); err != nil {
			t.Fatal(err)
		}
		body := "schema_version: \"1\"\n"
		if projectID != "" {
			body += "hub:\n  project_id: " + projectID + "\n"
		}
		if err := os.WriteFile(filepath.Join(scionDir, "settings.yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	globalDir := filepath.Join(home, ".scion")
	writeSettings(globalDir, other)
	linked := filepath.Join(home, "linked")
	writeSettings(filepath.Join(linked, ".scion"), target)
	unrelated := filepath.Join(home, "unrelated")
	writeSettings(filepath.Join(unrelated, ".scion"), other)
	unlinked := filepath.Join(home, "unlinked")
	writeSettings(filepath.Join(unlinked, ".scion"), "")

	for _, tc := range []struct {
		name, cwd string
		wantPath  bool
	}{
		{"CWD is the named project", linked, true},
		{"CWD is the global dir (home)", home, false},
		{"CWD is another project", unrelated, false},
		{"CWD project not linked", unlinked, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(tc.cwd)
			got := localPathForProvidedProject("", target)
			if tc.wantPath {
				want, _ := filepath.EvalSymlinks(filepath.Join(linked, ".scion"))
				if gotEval, _ := filepath.EvalSymlinks(got); gotEval != want {
					t.Errorf("got %q, want the named project's %q", got, want)
				}
			} else if got != "" {
				t.Errorf("got %q, want no local path (the broker resolves the project by slug)", got)
			}
		})
	}
	// The global project itself, provided from home, keeps its path.
	t.Chdir(home)
	if got := localPathForProvidedProject("", other); got == "" {
		t.Error("providing the global project from home registered no path")
	}
}
