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

package transfer

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
)

// openTestRoot opens dir as an os.Root, closed at the end of the test.
func openTestRoot(t *testing.T, dir string) *os.Root {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root
}

// A subdirectory replaced between the walk reading its entry and opening it
// fails the walk without blocking, in both users of the walk: by a FIFO
// (ENOTDIR, never opened); by a link (the trailing separator follows it, and
// the SameFile check refuses it: ErrDirReplaced); by another directory inside
// the root (only the SameFile check refuses it: ErrDirReplaced). An
// unchanged tree walks normally.
func TestWalkRoot_SubdirectorySwappedDuringWalk(t *testing.T) {
	swaps := map[string]func(t *testing.T, p string){
		"FIFO": func(t *testing.T, p string) {
			if err := os.RemoveAll(p); err != nil {
				t.Error(err)
			}
			if err := syscall.Mkfifo(p, 0o600); err != nil {
				t.Errorf("mkfifo: %v", err)
			}
		},
		"link": func(t *testing.T, p string) {
			outside := t.TempDir()
			writeReadTestFile(t, outside, "outside.txt", "outside\r\n")
			if err := os.RemoveAll(p); err != nil {
				t.Error(err)
			}
			if err := os.Symlink(outside, p); err != nil {
				t.Error(err)
			}
		},
		"replaced": func(t *testing.T, p string) {
			moved := p + ".moved"
			if err := os.Rename(p, moved); err != nil {
				t.Error(err)
			}
			writeReadTestFile(t, p, "new.txt", "new\r\n")
		},
	}
	walks := map[string]func(root *os.Root, dir string) error{
		"CollectFilesFromRoot": func(root *os.Root, dir string) error {
			_, err := CollectFilesFromRoot(root, dir, nil)
			return err
		},
		"NormalizeRoot": func(root *os.Root, _ string) error {
			return NormalizeRoot(root)
		},
	}
	for sname, swap := range swaps {
		for wname, walk := range walks {
			t.Run(sname+" "+wname, func(t *testing.T) {
				if sname == "FIFO" {
					if err := syscall.Mkfifo(filepath.Join(t.TempDir(), "probe"), 0o600); err != nil {
						t.Skipf("mkfifo: %v", err)
					}
				}
				dir := t.TempDir()
				writeReadTestFile(t, dir, "a.txt", "a\n")
				writeReadTestFile(t, dir, "sub/b.txt", "b\r\n")
				root := openTestRoot(t, dir)
				hook := func(rel string) {
					if rel == "sub" {
						swap(t, filepath.Join(dir, "sub"))
					}
				}
				walkRootBeforeOpenDir.Store(&hook)
				t.Cleanup(func() { walkRootBeforeOpenDir.Store(nil) })
				within(t, func() {
					err := walk(root, dir)
					if err == nil {
						t.Errorf("%s succeeded with sub replaced by a %s during the walk", wname, sname)
					} else if sname != "FIFO" && !errors.Is(err, ErrDirReplaced) {
						t.Errorf("%s with sub replaced by a %s: err = %v, want ErrDirReplaced (the SameFile refusal)", wname, sname, err)
					}
				})
			})
		}
	}
	t.Run("control", func(t *testing.T) {
		dir := t.TempDir()
		writeReadTestFile(t, dir, "a.txt", "a\n")
		writeReadTestFile(t, dir, "sub/b.txt", "b\r\n")
		root := openTestRoot(t, dir)
		files, err := CollectFilesFromRoot(root, dir, nil)
		if err != nil || len(files) != 2 {
			t.Fatalf("collect: %d files, %v", len(files), err)
		}
		if err := NormalizeRoot(root); err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(filepath.Join(dir, "sub", "b.txt")); string(got) != "b\n" {
			t.Errorf("sub/b.txt = %q, want normalized", got)
		}
	})
}

