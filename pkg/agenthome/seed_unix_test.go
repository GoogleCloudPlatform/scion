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
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// seedSource builds a source tree from rel -> (content, mode) and returns
// it opened as an os.Root.
func seedSource(t *testing.T, files map[string]os.FileMode) (string, *os.Root) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "src")
	for rel, mode := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("content of "+rel), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return dir, root
}

func seedOpts() SeedOptions {
	return SeedOptions{UID: os.Getuid(), GID: os.Getgid()}
}

// Files get the source mode capped (0755 with any execute bit, else 0644,
// never setuid, setgid or sticky); new directories are 0700; files of the
// home the copy does not write are kept.
func TestSeedTree_ModesAndSeedOver(t *testing.T) {
	f := newFixture(t)
	_, src := seedSource(t, map[string]os.FileMode{
		"plain":       0o664,
		"private":     0o600,
		"exec":        0o775 | os.ModeSetuid,
		"sub/deep/x":  0o755 | os.ModeSetgid | os.ModeSticky,
		"sub/ro":      0o444,
		"sub/wexec":   0o777,
		"sub/onlyexe": 0o500,
	})
	if err := os.WriteFile(filepath.Join(f.home, "users-own"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := f.h.SeedTree(src, seedOpts())
	if err != nil {
		t.Fatalf("SeedTree: %v", err)
	}
	for rel, want := range map[string]os.FileMode{
		"plain": 0o644, "private": 0o600, "exec": 0o755, "sub/deep/x": 0o755,
		"sub/ro": 0o444, "sub/wexec": 0o755, "sub/onlyexe": 0o500,
	} {
		fi, err := os.Lstat(filepath.Join(f.home, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode() != want {
			t.Errorf("%s mode = %v, want %v", rel, fi.Mode(), want)
		}
		data, _ := os.ReadFile(filepath.Join(f.home, filepath.FromSlash(rel)))
		if string(data) != "content of "+rel {
			t.Errorf("%s content differs", rel)
		}
	}
	for _, d := range []string{"sub", "sub/deep"} {
		fi, err := os.Lstat(filepath.Join(f.home, d))
		if err != nil || fi.Mode() != os.ModeDir|0o700 {
			t.Errorf("directory %s: %v, err %v; want a 0700 directory", d, fi, err)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(f.home, "users-own")); string(data) != "keep" {
		t.Error("a file the copy does not write was not kept")
	}
	if res.Files != 7 || res.Dirs != 2 || len(res.Skipped) != 0 {
		t.Errorf("result = %+v, want 7 files, 2 dirs, nothing skipped", res)
	}
	// No temporary file is left behind.
	entries, _ := os.ReadDir(f.home)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("temporary file %s left in the home", e.Name())
		}
	}
}

// A second copy replaces files in place and keeps an existing directory's
// mode.
func TestSeedTree_ResyncReplacesFiles(t *testing.T) {
	f := newFixture(t)
	srcDir, src := seedSource(t, map[string]os.FileMode{"d/a": 0o644})
	if err := os.MkdirAll(filepath.Join(f.home, "d"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.home, "d", "a"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.SeedTree(src, seedOpts()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "d", "a"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.SeedTree(src, seedOpts()); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(f.home, "d", "a")); string(data) != "new" {
		t.Errorf("content after resync = %q, want new", data)
	}
	if fi, _ := os.Lstat(filepath.Join(f.home, "d")); fi.Mode().Perm() != 0o750 {
		t.Errorf("existing directory mode changed to %v", fi.Mode().Perm())
	}
}

// Links and special files in the source are skipped (never followed) and
// listed; an excluded directory is not descended into.
func TestSeedTree_SkipsLinksAndExcludes(t *testing.T) {
	f := newFixture(t)
	srcDir, src := seedSource(t, map[string]os.FileMode{"keep": 0o644, "secret/k": 0o600, "secretX": 0o644})
	if err := os.Symlink(f.victim, filepath.Join(srcDir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(f.outside, filepath.Join(srcDir, "dirlink")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(srcDir, "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := seedOpts()
	opts.Exclude = func(rel string) bool { return rel == "secret" || strings.HasPrefix(rel, "secret/") }
	res, err := f.h.SeedTree(src, opts)
	if err != nil {
		t.Fatalf("SeedTree: %v", err)
	}
	slices.Sort(res.Skipped)
	if !slices.Equal(res.Skipped, []string{"dirlink", "fifo", "link"}) {
		t.Errorf("skipped = %v", res.Skipped)
	}
	for _, rel := range []string{"link", "dirlink", "fifo", "secret", "victim"} {
		if _, err := os.Lstat(filepath.Join(f.home, rel)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s is in the home (err %v)", rel, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(f.home, "secretX")); err != nil {
		t.Error("a name that only shares a prefix with an exclude was not copied")
	}
}

// A link in the home where a file or directory goes is replaced, never
// followed or written through, and listed in Replaced; the link's target
// outside the home (a file, a missing file, or a directory holding files)
// is unchanged.
func TestSeedTree_ReplacesHomeLinks(t *testing.T) {
	for _, tc := range []struct {
		name   string
		plant  func(f *fixture) error
		wantAt string
		dir    bool
	}{
		{"file link", func(f *fixture) error { return os.Symlink(f.victim, filepath.Join(f.home, "a")) }, "a", false},
		{"dangling file link", func(f *fixture) error {
			return os.Symlink(filepath.Join(f.outside, "new"), filepath.Join(f.home, "a"))
		}, "a", false},
		{"directory link", func(f *fixture) error { return os.Symlink(f.outside, filepath.Join(f.home, "d")) }, "d", true},
		{"dangling directory link", func(f *fixture) error {
			return os.Symlink(filepath.Join(f.outside, "newdir"), filepath.Join(f.home, "d"))
		}, "d", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			_, src := seedSource(t, map[string]os.FileMode{"a": 0o644, "d/victim": 0o644})
			if err := tc.plant(f); err != nil {
				t.Fatal(err)
			}
			res, err := f.h.SeedTree(src, seedOpts())
			if err != nil {
				t.Fatalf("SeedTree: %v", err)
			}
			if !slices.Equal(res.Replaced, []string{tc.wantAt}) {
				t.Errorf("Replaced = %v, want [%s]", res.Replaced, tc.wantAt)
			}
			fi, err := os.Lstat(filepath.Join(f.home, tc.wantAt))
			switch {
			case err != nil:
				t.Fatal(err)
			case fi.Mode()&os.ModeSymlink != 0:
				t.Error("the link is still in the home")
			case tc.dir && (!fi.IsDir() || fi.Mode().Perm() != 0o700):
				t.Errorf("%s is %v, want a 0700 directory", tc.wantAt, fi.Mode())
			case !tc.dir && !fi.Mode().IsRegular():
				t.Errorf("%s is %v, want a regular file", tc.wantAt, fi.Mode())
			}
			if data, _ := os.ReadFile(f.victim); string(data) != "original" {
				t.Error("the copy wrote through the link")
			}
			entries, _ := os.ReadDir(f.outside)
			if len(entries) != 1 || entries[0].Name() != "victim" {
				t.Errorf("the outside directory has %d entries, want only victim", len(entries))
			}
		})
	}
}

// Any other entry of the wrong type where a file or directory goes is
// refused with a *SeedConflictError naming the home-relative entry, and is
// left as it was.
func TestSeedTree_RefusesWrongTypes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		plant  func(f *fixture) error
		wantAt string
	}{
		{"directory where a file goes", func(f *fixture) error { return os.Mkdir(filepath.Join(f.home, "a"), 0o700) }, "a"},
		{"file where a directory goes", func(f *fixture) error { return os.WriteFile(filepath.Join(f.home, "d"), []byte("mine"), 0o600) }, "d"},
		{"fifo where a file goes", func(f *fixture) error { return syscall.Mkfifo(filepath.Join(f.home, "a"), 0o600) }, "a"},
		{"fifo where a directory goes", func(f *fixture) error { return syscall.Mkfifo(filepath.Join(f.home, "d"), 0o600) }, "d"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			_, src := seedSource(t, map[string]os.FileMode{"a": 0o644, "d/victim": 0o644})
			if err := tc.plant(f); err != nil {
				t.Fatal(err)
			}
			before, _ := os.Lstat(filepath.Join(f.home, tc.wantAt))
			_, err := f.h.SeedTree(src, seedOpts())
			var conflict *SeedConflictError
			if !errors.As(err, &conflict) || conflict.Rel != tc.wantAt {
				t.Fatalf("SeedTree = %v, want a conflict at %s", err, tc.wantAt)
			}
			if strings.Contains(err.Error(), f.home) {
				t.Error("the conflict error names a host path")
			}
			after, err := os.Lstat(filepath.Join(f.home, tc.wantAt))
			if err != nil || after.Mode() != before.Mode() {
				t.Errorf("the wrong-type entry was changed or removed (err %v)", err)
			}
			if tc.name == "file where a directory goes" {
				if data, _ := os.ReadFile(filepath.Join(f.home, "d")); string(data) != "mine" {
					t.Error("the file where a directory goes was changed")
				}
			}
		})
	}
}

// A hard-linked source file is skipped like a link and never copied.
func TestSeedTree_SkipsHardLinkedSource(t *testing.T) {
	f := newFixture(t)
	srcDir, src := seedSource(t, map[string]os.FileMode{"keep": 0o644})
	if err := os.Link(f.victim, filepath.Join(srcDir, "hard")); err != nil {
		t.Fatalf("hard link in the test dir: %v", err)
	}
	res, err := f.h.SeedTree(src, seedOpts())
	if err != nil {
		t.Fatalf("SeedTree: %v", err)
	}
	if !slices.Equal(res.Skipped, []string{"hard"}) {
		t.Errorf("Skipped = %v, want [hard]", res.Skipped)
	}
	if _, err := os.Lstat(filepath.Join(f.home, "hard")); !errors.Is(err, os.ErrNotExist) {
		t.Error("a hard-linked source file was copied")
	}
}

// Over a limit the copy fails with a *LimitError.
func TestSeedTree_Limits(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]os.FileMode
		lim   Limits
	}{
		{"files", map[string]os.FileMode{"a": 0o644, "b": 0o644, "c": 0o644}, Limits{MaxFiles: 2}},
		{"bytes", map[string]os.FileMode{"a": 0o644, "b": 0o644}, Limits{MaxBytes: 15}},
		{"depth", map[string]os.FileMode{"1/2/3/x": 0o644}, Limits{MaxDepth: 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			_, src := seedSource(t, tc.files)
			opts := seedOpts()
			opts.Limits = tc.lim
			_, err := f.h.SeedTree(src, opts)
			var lim *LimitError
			if !errors.As(err, &lim) {
				t.Fatalf("SeedTree = %v, want a *LimitError", err)
			}
		})
	}
}

// A failed chown fails the copy and leaves nothing in place. An
// unprivileged process cannot give a file to another user; root can, so
// there the copy succeeds and the owner is checked instead.
func TestSeedTree_ChownFailureFails(t *testing.T) {
	f := newFixture(t)
	_, src := seedSource(t, map[string]os.FileMode{"a": 0o644})
	const other = 54321
	_, err := f.h.SeedTree(src, SeedOptions{UID: other, GID: other})
	if os.Getuid() == 0 {
		if err != nil {
			t.Fatalf("SeedTree as root: %v", err)
		}
		fi, err := os.Lstat(filepath.Join(f.home, "a"))
		if err != nil {
			t.Fatal(err)
		}
		if st := fi.Sys().(*syscall.Stat_t); st.Uid != other || st.Gid != other {
			t.Errorf("owner = %d:%d, want %d:%d", st.Uid, st.Gid, other, other)
		}
		return
	}
	if err == nil || !strings.Contains(err.Error(), "chown") {
		t.Fatalf("SeedTree = %v, want a chown failure", err)
	}
	if _, err := os.Lstat(filepath.Join(f.home, "a")); !errors.Is(err, os.ErrNotExist) {
		t.Error("a file was renamed into place after its chown failed")
	}
}

func TestSeedFileMode(t *testing.T) {
	for src, want := range map[os.FileMode]os.FileMode{
		0o777:                                 0o755,
		0o666:                                 0o644,
		0o600:                                 0o600,
		0o755 | os.ModeSetuid:                 0o755,
		0o644 | os.ModeSetgid | os.ModeSticky: 0o644,
		0o010:                                 0o010,
	} {
		if got := seedFileMode(src); got != want {
			t.Errorf("seedFileMode(%v) = %v, want %v", src, got, want)
		}
	}
}

// A link that appears between this copy's own mkdirat of a missing directory
// and the reopen (an injected race) is not replaced: the copy fails closed
// with a conflict at that entry, the link stays, and its target is
// untouched.
func TestSeedTree_ReopenRaceFailsClosed(t *testing.T) {
	f := newFixture(t)
	_, src := seedSource(t, map[string]os.FileMode{"d/victim": 0o644})
	var hookErr error
	seedDirAfterMkdirHook = func(dstFd int, name string) {
		if name != "d" {
			return
		}
		if err := unix.Unlinkat(dstFd, name, unix.AT_REMOVEDIR); err != nil {
			hookErr = err
			return
		}
		hookErr = unix.Symlinkat(f.outside, dstFd, name)
	}
	t.Cleanup(func() { seedDirAfterMkdirHook = nil })

	res, err := f.h.SeedTree(src, seedOpts())
	if hookErr != nil {
		t.Fatalf("race injection: %v", hookErr)
	}
	var conflict *SeedConflictError
	if !errors.As(err, &conflict) || conflict.Rel != "d" {
		t.Fatalf("SeedTree = %v, want a conflict at d", err)
	}
	if len(res.Replaced) != 0 {
		t.Errorf("Replaced = %v, want none: a link at the reopen must not be replaced", res.Replaced)
	}
	if fi, err := os.Lstat(filepath.Join(f.home, "d")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the raced link was removed or replaced (err %v)", err)
	}
	entries, _ := os.ReadDir(f.outside)
	if len(entries) != 1 || entries[0].Name() != "victim" {
		t.Errorf("the outside directory has %d entries, want only victim", len(entries))
	}
	if data, _ := os.ReadFile(f.victim); string(data) != "original" {
		t.Error("the link target changed")
	}
}
