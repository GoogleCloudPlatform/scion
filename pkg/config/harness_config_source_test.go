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

import (
	"os"
	"path/filepath"
	"testing"
)

func writeSourceTestHC(t *testing.T, dir, image string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "harness: claude\nimage: " + image + "\nuser: scion\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestResolveHarnessConfigDir_Source pins the shared resolution order and the
// provenance recorded for each branch (ptone/scion#618, ptone/scion#620).
func TestResolveHarnessConfigDir_Source(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	project := filepath.Join(t.TempDir(), "project")
	tpl := filepath.Join(t.TempDir(), "tpl")
	hydrated := filepath.Join(t.TempDir(), "claude")
	writeSourceTestHC(t, filepath.Join(home, ".scion", harnessConfigsDirName, "claude"), "global")
	writeSourceTestHC(t, filepath.Join(project, harnessConfigsDirName, "claude"), "project")
	writeSourceTestHC(t, filepath.Join(tpl, harnessConfigsDirName, "claude"), "template")
	writeSourceTestHC(t, hydrated, "hydrated")
	writeSourceTestHC(t, filepath.Join(home, ".scion", harnessConfigsDirName, "only-global"), "global-only")

	cases := []struct {
		name      string
		hydrated  string
		hcName    string
		templates []string
		image     string
		source    HarnessConfigSource
	}{
		{"hydrated wins over every on-disk tier", hydrated, "claude", []string{tpl}, "hydrated", HarnessConfigSourceHubHydrated},
		{"template-bundled outranks project", "", "claude", []string{tpl}, "template", HarnessConfigSourceTemplateBundled},
		{"project is broker-local", "", "claude", nil, "project", HarnessConfigSourceBrokerLocal},
		{"global is broker-local", "", "only-global", nil, "global-only", HarnessConfigSourceBrokerLocal},
		{"synthetic generic is builtin", "", "generic", nil, "scion-base:latest", HarnessConfigSourceBuiltin},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hcDir, err := ResolveHarnessConfigDir(tc.hydrated, tc.hcName, project, tc.templates...)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if hcDir.Config.Image != tc.image {
				t.Errorf("image = %q, want %q (wrong tier resolved)", hcDir.Config.Image, tc.image)
			}
			if hcDir.Source != tc.source {
				t.Errorf("Source = %q, want %q", hcDir.Source, tc.source)
			}
		})
	}
}

// TestResolveHarnessConfigDir_HydratedLoadErrorDoesNotFallBack: launch never
// falls back from a broken hydrated copy to an on-disk one, so neither does
// the shared resolver.
func TestResolveHarnessConfigDir_HydratedLoadErrorDoesNotFallBack(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeSourceTestHC(t, filepath.Join(home, ".scion", harnessConfigsDirName, "claude"), "global")

	if _, err := ResolveHarnessConfigDir(filepath.Join(t.TempDir(), "missing"), "claude", ""); err == nil {
		t.Fatal("expected error for an unloadable hydrated path, got fallback to disk")
	}
}