// A file replaced by a link or a FIFO after the walk saw a regular file is
// skipped by NormalizeRoot: the link is not followed (its target is neither
// read nor copied into the directory) and the FIFO does not block.
func TestNormalizeRoot_FileSwappedAfterTypeCheck(t *testing.T) {
	t.Run("link", func(t *testing.T) {
		dir := t.TempDir()
		writeReadTestFile(t, dir, "a.txt", "mine\r\n")
		outside := filepath.Join(t.TempDir(), "secret.txt")
		if err := os.WriteFile(outside, []byte("outside\r\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		root := openTestRoot(t, dir)
		p := filepath.Join(dir, "a.txt")
		hook := func(_, rel string) {
			if rel != "a.txt" {
				return
			}
			if err := os.Remove(p); err != nil {
				t.Error(err)
			}
			if err := os.Symlink(outside, p); err != nil {
				t.Error(err)
			}
		}
		normalizeBeforeRead.Store(&hook)
		t.Cleanup(func() { normalizeBeforeRead.Store(nil) })
		if err := NormalizeRoot(root); err != nil {
			t.Fatalf("NormalizeRoot: %v", err)
		}
		if fi, err := os.Lstat(p); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Errorf("a.txt is no longer the link (%v, %v): the link target was copied into the directory", fi, err)
		}
		if got, _ := os.ReadFile(outside); string(got) != "outside\r\n" {
			t.Errorf("the link target was changed: %q", got)
		}
	})
	t.Run("FIFO", func(t *testing.T) {
		dir := t.TempDir()
		writeReadTestFile(t, dir, "a.txt", "mine\r\n")
		root := openTestRoot(t, dir)
		p := filepath.Join(dir, "a.txt")
		hook := func(_, rel string) {
			if rel != "a.txt" {
				return
			}
			if err := os.Remove(p); err != nil {
				t.Error(err)
			}
			if err := syscall.Mkfifo(p, 0o600); err != nil {
				t.Errorf("mkfifo: %v", err)
			}
		}
		normalizeBeforeRead.Store(&hook)
		t.Cleanup(func() { normalizeBeforeRead.Store(nil) })
		within(t, func() {
			if err := NormalizeRoot(root); err != nil {
				t.Errorf("NormalizeRoot: %v", err)
			}
		})
	})
	t.Run("control", func(t *testing.T) {
		dir := t.TempDir()
		writeReadTestFile(t, dir, "a.txt", "mine\r\n")
		if err := os.Chmod(filepath.Join(dir, "a.txt"), 0o640); err != nil {
			t.Fatal(err)
		}
		if err := NormalizeRoot(openTestRoot(t, dir)); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Lstat(filepath.Join(dir, "a.txt"))
		if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o640 {
			t.Fatalf("a.txt after normalize: %v, %v; want a regular 0640 file", fi, err)
		}
		if got, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(got) != "mine\n" {
			t.Errorf("a.txt = %q, want normalized", got)
		}
		entries, _ := os.ReadDir(dir)
		if len(entries) != 1 {
			t.Errorf("leftover entries after normalize: %v", entries)
		}
	})
}

// NormalizeDir leaves a linked directory alone, as the walk by path did.
func TestNormalizeDir_LinkedDirectoryLeftAlone(t *testing.T) {
	target := t.TempDir()
	writeReadTestFile(t, target, "a.txt", "x\r\n")
	link := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := NormalizeDir(link); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(target, "a.txt")); string(got) != "x\r\n" {
		t.Errorf("the target of a linked directory was normalized: %q", got)
	}
}

