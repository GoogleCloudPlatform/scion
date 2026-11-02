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
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestOpenRegularFile(t *testing.T) {
	dir := t.TempDir()
	writeReadTestFile(t, dir, "a.txt", "hello\n")
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	// An in-root link to a regular file: os.Root would resolve it, so only
	// the Lstat check refuses it.
	if err := os.Symlink("a.txt", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "fifo"), 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	t.Run("regular file opens", func(t *testing.T) {
		f, fi, err := OpenRegularFile(root, "a.txt")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = f.Close() }()
		data, err := io.ReadAll(f)
		if err != nil || string(data) != "hello\n" || fi.Size() != 6 {
			t.Errorf("read %q (size %d), %v", data, fi.Size(), err)
		}
	})
	for _, tc := range []struct{ name, rel string }{
		{"in-root link is refused", "link"},
		{"FIFO is refused without blocking", "fifo"},
		{"directory is refused", "sub"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			within(t, func() {
				f, _, err := OpenRegularFile(root, tc.rel)
				if f != nil {
					_ = f.Close()
				}
				if !errors.Is(err, ErrNotRegularFile) {
					t.Errorf("err = %v, want ErrNotRegularFile", err)
				}
			})
		})
	}
	t.Run("missing file is not found", func(t *testing.T) {
		if _, _, err := OpenRegularFile(root, "missing"); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("err = %v, want fs.ErrNotExist", err)
		}
	})
}

// A FIFO swapped in between the Lstat and the open does not block the open
// (readOnlyFlags) and is refused by the check on the opened file.
func TestOpenRegularFile_FIFOSwappedInAfterLstat(t *testing.T) {
	dir := t.TempDir()
	writeReadTestFile(t, dir, "a.txt", "hello\n")
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	p := filepath.Join(dir, "a.txt")
	swap := func(string) {
		if err := os.Remove(p); err != nil {
			t.Error(err)
		}
		if err := syscall.Mkfifo(p, 0o600); err != nil {
			t.Errorf("mkfifo: %v", err)
		}
	}
	openRegularAfterLstat.Store(&swap)
	t.Cleanup(func() { openRegularAfterLstat.Store(nil) })
	within(t, func() {
		f, _, err := OpenRegularFile(root, "a.txt")
		if f != nil {
			_ = f.Close()
		}
		if !errors.Is(err, ErrNotRegularFile) {
			t.Errorf("err = %v, want ErrNotRegularFile", err)
		}
	})
}

// A regular file replaced by another regular file between the Lstat and the
// open is refused (SameFile).
func TestOpenRegularFile_ReplacedAfterLstat(t *testing.T) {
	dir := t.TempDir()
	writeReadTestFile(t, dir, "a.txt", "hello\n")
	writeReadTestFile(t, dir, "b.txt", "other\n")
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	swap := func(string) {
		if err := os.Rename(filepath.Join(dir, "b.txt"), filepath.Join(dir, "a.txt")); err != nil {
			t.Error(err)
		}
	}
	openRegularAfterLstat.Store(&swap)
	t.Cleanup(func() { openRegularAfterLstat.Store(nil) })
	f, _, err := OpenRegularFile(root, "a.txt")
	if f != nil {
		_ = f.Close()
	}
	if !errors.Is(err, ErrFileReplaced) {
		t.Errorf("err = %v, want ErrFileReplaced", err)
	}
}
