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

package rootopen

import (
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestOpenRegularForRead_Policy(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a", "b", "file.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("file.txt", filepath.Join(dir, "a", "b", "leaf-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("b", filepath.Join(dir, "a", "dir-link")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(dir, "a", "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	sockPath := filepath.Join(dir, "sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	t.Run("regular_multi_component", func(t *testing.T) {
		f, info, err := OpenRegularForRead(root, "a/b/file.txt", nil)
		if err != nil {
			t.Fatalf("OpenRegularForRead: %v", err)
		}
		defer func() { _ = f.Close() }()
		if info.Size() != 5 {
			t.Fatalf("size = %d, want 5", info.Size())
		}
		got, err := io.ReadAll(f)
		if err != nil || string(got) != "hello" {
			t.Fatalf("read = %q, %v", got, err)
		}
	})

	t.Run("expected_same_file_mismatch", func(t *testing.T) {
		otherPath := filepath.Join(dir, "other.txt")
		if err := os.WriteFile(otherPath, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		otherInfo, err := os.Stat(otherPath)
		if err != nil {
			t.Fatal(err)
		}
		f, _, err := OpenRegularForRead(root, "a/b/file.txt", otherInfo)
		if !errors.Is(err, ErrRefused) {
			if f != nil {
				_ = f.Close()
			}
			t.Fatalf("err = %v, want ErrRefused", err)
		}
	})

	t.Run("leaf_symlink_refused", func(t *testing.T) {
		f, _, err := OpenRegularForRead(root, "a/b/leaf-link", nil)
		if !errors.Is(err, ErrRefused) {
			if f != nil {
				_ = f.Close()
			}
			t.Fatalf("err = %v, want ErrRefused", err)
		}
	})

	t.Run("intermediate_symlink_refused", func(t *testing.T) {
		f, _, err := OpenRegularForRead(root, "a/dir-link/file.txt", nil)
		if !errors.Is(err, ErrRefused) {
			if f != nil {
				_ = f.Close()
			}
			t.Fatalf("err = %v, want ErrRefused", err)
		}
	})

	t.Run("fifo_nonblocking_refused", func(t *testing.T) {
		done := make(chan error, 1)
		go func() {
			f, _, err := OpenRegularForRead(root, "a/pipe", nil)
			if f != nil {
				_ = f.Close()
			}
			done <- err
		}()
		select {
		case err := <-done:
			if !errors.Is(err, ErrRefused) {
				t.Fatalf("err = %v, want ErrRefused", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("OpenRegularForRead blocked on FIFO")
		}
	})

	t.Run("socket_refused", func(t *testing.T) {
		f, _, err := OpenRegularForRead(root, "sock", nil)
		if !errors.Is(err, ErrRefused) {
			if f != nil {
				_ = f.Close()
			}
			t.Fatalf("err = %v, want ErrRefused", err)
		}
	})

	t.Run("missing_returns_not_exist", func(t *testing.T) {
		_, _, err := OpenRegularForRead(root, "a/b/missing.txt", nil)
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("err = %v, want fs.ErrNotExist", err)
		}
	})

	t.Run("invalid_paths_refused", func(t *testing.T) {
		for _, bad := range []string{"", "/", "/a", "a/", "a//b", ".", "..", "a/../b", "a/./b", "a\x00b"} {
			if _, _, err := OpenRegularForRead(root, bad, nil); !errors.Is(err, ErrRefused) {
				t.Fatalf("path %q: err = %v, want ErrRefused", bad, err)
			}
		}
	})
}

func TestOpenFileForWrite_Policy(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(dir, "linkdir")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(dir, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	t.Run("create_new_and_excl_collision", func(t *testing.T) {
		f, err := OpenFileForWrite(root, "real/new.txt", 0o644, false)
		if err != nil {
			t.Fatalf("OpenFileForWrite: %v", err)
		}
		_, _ = f.WriteString("ok")
		_ = f.Close()

		_, err = OpenFileForWrite(root, "real/new.txt", 0o644, false)
		if !errors.Is(err, fs.ErrExist) {
			t.Fatalf("second excl create err = %v, want fs.ErrExist", err)
		}
	})

	t.Run("intermediate_symlink_refused", func(t *testing.T) {
		f, err := OpenFileForWrite(root, "linkdir/out.txt", 0o644, false)
		if !errors.Is(err, ErrRefused) {
			if f != nil {
				_ = f.Close()
			}
			t.Fatalf("err = %v, want ErrRefused", err)
		}
		if _, err := os.Stat(filepath.Join(dir, "real", "out.txt")); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("target dir should not have been written, stat err = %v", err)
		}
	})

	t.Run("fifo_nonblocking_refused", func(t *testing.T) {
		done := make(chan error, 1)
		go func() {
			f, err := OpenFileForWrite(root, "pipe", 0o644, true)
			if f != nil {
				_ = f.Close()
			}
			done <- err
		}()
		select {
		case err := <-done:
			if !errors.Is(err, ErrRefused) {
				t.Fatalf("err = %v, want ErrRefused", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("OpenFileForWrite blocked on FIFO")
		}
	})

	t.Run("fifo_with_reader_refused", func(t *testing.T) {
		reader, err := os.OpenFile(filepath.Join(dir, "pipe"), os.O_RDONLY|unix.O_NONBLOCK, 0)
		if err != nil {
			t.Fatalf("open fifo reader: %v", err)
		}
		defer func() { _ = reader.Close() }()

		done := make(chan error, 1)
		go func() {
			f, err := OpenFileForWrite(root, "pipe", 0o644, true)
			if f != nil {
				_ = f.Close()
			}
			done <- err
		}()
		select {
		case err := <-done:
			if !errors.Is(err, ErrRefused) {
				t.Fatalf("err = %v, want ErrRefused", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("OpenFileForWrite blocked on reader-held FIFO")
		}
	})
}

func TestMkdirAllAndDirOps_Policy(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real-target")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real-target", filepath.Join(dir, "link-top")); err != nil {
		t.Fatal(err)
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	t.Run("symlink_component_refused_without_mkdir", func(t *testing.T) {
		d, err := MkdirAll(root, "link-top/sub/leaf", 0o755)
		if d != nil {
			_ = d.Close()
		}
		if !errors.Is(err, ErrRefused) {
			t.Fatalf("MkdirAll through symlink err = %v, want ErrRefused", err)
		}
		entries, err := os.ReadDir(target)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("MkdirAll created entries inside symlink target: %v", entries)
		}
	})

	t.Run("create_and_unlink_pinned_dir", func(t *testing.T) {
		leafDir, err := MkdirAll(root, "a/b/c", 0o755)
		if err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		defer func() { _ = leafDir.Close() }()

		f, err := OpenFileForWriteInDir(leafDir, "file.txt", 0o644, false)
		if err != nil {
			t.Fatalf("OpenFileForWriteInDir: %v", err)
		}
		_, _ = f.WriteString("hello")
		_ = f.Close()

		// Collision with exists=false is refused with fs.ErrExist.
		if _, err := OpenFileForWriteInDir(leafDir, "file.txt", 0o644, false); !errors.Is(err, fs.ErrExist) {
			t.Fatalf("second excl create err = %v, want fs.ErrExist", err)
		}

		// Multi-component or invalid names are refused.
		for _, bad := range []string{"", ".", "..", "sub/file.txt", "a\x00b"} {
			if _, err := OpenFileForWriteInDir(leafDir, bad, 0o644, false); !errors.Is(err, ErrRefused) {
				t.Fatalf("OpenFileForWriteInDir(%q) err = %v, want ErrRefused", bad, err)
			}
			if err := UnlinkInDir(leafDir, bad); !errors.Is(err, ErrRefused) {
				t.Fatalf("UnlinkInDir(%q) err = %v, want ErrRefused", bad, err)
			}
		}

		// FIFO with an active reader is refused on exists=true.
		pipePath := filepath.Join(dir, "a", "b", "c", "pipe")
		if err := unix.Mkfifo(pipePath, 0o600); err != nil {
			t.Fatal(err)
		}
		reader, err := os.OpenFile(pipePath, os.O_RDONLY|unix.O_NONBLOCK, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = reader.Close() }()
		if f, err := OpenFileForWriteInDir(leafDir, "pipe", 0o644, true); !errors.Is(err, ErrRefused) {
			if f != nil {
				_ = f.Close()
			}
			t.Fatalf("OpenFileForWriteInDir(reader-held FIFO) err = %v, want ErrRefused", err)
		}

		// UnlinkInDir unlinks in the pinned dir, not through a swapped symlink.
		victim := filepath.Join(target, "b", "c", "file.txt")
		if err := os.MkdirAll(filepath.Dir(victim), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(victim, []byte("keep"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(filepath.Join(dir, "a"), filepath.Join(dir, "a.moved")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("real-target", filepath.Join(dir, "a")); err != nil {
			t.Fatal(err)
		}

		if err := UnlinkInDir(leafDir, "file.txt"); err != nil {
			t.Fatalf("UnlinkInDir: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, "a.moved", "b", "c", "file.txt")); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("pinned file should have been removed, stat err = %v", err)
		}
		got, err := os.ReadFile(victim)
		if err != nil || string(got) != "keep" {
			t.Fatalf("symlink target file was modified: %q, %v", got, err)
		}
	})
}

func TestOpenDir_Policy(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("sub", filepath.Join(dir, "linksub")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plain.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	if d, err := OpenDir(root, "."); err != nil {
		t.Fatalf("OpenDir('.'): %v", err)
	} else {
		_ = d.Close()
	}

	if d, err := OpenDir(root, "sub/nested"); err != nil {
		t.Fatalf("OpenDir('sub/nested'): %v", err)
	} else {
		_ = d.Close()
	}

	if d, err := OpenDir(root, "linksub"); !errors.Is(err, ErrRefused) {
		if d != nil {
			_ = d.Close()
		}
		t.Fatalf("OpenDir(symlink) err = %v, want ErrRefused", err)
	}

	if d, err := OpenDir(root, "linksub/nested"); !errors.Is(err, ErrRefused) {
		if d != nil {
			_ = d.Close()
		}
		t.Fatalf("OpenDir(intermediate symlink) err = %v, want ErrRefused", err)
	}

	if d, err := OpenDir(root, "plain.txt"); !errors.Is(err, ErrRefused) {
		if d != nil {
			_ = d.Close()
		}
		t.Fatalf("OpenDir(regular file) err = %v, want ErrRefused", err)
	}
}
