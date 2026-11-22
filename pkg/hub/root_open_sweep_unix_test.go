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

package hub

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/GoogleCloudPlatform/scion/pkg/util/rootopen"
)

func TestLocalDiskAttachmentStore_Get_OpenPolicy(t *testing.T) {
	base := t.TempDir()
	store, err := NewLocalDiskAttachmentStore(base)
	if err != nil {
		t.Fatalf("NewLocalDiskAttachmentStore: %v", err)
	}

	// Legitimate save and get.
	meta, err := store.Save(context.Background(), "proj-1", "note.txt", strings.NewReader("hello"), 5, "text/plain")
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	rc, gotMeta, err := store.Get(context.Background(), "proj-1", meta.ID)
	if err != nil {
		t.Fatalf("Get regular: %v", err)
	}
	body, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || string(body) != "hello" || gotMeta.Filename != "note.txt" {
		t.Fatalf("Get regular = (%q, %+v, %v)", body, gotMeta, err)
	}

	// In-root intermediate symlink for project dir must be refused.
	t.Run("in_root_intermediate_project_symlink_refused", func(t *testing.T) {
		if err := os.Symlink("proj-1", filepath.Join(base, "proj-link")); err != nil {
			t.Fatal(err)
		}
		rc, _, err := store.Get(context.Background(), "proj-link", meta.ID)
		if err == nil {
			_ = rc.Close()
			t.Fatal("Get succeeded through in-root project symlink")
		}
	})

	// In-root intermediate symlink for attachment id dir must be refused.
	t.Run("in_root_intermediate_id_symlink_refused", func(t *testing.T) {
		if err := os.Symlink(meta.ID, filepath.Join(base, "proj-1", "id-link")); err != nil {
			t.Fatal(err)
		}
		rc, _, err := store.Get(context.Background(), "proj-1", "id-link")
		if err == nil {
			_ = rc.Close()
			t.Fatal("Get succeeded through in-root id symlink")
		}
	})

	// In-root leaf symlink must be refused.
	t.Run("in_root_leaf_symlink_refused", func(t *testing.T) {
		attDir := filepath.Join(base, "proj-1", "leaf-sym-id")
		if err := os.MkdirAll(attDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("../"+meta.ID+"/note.txt", filepath.Join(attDir, "alias.txt")); err != nil {
			t.Fatal(err)
		}
		rc, _, err := store.Get(context.Background(), "proj-1", "leaf-sym-id")
		if err == nil {
			_ = rc.Close()
			t.Fatal("Get succeeded through in-root leaf symlink")
		}
		if !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("err = %v, want 'not a regular file'", err)
		}
	})

	// Swapped-in FIFO must not block and must be refused.
	t.Run("fifo_nonblocking_refused", func(t *testing.T) {
		attDir := filepath.Join(base, "proj-1", "fifo-id")
		if err := os.MkdirAll(attDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := unix.Mkfifo(filepath.Join(attDir, "pipe.txt"), 0o600); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			rc, _, err := store.Get(context.Background(), "proj-1", "fifo-id")
			if rc != nil {
				_ = rc.Close()
			}
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("Get succeeded on FIFO")
			}
			if !strings.Contains(err.Error(), "not a regular file") {
				t.Fatalf("err = %v, want 'not a regular file'", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Get blocked on FIFO")
		}
	})

	// Swapped-in leaf symlink via hook must be refused.
	t.Run("swapped_leaf_symlink_refused", func(t *testing.T) {
		attDir := filepath.Join(base, "proj-1", "swap-id")
		if err := os.MkdirAll(attDir, 0o755); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(attDir, "doc.txt")
		if err := os.WriteFile(target, []byte("ok"), 0o644); err != nil {
			t.Fatal(err)
		}
		restore := rootopen.SetBeforeOpenHookForTest(func(name string) {
			if strings.HasSuffix(name, "/doc.txt") {
				_ = os.Remove(target)
				_ = os.Symlink("../"+meta.ID+"/note.txt", target)
			}
		})
		defer restore()

		rc, _, err := store.Get(context.Background(), "proj-1", "swap-id")
		if err == nil {
			_ = rc.Close()
			t.Fatal("Get succeeded after leaf swap to symlink")
		}
		if !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("err = %v, want 'not a regular file'", err)
		}
	})
}

func TestReadRootSmallFile_OpenPolicy(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "project-id"), []byte("pid-123\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("project-id", filepath.Join(dir, "link-id")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(dir, "pipe-id"), 0o600); err != nil {
		t.Fatal(err)
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	got, err := readRootSmallFile(root, "project-id")
	if err != nil || got != "pid-123" {
		t.Fatalf("readRootSmallFile(regular) = (%q, %v)", got, err)
	}

	if _, err := readRootSmallFile(root, "missing"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("readRootSmallFile(missing) err = %v, want fs.ErrNotExist", err)
	}

	if _, err := readRootSmallFile(root, "link-id"); !errors.Is(err, rootopen.ErrRefused) {
		t.Fatalf("readRootSmallFile(symlink) err = %v, want rootopen.ErrRefused", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := readRootSmallFile(root, "pipe-id")
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, rootopen.ErrRefused) {
			t.Fatalf("readRootSmallFile(fifo) err = %v, want rootopen.ErrRefused", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("readRootSmallFile blocked on FIFO")
	}
}

func TestReadRootDir_OpenPolicy(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "real", "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(dir, "linkdir")); err != nil {
		t.Fatal(err)
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	entries, err := readRootDir(root, ".")
	if err != nil || len(entries) != 2 {
		t.Fatalf("readRootDir('.') = (%d, %v)", len(entries), err)
	}
	if _, err := readRootDir(root, "linkdir"); !errors.Is(err, rootopen.ErrRefused) {
		t.Fatalf("readRootDir(symlink) err = %v, want rootopen.ErrRefused", err)
	}

	// Also confirm sweepProjectionsIn ignores a symlinked .attachments pointing inside root.
	victim := filepath.Join(dir, "victim")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.WriteFile(filepath.Join(victim, "keep.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(filepath.Join(victim, "keep.txt"), old, old)
	if err := os.Symlink("victim", filepath.Join(dir, projectfs.AttachmentsDir)); err != nil {
		t.Fatal(err)
	}
	res := projectfs.Resolved{Backend: projectfs.BackendLocal, HostPath: dir}
	removed, err := sweepProjectionsIn(res, time.Now())
	if err != nil || removed != 0 {
		t.Fatalf("sweepProjectionsIn(symlink) = (%d, %v), want (0, nil)", removed, err)
	}
	if _, err := os.Stat(filepath.Join(victim, "keep.txt")); err != nil {
		t.Fatalf("victim file should not be removed: %v", err)
	}
}
