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
	"sort"
	"testing"
	"testing/fstest"

	"golang.org/x/sys/unix"
)

// treeSrc is a source tree for the *From copies: srcRoot/tpl holds a
// directory "unit" with a regular file, a nested directory, links to an
// outside file and an outside directory, a hard-linked file, a FIFO and
// Python bytecode.
type treeSrc struct {
	root, outside, marker string
}

func newTreeSrc(t *testing.T) *treeSrc {
	t.Helper()
	base := t.TempDir()
	s := &treeSrc{root: filepath.Join(base, "src"), outside: filepath.Join(base, "marker-dir")}
	s.marker = filepath.Join(s.outside, "marker")
	unit := filepath.Join(s.root, "tpl", "unit")
	for _, d := range []string{filepath.Join(unit, "nested"), filepath.Join(unit, "__pycache__"), s.outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, data string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(p, []byte(data), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(s.marker, "marker", 0o644)
	write(filepath.Join(unit, "MAIN.md"), "main", 0o644)
	write(filepath.Join(unit, "nested", "run.sh"), "#!", 0o755)
	write(filepath.Join(unit, "nested", "mod.pyc"), "bytecode", 0o644)
	write(filepath.Join(unit, "__pycache__", "x.pyc"), "bytecode", 0o644)
	write(filepath.Join(s.root, "hard-source"), "hard", 0o644)
	if err := os.Link(filepath.Join(s.root, "hard-source"), filepath.Join(unit, "hard.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(s.marker, filepath.Join(unit, "linked-file")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(s.outside, filepath.Join(unit, "linked-dir")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(unit, "fifo"), 0o644); err != nil {
		t.Fatal(err)
	}
	return s
}

func (s *treeSrc) open(t *testing.T) *Home {
	t.Helper()
	h, err := OpenBelow(s.root, "tpl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

// checkMarker asserts the outside marker directory is unchanged.
func (s *treeSrc) checkMarker(t *testing.T) {
	t.Helper()
	got, err := os.ReadFile(s.marker)
	if err != nil || string(got) != "marker" {
		t.Fatalf("marker = %q, %v; want unchanged", got, err)
	}
	entries, err := os.ReadDir(s.outside)
	if err != nil || len(entries) != 1 {
		t.Fatalf("marker dir entries = %v, %v; want only the marker", entries, err)
	}
}

// treeNames lists every path below dir (slash separated), without
// following links.
func treeNames(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p != dir {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func equalNames(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestInstallTreeFrom_SkipsSourceLinksAndSpecialFiles(t *testing.T) {
	s := newTreeSrc(t)
	f := newFixture(t)
	if err := f.h.InstallTreeFrom(s.open(t), "unit", "skills/unit", Limits{}); err != nil {
		t.Fatal(err)
	}
	got := treeNames(t, filepath.Join(f.home, "skills/unit"))
	want := []string{"MAIN.md", "nested", "nested/run.sh"}
	if !equalNames(got, want) {
		t.Fatalf("installed %v, want %v (links, hard links, FIFOs and bytecode skipped)", got, want)
	}
	s.checkMarker(t)
	f.checkOutside(t)
}

func TestInstallTreeFrom_LinkedSourceComponentIsRefused(t *testing.T) {
	s := newTreeSrc(t)
	if err := os.Symlink(filepath.Join(s.root, "tpl"), filepath.Join(s.root, "linked-tpl")); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenBelow(s.root, "linked-tpl"); !isLinkRefusal(err) {
		t.Fatalf("OpenBelow through a linked source dir = %v, want a link refusal", err)
	}
	// A linked directory inside the source is refused as srcRel too.
	f := newFixture(t)
	if err := f.h.InstallTreeFrom(s.open(t), "unit/linked-dir", "skills/x", Limits{}); !isLinkRefusal(err) {
		t.Fatalf("InstallTreeFrom of a linked srcRel = %v, want a link refusal", err)
	}
	if _, err := os.Lstat(filepath.Join(f.home, "skills/x")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("destination created from a linked source (err %v)", err)
	}
	s.checkMarker(t)
}

func TestInstallTreeFrom_DestinationLinks(t *testing.T) {
	s := newTreeSrc(t)

	t.Run("linked leaf is replaced", func(t *testing.T) {
		f := newFixture(t)
		f.mkdir(t, "skills")
		f.link(t, f.outside, "skills/unit")
		if err := f.h.InstallTreeFrom(s.open(t), "unit", "skills/unit", Limits{}); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Lstat(filepath.Join(f.home, "skills/unit"))
		if err != nil || !fi.IsDir() {
			t.Fatalf("leaf = %v, %v; want a real directory", fi, err)
		}
		f.checkOutside(t)
	})

	t.Run("linked parent is refused", func(t *testing.T) {
		f := newFixture(t)
		f.link(t, f.outside, "skills")
		if err := f.h.InstallTreeFrom(s.open(t), "unit", "skills/unit", Limits{}); !isLinkRefusal(err) {
			t.Fatalf("install below a linked parent = %v, want a link refusal", err)
		}
		f.checkOutside(t)
	})
}

func TestInstallTreeFrom_LimitKeepsDestination(t *testing.T) {
	s := newTreeSrc(t)
	f := newFixture(t)
	f.mkdir(t, "skills/unit")
	if err := os.WriteFile(filepath.Join(f.home, "skills/unit/OLD.md"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := f.h.InstallTreeFrom(s.open(t), "unit", "skills/unit", Limits{MaxFiles: 2})
	var le *LimitError
	if !errors.As(err, &le) {
		t.Fatalf("err = %v, want a *LimitError", err)
	}
	if got := treeNames(t, filepath.Join(f.home, "skills")); !equalNames(got, []string{"unit", "unit/OLD.md"}) {
		t.Fatalf("skills after a limit failure = %v, want the old copy only", got)
	}
}

func TestInstallTreeFS(t *testing.T) {
	f := newFixture(t)
	fsys := fstest.MapFS{
		"unit/SKILL.md":       {Data: []byte("skill"), Mode: 0o444},
		"unit/bin/tool":       {Data: []byte("#!"), Mode: 0o555},
		"unit/__pycache__/x":  {Data: []byte("x")},
		"unit/nested/mod.pyc": {Data: []byte("x")},
	}
	f.mkdir(t, "skills")
	f.link(t, f.outside, "skills/unit")
	if err := f.h.InstallTreeFS(fsys, "unit", "skills/unit", Limits{}); err != nil {
		t.Fatal(err)
	}
	got := treeNames(t, filepath.Join(f.home, "skills/unit"))
	if want := []string{"SKILL.md", "bin", "bin/tool", "nested"}; !equalNames(got, want) {
		t.Fatalf("installed %v, want %v", got, want)
	}
	for rel, want := range map[string]os.FileMode{"SKILL.md": 0o644, "bin/tool": 0o755} {
		fi, err := os.Stat(filepath.Join(f.home, "skills/unit", rel))
		if err != nil || fi.Mode().Perm() != want {
			t.Errorf("%s mode = %v, %v; want %v", rel, fi.Mode().Perm(), err, want)
		}
	}
	f.checkOutside(t)
}

// TestTreeCopyModes pins the mode rule of the tree copies: owner-only
// stays owner-only, everything else is 0644/0755, and setuid, setgid,
// sticky and group or other write bits are never copied.
func TestTreeCopyModes(t *testing.T) {
	src := t.TempDir()
	unit := filepath.Join(src, "unit")
	private := filepath.Join(unit, "private")
	if err := os.MkdirAll(private, 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]os.FileMode{"suid": 0o4755, "world": 0o666, "owner": 0o600, "owner-exec": 0o700}
	for name, mode := range files {
		p := filepath.Join(unit, name)
		if err := os.WriteFile(p, []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(private, 0o700); err != nil {
		t.Fatal(err)
	}
	want := map[string]os.FileMode{"suid": 0o755, "world": 0o644, "owner": 0o600, "owner-exec": 0o700, "private": 0o700}
	sh, err := OpenBelow(src, ".")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sh.Close() }()

	check := func(t *testing.T, dir string) {
		t.Helper()
		for name, m := range want {
			fi, err := os.Lstat(filepath.Join(dir, name))
			if err != nil {
				t.Fatal(err)
			}
			if got := fi.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky); got != m {
				t.Errorf("%s mode = %v, want %v", name, got, m)
			}
		}
	}
	t.Run("InstallTreeFrom", func(t *testing.T) {
		f := newFixture(t)
		if err := f.h.InstallTreeFrom(sh, "unit", "out", Limits{}); err != nil {
			t.Fatal(err)
		}
		check(t, filepath.Join(f.home, "out"))
	})
	t.Run("OverlayTreeFrom", func(t *testing.T) {
		f := newFixture(t)
		if err := f.h.OverlayTreeFrom(sh, "unit", "out", Limits{}); err != nil {
			t.Fatal(err)
		}
		check(t, filepath.Join(f.home, "out"))
	})
}

func TestOverlayTreeFrom_Merges(t *testing.T) {
	s := newTreeSrc(t)
	f := newFixture(t)
	f.mkdir(t, "nested")
	for rel, data := range map[string]string{"KEEP.md": "keep", "MAIN.md": "old", "nested/other": "other"} {
		if err := os.WriteFile(filepath.Join(f.home, rel), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.h.OverlayTreeFrom(s.open(t), "unit", ".", Limits{}); err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]string{"KEEP.md": "keep", "MAIN.md": "main", "nested/other": "other", "nested/run.sh": "#!"} {
		if got, err := os.ReadFile(filepath.Join(f.home, rel)); err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want %q", rel, got, err, want)
		}
	}
	for _, rel := range []string{"linked-file", "linked-dir", "hard.md", "fifo", "__pycache__", "nested/mod.pyc"} {
		if _, err := os.Lstat(filepath.Join(f.home, rel)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s copied (err %v), want skipped", rel, err)
		}
	}
	s.checkMarker(t)
	f.checkOutside(t)
}

func TestOverlayTreeFrom_NeverFollowsDestinationLinks(t *testing.T) {
	s := newTreeSrc(t)
	f := newFixture(t)
	// A link where a file goes, and a link where a directory goes.
	f.link(t, f.victim, "MAIN.md")
	f.link(t, f.outside, "nested")
	if err := f.h.OverlayTreeFrom(s.open(t), "unit", ".", Limits{}); err != nil {
		t.Fatal(err)
	}
	for rel, isDir := range map[string]bool{"MAIN.md": false, "nested": true} {
		fi, err := os.Lstat(filepath.Join(f.home, rel))
		if err != nil || fi.Mode()&fs.ModeSymlink != 0 || fi.IsDir() != isDir {
			t.Errorf("%s = %v, %v; want the link replaced by a real entry", rel, fi, err)
		}
	}
	f.checkOutside(t)

	t.Run("linked destination parent is refused", func(t *testing.T) {
		f := newFixture(t)
		f.link(t, f.outside, "dst")
		if err := f.h.OverlayTreeFrom(s.open(t), "unit", "dst/x", Limits{}); !isLinkRefusal(err) {
			t.Fatalf("overlay below a linked parent = %v, want a link refusal", err)
		}
		f.checkOutside(t)
	})
}

func TestOverlayTreeFrom_LimitWritesNothing(t *testing.T) {
	s := newTreeSrc(t)
	f := newFixture(t)
	err := f.h.OverlayTreeFrom(s.open(t), "unit", ".", Limits{MaxBytes: 3})
	var le *LimitError
	if !errors.As(err, &le) {
		t.Fatalf("err = %v, want a *LimitError", err)
	}
	if got := treeNames(t, f.home); len(got) != 0 {
		t.Fatalf("home after a limit failure = %v, want nothing written", got)
	}
}

func TestReadDirAndLstat(t *testing.T) {
	f := newFixture(t)
	f.mkdir(t, "dir/real")
	if err := os.WriteFile(filepath.Join(f.home, "dir/file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.link(t, f.outside, "dir/link")
	infos, err := f.h.ReadDir("dir")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]fs.FileMode{}
	var names []string
	for _, fi := range infos {
		names = append(names, fi.Name())
		got[fi.Name()] = fi.Mode().Type()
	}
	if !equalNames(names, []string{"file", "link", "real"}) {
		t.Fatalf("names = %v, want name order", names)
	}
	if got["link"] != fs.ModeSymlink || got["real"] != fs.ModeDir || got["file"] != 0 {
		t.Fatalf("types = %v; want the link reported as a link", got)
	}
	if fi, err := f.h.Lstat("dir/link"); err != nil || fi.Mode()&fs.ModeSymlink == 0 || fi.IsDir() {
		t.Fatalf("Lstat(link) = %v, %v; want a link, not its target", fi, err)
	}
	if _, err := f.h.ReadDir("dir/link"); !isLinkRefusal(err) {
		t.Fatalf("ReadDir of a linked dir = %v, want a link refusal", err)
	}
	if _, err := f.h.Lstat("dir/link/victim"); !isLinkRefusal(err) {
		t.Fatalf("Lstat below a linked dir = %v, want a link refusal", err)
	}
	if _, err := f.h.Lstat("dir/missing"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Lstat(missing) = %v, want fs.ErrNotExist", err)
	}
}

func TestSkipCopyName(t *testing.T) {
	for _, tc := range []struct {
		name  string
		isDir bool
		want  bool
	}{
		{"__pycache__", true, true},
		{"__pycache__", false, false},
		{"mod.pyc", false, true},
		{"mod.pyc", true, false},
		{"SKILL.md", false, false},
	} {
		if got := SkipCopyName(tc.name, tc.isDir); got != tc.want {
			t.Errorf("SkipCopyName(%q, %v) = %v, want %v", tc.name, tc.isDir, got, tc.want)
		}
	}
}
