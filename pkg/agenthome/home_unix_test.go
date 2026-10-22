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

package agenthome

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// fixture is a home plus an "outside" directory holding a file the tests
// check is never written, removed or traversed.
type fixture struct {
	home, outside, victim string
	h                     *Home
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	base := t.TempDir()
	f := &fixture{home: filepath.Join(base, "home"), outside: filepath.Join(base, "outside")}
	for _, d := range []string{f.home, f.outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.victim = filepath.Join(f.outside, "victim")
	if err := os.WriteFile(f.victim, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	f.h = h
	return f
}

func (f *fixture) mkdir(t *testing.T, rel string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(f.home, rel), 0o755); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) link(t *testing.T, target, rel string) {
	t.Helper()
	if err := os.Symlink(target, filepath.Join(f.home, rel)); err != nil {
		t.Fatal(err)
	}
}

// checkOutside asserts the outside directory still holds exactly the
// victim file with its original content.
func (f *fixture) checkOutside(t *testing.T) {
	t.Helper()
	got, err := os.ReadFile(f.victim)
	if err != nil || string(got) != "original" {
		t.Fatalf("victim = %q, %v; want unchanged", got, err)
	}
	entries, err := os.ReadDir(f.outside)
	if err != nil || len(entries) != 1 {
		t.Fatalf("outside dir entries = %v, %v; want only the victim", entries, err)
	}
}

func isLinkRefusal(err error) bool {
	return errors.Is(err, ErrLinkComponent) || errors.Is(err, ErrNotDirectory)
}

func TestWriteFile_ReplacesLinkLeaf(t *testing.T) {
	f := newFixture(t)
	f.mkdir(t, "a")
	f.link(t, f.victim, "a/file")
	if err := f.h.WriteFile("a/file", []byte("new"), 0o640); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(filepath.Join(f.home, "a/file"))
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o640 {
		t.Fatalf("leaf = %v, %v; want a regular 0640 file", fi, err)
	}
	if got, _ := os.ReadFile(filepath.Join(f.home, "a/file")); string(got) != "new" {
		t.Fatalf("content = %q", got)
	}
	f.checkOutside(t)
}

func TestWriteFile_RefusesLinkedDirComponent(t *testing.T) {
	f := newFixture(t)
	f.link(t, f.outside, "a")
	err := f.h.WriteFile("a/victim", []byte("new"), 0o644)
	if !errors.Is(err, ErrLinkComponent) {
		t.Fatalf("err = %v, want ErrLinkComponent", err)
	}
	var pe *fs.PathError
	if !errors.As(err, &pe) || pe.Path != filepath.Join(f.home, "a") {
		t.Fatalf("err = %#v, want a PathError naming the component", err)
	}
	f.checkOutside(t)
}

func TestWriteFile_PermissionErrorKeepsPath(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	f := newFixture(t)
	f.mkdir(t, "ro")
	if err := os.Chmod(filepath.Join(f.home, "ro"), 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(f.home, "ro"), 0o755) })
	err := f.h.WriteFile("ro/file", []byte("x"), 0o644)
	var pe *fs.PathError
	if !errors.Is(err, fs.ErrPermission) || !errors.As(err, &pe) || pe.Path != filepath.Join(f.home, "ro/file") {
		t.Fatalf("err = %v, want a permission PathError for ro/file", err)
	}
}

func TestMkdirAll_CreatesAndRefusesLinkedComponent(t *testing.T) {
	f := newFixture(t)
	if err := f.h.MkdirAll("x/y/z", 0o700); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(filepath.Join(f.home, "x/y/z")); err != nil || !fi.IsDir() {
		t.Fatalf("x/y/z = %v, %v", fi, err)
	}
	f.link(t, f.outside, "l")
	if err := f.h.MkdirAll("l/sub", 0o755); !isLinkRefusal(err) {
		t.Fatalf("err = %v, want a link refusal", err)
	}
	if err := f.h.MkdirAll("l", 0o755); !isLinkRefusal(err) {
		t.Fatalf("leaf link: err = %v, want a link refusal", err)
	}
	f.checkOutside(t)
}

