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

// These tests create symlinks.
//go:build unix

package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// legacyNarrowModuleAdminDirs is narrowModuleAdminDirs as it was before the
// discovery moved to util.SubmoduleAdminDirs, kept verbatim as the reference the
// current function must match bind for bind.
func legacyNarrowModuleAdminDirs(registerMount func(string, string, bool, bool), gitDir, containerGitDir string) error {
	modulesDir := filepath.Join(gitDir, "modules")
	return filepath.WalkDir(modulesDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if _, serr := os.Stat(filepath.Join(path, "HEAD")); serr != nil {
			return nil
		}
		rel, relErr := filepath.Rel(gitDir, path)
		if relErr != nil {
			return nil
		}
		relComps := strings.Split(filepath.ToSlash(rel), "/")
		leaf := func(name string) []string {
			c := make([]string, 0, len(relComps)+1)
			c = append(c, relComps...)
			return append(c, name)
		}
		for _, dir := range []string{"hooks", "info"} {
			if e := bindChecked(registerMount, gitDir, containerGitDir, true, bindDirRequire, leaf(dir)...); e != nil {
				return e
			}
		}
		return bindChecked(registerMount, gitDir, containerGitDir, true, bindFileRequire, leaf("config")...)
	})
}

// moduleAdminFixture builds a .git with nested submodule admin dirs, including
// the shapes the discovery predicate treats specially.
func moduleAdminFixture(t *testing.T) string {
	t.Helper()
	gitDir := filepath.Join(t.TempDir(), ".git")
	mk := func(rel string) {
		if err := os.MkdirAll(filepath.Join(gitDir, filepath.FromSlash(rel)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	file := func(rel string) {
		p := filepath.Join(gitDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A full admin dir, with a nested submodule and the logs/HEAD reflog that
	// the predicate also matches.
	file("modules/a/HEAD")
	file("modules/a/config")
	mk("modules/a/hooks")
	mk("modules/a/info")
	file("modules/a/logs/HEAD")
	file("modules/a/modules/b/HEAD")
	file("modules/a/modules/b/config")
	mk("modules/a/modules/b/hooks")
	// A grouping dir without HEAD, and an admin dir without config or hooks.
	mk("modules/libs")
	file("modules/libs/c/HEAD")
	// An admin dir whose HEAD is a symlink (os.Stat follows it).
	file("modules/d/real-head")
	mk("modules/d/info")
	if err := os.Symlink(filepath.Join(gitDir, "modules", "d", "real-head"), filepath.Join(gitDir, "modules", "d", "HEAD")); err != nil {
		t.Fatal(err)
	}
	// A symlinked directory the walk does not descend.
	file("elsewhere/HEAD")
	if err := os.Symlink(filepath.Join(gitDir, "elsewhere"), filepath.Join(gitDir, "modules", "e")); err != nil {
		t.Fatal(err)
	}
	return gitDir
}

// The binds are identical before and after the discovery moved to pkg/util.
func TestNarrowModuleAdminDirs_SameBindsAsBefore(t *testing.T) {
	record := func(fn func(func(string, string, bool, bool), string, string) error, gitDir string) ([]string, error) {
		var got []string
		err := fn(func(src, dst string, ro, _ bool) {
			got = append(got, fmt.Sprintf("%s -> %s ro=%v", src, dst, ro))
		}, gitDir, "/repo-root/.git")
		return got, err
	}
	for name, gitDir := range map[string]string{
		"nested fixture": moduleAdminFixture(t),
		"no modules dir": filepath.Join(t.TempDir(), ".git"),
	} {
		t.Run(name, func(t *testing.T) {
			before, berr := record(legacyNarrowModuleAdminDirs, gitDir)
			after, aerr := record(narrowModuleAdminDirs, gitDir)
			if (berr == nil) != (aerr == nil) {
				t.Fatalf("errors differ: before %v, after %v", berr, aerr)
			}
			if !reflect.DeepEqual(before, after) {
				t.Errorf("binds differ:\nbefore:\n%s\nafter:\n%s", strings.Join(before, "\n"), strings.Join(after, "\n"))
			}
			if name == "nested fixture" && len(after) == 0 {
				t.Error("fixture produced no binds; the comparison proves nothing")
			}
		})
	}
}