// walkRoot follows fs.WalkDir's SkipDir and SkipAll rules: SkipDir from a
// directory skips its contents, SkipDir from a file skips the rest of its
// directory, and SkipAll ends the walk without an error.
func TestWalkRoot_SkipSemanticsMatchWalkDir(t *testing.T) {
	dir := t.TempDir()
	for _, rel := range []string{"a/x", "a/y", "a/z", "b/w", "c/v", "d"} {
		writeReadTestFile(t, dir, rel, rel)
	}
	for name, decide := range map[string]func(rel string) error{
		"SkipDir from a directory": func(rel string) error {
			if rel == "b" {
				return fs.SkipDir
			}
			return nil
		},
		"SkipDir from a file": func(rel string) error {
			if rel == "a/y" {
				return fs.SkipDir
			}
			return nil
		},
		"SkipAll": func(rel string) error {
			if rel == "b/w" {
				return fs.SkipAll
			}
			return nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			var want []string
			werr := fs.WalkDir(os.DirFS(dir), ".", func(rel string, d fs.DirEntry, err error) error {
				if err != nil || rel == "." {
					return err
				}
				want = append(want, rel)
				return decide(rel)
			})
			var got []string
			gerr := walkRoot(openTestRoot(t, dir), func(rel string, d fs.DirEntry) error {
				got = append(got, rel)
				return decide(rel)
			})
			if werr != nil || gerr != nil {
				t.Fatalf("errors: fs.WalkDir %v, walkRoot %v; want nil", werr, gerr)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("walkRoot visited %v, fs.WalkDir %v", got, want)
			}
		})
	}
}

// The temporary file name is retried on a collision (O_EXCL): an existing
// entry with the chosen name is never followed or overwritten, and the walk
// gives up after normalizeTempTries collisions.
func TestNormalizeRoot_TemporaryNameCollisionRetried(t *testing.T) {
	// setSuffixes makes the suffix function return names in turn (the last
	// one repeating) and returns a pointer to the number of calls.
	setSuffixes := func(t *testing.T, names ...string) *int {
		i := 0
		orig := normalizeTempSuffix
		normalizeTempSuffix = func() (string, error) {
			n := names[len(names)-1]
			if i < len(names) {
				n = names[i]
			}
			i++
			return n, nil
		}
		t.Cleanup(func() { normalizeTempSuffix = orig })
		return &i
	}
	t.Run("one collision", func(t *testing.T) {
		dir := t.TempDir()
		writeReadTestFile(t, dir, "a.txt", "x\r\n")
		writeReadTestFile(t, dir, ".scion-normalize.tmp-taken", "kept\n")
		setSuffixes(t, "taken", "free")
		if err := NormalizeRoot(openTestRoot(t, dir)); err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(got) != "x\n" {
			t.Errorf("a.txt = %q, want normalized", got)
		}
		if got, _ := os.ReadFile(filepath.Join(dir, ".scion-normalize.tmp-taken")); string(got) != "kept\n" {
			t.Errorf("the colliding entry was changed: %q", got)
		}
		if _, err := os.Lstat(filepath.Join(dir, ".scion-normalize.tmp-free")); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the temporary file was left behind: %v", err)
		}
	})
	t.Run("every name collides", func(t *testing.T) {
		dir := t.TempDir()
		writeReadTestFile(t, dir, "a.txt", "x\r\n")
		writeReadTestFile(t, dir, ".scion-normalize.tmp-taken", "kept\n")
		tries := setSuffixes(t, "taken")
		if err := NormalizeRoot(openTestRoot(t, dir)); !errors.Is(err, fs.ErrExist) {
			t.Errorf("err = %v, want the collision error after %d tries", err, normalizeTempTries)
		}
		if *tries != normalizeTempTries {
			t.Errorf("tried %d temporary names, want exactly %d", *tries, normalizeTempTries)
		}
		if got, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(got) != "x\r\n" {
			t.Errorf("a.txt = %q, want unchanged", got)
		}
	})
}

// NormalizeDir on a regular file normalizes that file (binary extensions
// excepted), as the walk by path did.
func TestNormalizeDir_RegularFile(t *testing.T) {
	dir := t.TempDir()
	writeReadTestFile(t, dir, "a.txt", "x\r\n")
	writeReadTestFile(t, dir, "b.png", "x\r\n")
	for _, n := range []string{"a.txt", "b.png"} {
		if err := NormalizeDir(filepath.Join(dir, n)); err != nil {
			t.Fatalf("%s: %v", n, err)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(got) != "x\n" {
		t.Errorf("a.txt = %q, want normalized", got)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "b.png")); string(got) != "x\r\n" {
		t.Errorf("b.png = %q, want unchanged (binary extension)", got)
	}
}
