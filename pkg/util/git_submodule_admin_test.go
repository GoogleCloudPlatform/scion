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

package util

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type submoduleFixture struct {
	t      *testing.T
	gitDir string
}

func newSubmoduleFixture(t *testing.T) *submoduleFixture {
	return &submoduleFixture{t: t, gitDir: filepath.Join(t.TempDir(), ".git")}
}

func (f *submoduleFixture) path(rel string) string {
	return filepath.Join(f.gitDir, filepath.FromSlash(rel))
}

func (f *submoduleFixture) dir(rel string) {
	f.t.Helper()
	if err := os.MkdirAll(f.path(rel), 0o755); err != nil {
		f.t.Fatal(err)
	}
}

func (f *submoduleFixture) file(rel string) {
	f.t.Helper()
	f.dir(filepath.ToSlash(filepath.Dir(filepath.FromSlash(rel))))
	if err := os.WriteFile(f.path(rel), []byte("x\n"), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *submoduleFixture) link(target, rel string) {
	f.t.Helper()
	f.dir(filepath.ToSlash(filepath.Dir(filepath.FromSlash(rel))))
	if err := os.Symlink(target, f.path(rel)); err != nil {
		f.t.Fatal(err)
	}
}

func TestSubmoduleAdminDirs_Discovery(t *testing.T) {
	f := newSubmoduleFixture(t)
	f.file("modules/a/HEAD")
	f.file("modules/a/config")
	f.file("modules/a/logs/HEAD") // matched by the HEAD predicate, as in the runtime
	f.file("modules/a/modules/b/HEAD")
	f.dir("modules/libs")
	f.file("modules/libs/c/HEAD")
	dirs, unsafe, err := SubmoduleAdminDirs(f.gitDir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"modules/a", "modules/a/logs", "modules/a/modules/b", "modules/libs/c"}
	if !reflect.DeepEqual(dirs, want) {
		t.Errorf("dirs = %q, want %q", dirs, want)
	}
	if len(unsafe) != 0 {
		t.Errorf("clean tree reported unsafe %q", unsafe)
	}
}

func TestSubmoduleAdminDirs_AbsentOrNotADir(t *testing.T) {
	f := newSubmoduleFixture(t)
	f.dir("")
	if dirs, unsafe, err := SubmoduleAdminDirs(f.gitDir); dirs != nil || unsafe != nil || err != nil {
		t.Errorf("absent modules = %q, %q, %v; want nil, nil, nil", dirs, unsafe, err)
	}
	f.file("modules")
	if _, unsafe, _ := SubmoduleAdminDirs(f.gitDir); !reflect.DeepEqual(unsafe, []string{"modules"}) {
		t.Errorf("modules as a file: unsafe = %q", unsafe)
	}
	g := newSubmoduleFixture(t)
	g.file("elsewhere/x")
	g.link(g.path("elsewhere"), "modules")
	if dirs, unsafe, _ := SubmoduleAdminDirs(g.gitDir); dirs != nil || !reflect.DeepEqual(unsafe, []string{"modules"}) {
		t.Errorf("symlinked modules: dirs %q unsafe %q", dirs, unsafe)
	}
}

func TestSubmoduleAdminDirs_Unsafe(t *testing.T) {
	f := newSubmoduleFixture(t)
	f.file("modules/a/HEAD")
	f.file("elsewhere/HEAD")
	// Inside a's internals: not unsafe.
	f.link(f.path("elsewhere"), "modules/a/hooks/pre-commit")
	f.link(f.path("elsewhere"), "modules/a/objects/info/alt")
	// Outside any admin dir's internals: unsafe.
	f.link(f.path("elsewhere"), "modules/x")
	// In a's nested modules subtree: unsafe.
	f.link(f.path("elsewhere"), "modules/a/modules/y")
	// Seed-read files that are not regular, and a symlinked worktrees/<n>.
	f.dir("modules/a/config")
	f.dir("modules/a/worktrees/w1/config.worktree")
	f.link(f.path("elsewhere"), "modules/a/worktrees/w2")
	_, unsafe, err := SubmoduleAdminDirs(f.gitDir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, u := range unsafe {
		got[u] = true
	}
	for _, want := range []string{"modules/x", "modules/a/modules/y", "modules/a/config", "modules/a/worktrees/w1/config.worktree", "modules/a/worktrees/w2"} {
		if !got[want] {
			t.Errorf("unsafe lacks %s (got %q)", want, unsafe)
		}
	}
	for _, notWant := range []string{"modules/a/hooks/pre-commit", "modules/a/objects/info/alt"} {
		if got[notWant] {
			t.Errorf("unsafe flags %s inside an admin dir's internals", notWant)
		}
	}
}
