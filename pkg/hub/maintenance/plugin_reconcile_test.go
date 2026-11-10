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

//go:build unix

package maintenance

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// rollbackSetup builds the state the boot-failure guard leaves after
// rolling nightly-20261023 (failed) back to nightly-20261022: current points
// at 22, the marker says from 23 to 22, both releases retained their
// plugins, and the plugin directory still holds 23's builds.
func rollbackSetup(t *testing.T) (root, dir string) {
	t.Helper()
	root = installLayout(t, "nightly-20261022", "nightly-20261023")
	for tag, body := range map[string]string{"nightly-20261022": "22", "nightly-20261023": "23"} {
		p := filepath.Join(root, "releases", tag, pluginsSubdir)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"telegram", "slack"} {
			if err := os.WriteFile(filepath.Join(p, PluginBinaryPrefix+name), []byte(name+" "+body), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	writeMarker(t, root, `{"at":"2026-10-24T03:10:00Z","from":"nightly-20261023","to":"nightly-20261022"}`)
	dir = pluginDirWith(t, map[string]string{"telegram": "telegram 23", "slack": "slack 23"})
	return root, dir
}

func writeMarker(t *testing.T, root, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, RollbackMarkerName), []byte(content+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReconcilePluginsAfterRollback_ReplacesFailedBuilds(t *testing.T) {
	root, dir := rollbackSetup(t)
	replaced, err := ReconcilePluginsAfterRollback(root, dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(dir, "scion-plugin-slack"), filepath.Join(dir, "scion-plugin-telegram")}
	if !slices.Equal(replaced, want) {
		t.Fatalf("replaced = %v, want %v", replaced, want)
	}
	wantFile(t, filepath.Join(dir, "scion-plugin-telegram"), "telegram 22")
	wantFile(t, filepath.Join(dir, "scion-plugin-slack"), "slack 22")
	noStaging(t, dir)

	// Idempotent: the next boot changes nothing.
	replaced, err = ReconcilePluginsAfterRollback(root, dir)
	if err != nil || len(replaced) != 0 {
		t.Fatalf("second run replaced %v, %v", replaced, err)
	}
}

func TestReconcilePluginsAfterRollback_LeavesOtherBuilds(t *testing.T) {
	root, dir := rollbackSetup(t)
	// Rebuilt by hand after the update: not the failed release's build.
	if err := os.WriteFile(filepath.Join(dir, "scion-plugin-slack"), []byte("slack from git"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Not installed here: never added.
	if err := os.Remove(filepath.Join(dir, "scion-plugin-telegram")); err != nil {
		t.Fatal(err)
	}
	replaced, err := ReconcilePluginsAfterRollback(root, dir)
	if err != nil || len(replaced) != 0 {
		t.Fatalf("replaced %v, %v", replaced, err)
	}
	wantFile(t, filepath.Join(dir, "scion-plugin-slack"), "slack from git")
	if _, err := os.Lstat(filepath.Join(dir, "scion-plugin-telegram")); !os.IsNotExist(err) {
		t.Fatalf("telegram was installed: %v", err)
	}
}

func TestReconcilePluginsAfterRollback_NothingToDo(t *testing.T) {
	for name, mutate := range map[string]func(t *testing.T, root, dir string){
		"no marker": func(t *testing.T, root, _ string) {
			if err := os.Remove(filepath.Join(root, RollbackMarkerName)); err != nil {
				t.Fatal(err)
			}
		},
		"current moved on": func(t *testing.T, root, _ string) {
			replaceLink(t, root, currentLinkName, "releases/nightly-20261023")
		},
		"restored release has no plugins": func(t *testing.T, root, _ string) {
			if err := os.RemoveAll(filepath.Join(root, "releases", "nightly-20261022", pluginsSubdir)); err != nil {
				t.Fatal(err)
			}
		},
		"failed release has no plugins": func(t *testing.T, root, _ string) {
			if err := os.RemoveAll(filepath.Join(root, "releases", "nightly-20261023", pluginsSubdir)); err != nil {
				t.Fatal(err)
			}
		},
		"restored plugins dir is a symlink": func(t *testing.T, root, _ string) {
			p := filepath.Join(root, "releases", "nightly-20261022", pluginsSubdir)
			outside := filepath.Join(t.TempDir(), "elsewhere")
			if err := os.Rename(p, outside); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, p); err != nil {
				t.Fatal(err)
			}
		},
		"marker names a path": func(t *testing.T, root, _ string) {
			writeMarker(t, root, `{"at":"2026-10-24T03:10:00Z","from":"../x","to":"nightly-20261022"}`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			root, dir := rollbackSetup(t)
			mutate(t, root, dir)
			replaced, _ := ReconcilePluginsAfterRollback(root, dir)
			if len(replaced) != 0 {
				t.Fatalf("replaced %v", replaced)
			}
			wantFile(t, filepath.Join(dir, "scion-plugin-telegram"), "telegram 23")
		})
	}
}

func TestReconcilePluginsAfterRollback_SymlinkedPluginUntouched(t *testing.T) {
	root, dir := rollbackSetup(t)
	p := filepath.Join(dir, "scion-plugin-telegram")
	target := filepath.Join(root, "releases", "nightly-20261023", pluginsSubdir, "scion-plugin-telegram")
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, p); err != nil {
		t.Fatal(err)
	}
	replaced, err := ReconcilePluginsAfterRollback(root, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(replaced, []string{filepath.Join(dir, "scion-plugin-slack")}) {
		t.Fatalf("replaced = %v", replaced)
	}
	if got, err := os.Readlink(p); err != nil || got != target {
		t.Fatalf("symlink changed: %q, %v", got, err)
	}
	wantFile(t, target, "telegram 23")
}

func TestSymlinkInstaller_RetainPlugins(t *testing.T) {
	root := installLayout(t, "nightly-20261022")
	s := newSymlinkInstaller(symlinkOpts(root))
	if got := s.ActiveRelease(); got != "nightly-20261022" {
		t.Fatalf("ActiveRelease = %q", got)
	}
	src := pluginDirWith(t, map[string]string{"telegram": "tg"})
	bins := map[string]string{"scion-plugin-telegram": filepath.Join(src, "scion-plugin-telegram")}
	if err := s.RetainPlugins("nightly-20261022", bins); err != nil {
		t.Fatal(err)
	}
	wantFile(t, filepath.Join(root, "releases", "nightly-20261022", pluginsSubdir, "scion-plugin-telegram"), "tg")
	// Replaced as a whole.
	if err := s.RetainPlugins("nightly-20261022", map[string]string{}); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(filepath.Join(root, "releases", "nightly-20261022", pluginsSubdir)); err != nil || len(entries) != 0 {
		t.Fatalf("entries = %v, %v", entries, err)
	}
	for _, bad := range []string{"../x", "nightly-20261099", "x.partial"} {
		if err := s.RetainPlugins(bad, bins); err == nil {
			t.Errorf("RetainPlugins(%q) succeeded", bad)
		}
	}
	if err := s.RetainPlugins("nightly-20261022", map[string]string{"../evil": bins["scion-plugin-telegram"]}); err == nil {
		t.Error("RetainPlugins accepted a bad file name")
	}
}