func TestRemove_LinkLeafAndLinkedParent(t *testing.T) {
	f := newFixture(t)
	f.link(t, f.victim, "leaf")
	if err := f.h.Remove("leaf"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(f.home, "leaf")); !os.IsNotExist(err) {
		t.Fatalf("leaf link still present: %v", err)
	}
	f.link(t, f.outside, "p")
	if err := f.h.Remove("p/victim"); !isLinkRefusal(err) {
		t.Fatalf("err = %v, want a link refusal", err)
	}
	if err := f.h.Remove("missing/file"); err != nil {
		t.Fatalf("missing parent: %v", err)
	}
	if err := f.h.Remove("missing"); err != nil {
		t.Fatalf("missing leaf: %v", err)
	}
	f.mkdir(t, "emptydir")
	if err := f.h.Remove("emptydir"); err != nil {
		t.Fatalf("empty dir: %v", err)
	}
	f.checkOutside(t)
}

func TestRemoveAll_NeverFollowsLinks(t *testing.T) {
	f := newFixture(t)
	f.mkdir(t, "tree/sub")
	if err := os.WriteFile(filepath.Join(f.home, "tree/sub/f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.link(t, f.outside, "tree/dirlink")
	f.link(t, f.victim, "tree/sub/filelink")
	if err := f.h.RemoveAll("tree"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(f.home, "tree")); !os.IsNotExist(err) {
		t.Fatalf("tree still present: %v", err)
	}
	f.checkOutside(t)
}

func TestRemoveAll_LinkLeafRemovedItself(t *testing.T) {
	f := newFixture(t)
	f.link(t, f.outside, "dir")
	if err := f.h.RemoveAll("dir"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(f.home, "dir")); !os.IsNotExist(err) {
		t.Fatalf("link still present: %v", err)
	}
	f.checkOutside(t)
}

func TestRemoveAll_RefusesLinkedParent(t *testing.T) {
	f := newFixture(t)
	f.link(t, f.outside, "p")
	if err := f.h.RemoveAll("p/victim"); !isLinkRefusal(err) {
		t.Fatalf("err = %v, want a link refusal", err)
	}
	if err := f.h.RemoveAll("nope/deeper"); err != nil {
		t.Fatalf("missing parent: %v", err)
	}
	f.checkOutside(t)
}

func TestHome_LinkPlantedBetweenOperationsIsRefused(t *testing.T) {
	f := newFixture(t)
	if err := f.h.MkdirAll("a/b", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := f.h.WriteFile("a/b/file", []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Replace a/b with a link after the Home was opened and used.
	if err := os.RemoveAll(filepath.Join(f.home, "a/b")); err != nil {
		t.Fatal(err)
	}
	f.link(t, f.outside, "a/b")
	if err := f.h.WriteFile("a/b/victim", []byte("2"), 0o644); !isLinkRefusal(err) {
		t.Fatalf("write: err = %v, want a link refusal", err)
	}
	if err := f.h.RemoveAll("a/b/victim"); !isLinkRefusal(err) {
		t.Fatalf("remove: err = %v, want a link refusal", err)
	}
	if _, err := f.h.ReadFile("a/b/victim", 1<<10); !isLinkRefusal(err) {
		t.Fatalf("read: err = %v, want a link refusal", err)
	}
	f.checkOutside(t)
}

func TestReadFile(t *testing.T) {
	f := newFixture(t)
	f.mkdir(t, "d")
	if err := os.WriteFile(filepath.Join(f.home, "d/f"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := f.h.ReadFile("d/f", 100)
	if err != nil || string(got) != "hello" {
		t.Fatalf("ReadFile = %q, %v", got, err)
	}
	if _, err := f.h.ReadFile("d/missing", 100); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing: err = %v, want ErrNotExist", err)
	}
	f.link(t, f.victim, "d/l")
	if _, err := f.h.ReadFile("d/l", 100); err == nil {
		t.Fatal("ReadFile followed a link leaf")
	}
}

func TestPathValidation(t *testing.T) {
	f := newFixture(t)
	for _, rel := range []string{"", "/abs", "..", "../x", "a/../b", "."} {
		if err := f.h.WriteFile(rel, nil, 0o644); err == nil {
			t.Errorf("WriteFile(%q) accepted", rel)
		}
		if err := f.h.RemoveAll(rel); err == nil {
			t.Errorf("RemoveAll(%q) accepted", rel)
		}
	}
	if err := f.h.MkdirAll(".", 0o755); err != nil {
		t.Errorf("MkdirAll(.) = %v", err)
	}
}

func newSource(t *testing.T) (*os.Root, string) {
	t.Helper()
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "tree/sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "tree/a.txt"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "tree/sub/run.sh"), []byte("#!"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/hostname", filepath.Join(src, "tree/skip")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(src)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root, src
}

func TestInstallTree_ReplacesDestination(t *testing.T) {
	src, _ := newSource(t)
	for _, tc := range []struct {
		name  string
		plant func(f *fixture)
	}{
		{"absent", func(f *fixture) {}},
		{"link leaf", func(f *fixture) { f.link(t, f.outside, "in/dst") }},
		{"dir with links", func(f *fixture) {
			f.mkdir(t, "in/dst/old")
			f.link(t, f.outside, "in/dst/old/l")
			f.link(t, f.victim, "in/dst/fl")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.mkdir(t, "in")
			tc.plant(f)
			if err := f.h.InstallTree(src, "tree", "in/dst", Limits{}); err != nil {
				t.Fatal(err)
			}
			dst := filepath.Join(f.home, "in/dst")
			if fi, err := os.Lstat(dst); err != nil || !fi.IsDir() {
				t.Fatalf("dst = %v, %v", fi, err)
			}
			if got, _ := os.ReadFile(filepath.Join(dst, "a.txt")); string(got) != "a" {
				t.Fatalf("a.txt = %q", got)
			}
			if fi, _ := os.Lstat(filepath.Join(dst, "a.txt")); fi.Mode().Perm() != 0o644 {
				t.Fatalf("a.txt mode = %v", fi.Mode())
			}
			if fi, _ := os.Lstat(filepath.Join(dst, "sub/run.sh")); fi.Mode().Perm() != 0o755 {
				t.Fatalf("run.sh mode = %v", fi.Mode())
			}
			if _, err := os.Lstat(filepath.Join(dst, "skip")); !os.IsNotExist(err) {
				t.Fatalf("source link copied: %v", err)
			}
			entries, _ := os.ReadDir(filepath.Join(f.home, "in"))
			if len(entries) != 1 {
				t.Fatalf("leftover entries in parent: %v", entries)
			}
			f.checkOutside(t)
		})
	}
}

func TestInstallTree_RefusesLinkedParent(t *testing.T) {
	src, _ := newSource(t)
	f := newFixture(t)
	f.link(t, f.outside, "in")
	if err := f.h.InstallTree(src, "tree", "in/dst", Limits{}); !isLinkRefusal(err) {
		t.Fatalf("err = %v, want a link refusal", err)
	}
	f.checkOutside(t)
}

func TestInstallTree_LimitLeavesDestination(t *testing.T) {
	src, srcDir := newSource(t)
	f := newFixture(t)
	f.mkdir(t, "in/dst")
	if err := os.WriteFile(filepath.Join(f.home, "in/dst/keep"), []byte("k"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(srcDir, "tree/sub/deeper"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, lim := range []Limits{{MaxFiles: 2}, {MaxBytes: 2}, {MaxDepth: 1}} {
		err := f.h.InstallTree(src, "tree", "in/dst", lim)
		var le *LimitError
		if !errors.As(err, &le) {
			t.Fatalf("lim %+v: err = %v, want LimitError", lim, err)
		}
		if got, _ := os.ReadFile(filepath.Join(f.home, "in/dst/keep")); string(got) != "k" {
			t.Fatalf("lim %+v: destination changed", lim)
		}
		entries, _ := os.ReadDir(filepath.Join(f.home, "in"))
		if len(entries) != 1 {
			t.Fatalf("lim %+v: leftover entries: %v", lim, entries)
		}
	}
}
