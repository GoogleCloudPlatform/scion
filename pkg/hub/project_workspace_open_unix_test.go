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

//go:build unix && !no_sqlite

package hub

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func runWithinTimeout(t *testing.T, timeout time.Duration, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatalf("operation did not complete within %v (blocked on non-regular file)", timeout)
	}
}

func setupWorkspaceAndSharedDirForOpenTest(t *testing.T, srv *Server, name string) []struct {
	label      string
	dir        string
	filesURL   string
	archiveURL string
} {
	t.Helper()
	project, workspacePath := createTestHubManagedProject(t, srv, name)
	addSharedDirToProject(t, srv, project.ID, "scratch")
	sharedDirPath := resolveTestSharedDirPath(t, project, "scratch")
	require.NoError(t, os.MkdirAll(sharedDirPath, 0o755))

	return []struct {
		label      string
		dir        string
		filesURL   string
		archiveURL string
	}{
		{
			label:      "workspace",
			dir:        workspacePath,
			filesURL:   fmt.Sprintf("/api/v1/projects/%s/workspace/files", project.ID),
			archiveURL: fmt.Sprintf("/api/v1/projects/%s/workspace/archive", project.ID),
		},
		{
			label:      "shareddir",
			dir:        sharedDirPath,
			filesURL:   fmt.Sprintf("/api/v1/projects/%s/shared-dirs/scratch/files", project.ID),
			archiveURL: fmt.Sprintf("/api/v1/projects/%s/shared-dirs/scratch/archive", project.ID),
		},
	}
}

func TestOpenWorkspaceTarget_FIFODoesNotBlock(t *testing.T) {
	dir := t.TempDir()
	fifoPath := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(fifoPath, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer func() { _ = root.Close() }()

	runWithinTimeout(t, 2*time.Second, func() {
		f, openErr := openWorkspaceTarget(root, "pipe", true)
		if f != nil {
			_ = f.Close()
		}
		assert.ErrorIs(t, openErr, errWorkspaceWriteRefused)
	})
}

func TestOpenWorkspaceRegularForRead_SymlinkMultiComponentAndRegular(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ok.txt"), []byte("hello"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("secret"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub", "nested"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sub", "nested", "deep.txt"), []byte("deep-ok"), 0o644))
	require.NoError(t, os.Symlink("secret.txt", filepath.Join(dir, "link.txt")))
	require.NoError(t, os.Symlink("../secret.txt", filepath.Join(dir, "sub", "leaf-link.txt")))
	require.NoError(t, os.Symlink("sub", filepath.Join(dir, "symdir")))

	sockPath := filepath.Join(dir, "sock")
	ln, sockErr := net.Listen("unix", sockPath)
	if sockErr == nil {
		t.Cleanup(func() { _ = ln.Close() })
	}

	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer func() { _ = root.Close() }()

	t.Run("regular_single_component", func(t *testing.T) {
		f, info, err := openWorkspaceRegularForRead(root, "ok.txt", nil)
		require.NoError(t, err)
		require.NotNil(t, f)
		require.NotNil(t, info)
		defer func() { _ = f.Close() }()
		data, err := io.ReadAll(f)
		require.NoError(t, err)
		assert.Equal(t, "hello", string(data))
	})

	t.Run("regular_multi_component", func(t *testing.T) {
		expected, err := os.Lstat(filepath.Join(dir, "sub", "nested", "deep.txt"))
		require.NoError(t, err)
		f, info, err := openWorkspaceRegularForRead(root, "sub/nested/deep.txt", expected)
		require.NoError(t, err)
		require.NotNil(t, f)
		require.NotNil(t, info)
		defer func() { _ = f.Close() }()
		data, err := io.ReadAll(f)
		require.NoError(t, err)
		assert.Equal(t, "deep-ok", string(data))
	})

	t.Run("live_in_root_symlink_nil_expected", func(t *testing.T) {
		f, info, err := openWorkspaceRegularForRead(root, "link.txt", nil)
		if f != nil {
			_ = f.Close()
		}
		assert.Nil(t, f)
		assert.Nil(t, info)
		assert.ErrorIs(t, err, errWorkspaceWriteRefused)
	})

	t.Run("live_in_root_symlink_multi_component_leaf", func(t *testing.T) {
		f, info, err := openWorkspaceRegularForRead(root, "sub/leaf-link.txt", nil)
		if f != nil {
			_ = f.Close()
		}
		assert.Nil(t, f)
		assert.Nil(t, info)
		assert.ErrorIs(t, err, errWorkspaceWriteRefused)
	})

	t.Run("live_in_root_symlink_intermediate_dir", func(t *testing.T) {
		f, info, err := openWorkspaceRegularForRead(root, "symdir/nested/deep.txt", nil)
		if f != nil {
			_ = f.Close()
		}
		assert.Nil(t, f)
		assert.Nil(t, info)
		assert.ErrorIs(t, err, errWorkspaceWriteRefused)
	})

	t.Run("invalid_paths_refused", func(t *testing.T) {
		for _, bad := range []string{"", ".", "..", "/ok.txt", "sub/../ok.txt", "sub/", "sub//nested/deep.txt"} {
			f, info, err := openWorkspaceRegularForRead(root, bad, nil)
			if f != nil {
				_ = f.Close()
			}
			assert.Nil(t, f, "path %q", bad)
			assert.Nil(t, info, "path %q", bad)
			assert.ErrorIs(t, err, errWorkspaceWriteRefused, "path %q", bad)
		}
	})

	if sockErr == nil {
		t.Run("unix_socket_refused", func(t *testing.T) {
			runWithinTimeout(t, 2*time.Second, func() {
				f, info, err := openWorkspaceRegularForRead(root, "sock", nil)
				if f != nil {
					_ = f.Close()
				}
				assert.Nil(t, f)
				assert.Nil(t, info)
				assert.ErrorIs(t, err, errWorkspaceWriteRefused)
			})
		})
	}
}

func TestOpenWorkspaceFileForWrite_SymlinkMultiComponentAndRegular(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("git-secret"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "existing.txt"), []byte("before"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sub", "target.txt"), []byte("sub-before"), 0o644))
	require.NoError(t, os.Symlink(".git/config", filepath.Join(dir, "link.txt")))
	require.NoError(t, os.Symlink("sub", filepath.Join(dir, "symdir")))

	sockPath := filepath.Join(dir, "sock")
	ln, sockErr := net.Listen("unix", sockPath)
	if sockErr == nil {
		t.Cleanup(func() { _ = ln.Close() })
	}

	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer func() { _ = root.Close() }()

	t.Run("live_in_root_symlink_exists_true", func(t *testing.T) {
		f, err := openWorkspaceFileForWrite(root, "link.txt", true)
		if f != nil {
			_ = f.Close()
		}
		assert.Nil(t, f)
		assert.ErrorIs(t, err, errWorkspaceWriteRefused)
	})

	t.Run("live_in_root_symlink_exists_false", func(t *testing.T) {
		f, err := openWorkspaceFileForWrite(root, "link.txt", false)
		if f != nil {
			_ = f.Close()
		}
		assert.Nil(t, f)
		assert.ErrorIs(t, err, errWorkspaceWriteRefused)
	})

	t.Run("live_in_root_symlink_intermediate_dir", func(t *testing.T) {
		f, err := openWorkspaceFileForWrite(root, "symdir/target.txt", true)
		if f != nil {
			_ = f.Close()
		}
		assert.Nil(t, f)
		assert.ErrorIs(t, err, errWorkspaceWriteRefused)
	})

	t.Run("regular_existing_file", func(t *testing.T) {
		f, err := openWorkspaceFileForWrite(root, "existing.txt", true)
		require.NoError(t, err)
		require.NotNil(t, f)
		require.NoError(t, f.Truncate(0))
		_, err = f.WriteString("after")
		require.NoError(t, err)
		require.NoError(t, f.Close())
		got, err := os.ReadFile(filepath.Join(dir, "existing.txt"))
		require.NoError(t, err)
		assert.Equal(t, "after", string(got))
	})

	t.Run("regular_new_file", func(t *testing.T) {
		f, err := openWorkspaceFileForWrite(root, "created.txt", false)
		require.NoError(t, err)
		require.NotNil(t, f)
		_, err = f.WriteString("brand-new")
		require.NoError(t, err)
		require.NoError(t, f.Close())
		got, err := os.ReadFile(filepath.Join(dir, "created.txt"))
		require.NoError(t, err)
		assert.Equal(t, "brand-new", string(got))
	})

	if sockErr == nil {
		t.Run("unix_socket_refused", func(t *testing.T) {
			runWithinTimeout(t, 2*time.Second, func() {
				f, err := openWorkspaceFileForWrite(root, "sock", true)
				if f != nil {
					_ = f.Close()
				}
				assert.Nil(t, f)
				assert.ErrorIs(t, err, errWorkspaceWriteRefused)
			})
		})
	}
}

func TestWorkspaceDownload_NonRegularRefused(t *testing.T) {
	srv, _ := testServer(t)
	targets := setupWorkspaceAndSharedDirForOpenTest(t, srv, "Open Download NonRegular")

	for _, tgt := range targets {
		t.Run(tgt.label, func(t *testing.T) {
			require.NoError(t, os.WriteFile(filepath.Join(tgt.dir, "ok.txt"), []byte("hello"), 0o644))
			fifoPath := filepath.Join(tgt.dir, "pipe")
			if err := syscall.Mkfifo(fifoPath, 0o600); err != nil {
				t.Skipf("mkfifo: %v", err)
			}

			for _, suffix := range []string{"", "?view=true", "?format=json"} {
				t.Run("fifo"+suffix, func(t *testing.T) {
					runWithinTimeout(t, 2*time.Second, func() {
						rec := doRequest(t, srv, http.MethodGet, tgt.filesURL+"/pipe"+suffix, nil)
						assert.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
					})
				})
			}

			rec := doRequest(t, srv, http.MethodGet, tgt.filesURL+"/ok.txt", nil)
			require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
			assert.Equal(t, "hello", rec.Body.String())
		})
	}
}

func TestWorkspaceDownload_SwappedFIFORefused(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestHubManagedProject(t, srv, "Open Download Swap FIFO")
	filesURL := fmt.Sprintf("/api/v1/projects/%s/workspace/files", project.ID)

	for _, suffix := range []string{"", "?format=json"} {
		t.Run("swap"+suffix, func(t *testing.T) {
			targetPath := filepath.Join(workspacePath, "target.txt")
			_ = os.Remove(targetPath)
			require.NoError(t, os.WriteFile(targetPath, []byte("regular"), 0o644))

			swap := func(name string) {
				if name != "target.txt" {
					return
				}
				_ = os.Remove(targetPath)
				_ = syscall.Mkfifo(targetPath, 0o600)
			}
			workspaceBeforeOpenHook.Store(&swap)
			t.Cleanup(func() { workspaceBeforeOpenHook.Store(nil) })

			runWithinTimeout(t, 2*time.Second, func() {
				rec := doRequest(t, srv, http.MethodGet, filesURL+"/target.txt"+suffix, nil)
				assert.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
			})
			workspaceBeforeOpenHook.Store(nil)
		})
	}
}

func TestWorkspaceDownload_SwappedSymlinkRefused(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestHubManagedProject(t, srv, "Open Download Swap Symlink")
	plantDotGitReadTree(t, workspacePath)
	filesURL := fmt.Sprintf("/api/v1/projects/%s/workspace/files", project.ID)

	targetPath := filepath.Join(workspacePath, "swap.txt")
	require.NoError(t, os.WriteFile(targetPath, []byte("regular"), 0o644))

	swap := func(name string) {
		if name != "swap.txt" {
			return
		}
		_ = os.Remove(targetPath)
		_ = os.Symlink(".git/config", targetPath)
	}
	workspaceBeforeOpenHook.Store(&swap)
	t.Cleanup(func() { workspaceBeforeOpenHook.Store(nil) })

	rec := doRequest(t, srv, http.MethodGet, filesURL+"/swap.txt", nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), dotGitReadSecret)
}

func TestWorkspaceArchive_NonRegularAndSwappedEntriesSkipped(t *testing.T) {
	srv, _ := testServer(t)
	targets := setupWorkspaceAndSharedDirForOpenTest(t, srv, "Open Archive Hardened")

	readZipEntries := func(t *testing.T, body []byte) map[string]string {
		t.Helper()
		zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
		require.NoError(t, err)
		entries := make(map[string]string, len(zr.File))
		for _, f := range zr.File {
			rc, err := f.Open()
			require.NoError(t, err)
			data, err := io.ReadAll(rc)
			require.NoError(t, rc.Close())
			require.NoError(t, err)
			entries[f.Name] = string(data)
		}
		return entries
	}

	for _, tgt := range targets {
		t.Run("steady_fifo_"+tgt.label, func(t *testing.T) {
			require.NoError(t, os.WriteFile(filepath.Join(tgt.dir, "keep.txt"), []byte("kept"), 0o644))
			if err := syscall.Mkfifo(filepath.Join(tgt.dir, "pipe"), 0o600); err != nil {
				t.Skipf("mkfifo: %v", err)
			}

			runWithinTimeout(t, 2*time.Second, func() {
				rec := doRequest(t, srv, http.MethodGet, tgt.archiveURL, nil)
				require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
				entries := readZipEntries(t, rec.Body.Bytes())
				assert.Equal(t, "kept", entries["keep.txt"])
				assert.NotContains(t, entries, "pipe")
			})
		})
	}

	t.Run("swapped_fifo_and_symlink", func(t *testing.T) {
		project, workspacePath := createTestHubManagedProject(t, srv, "Open Archive Swap")
		plantDotGitReadTree(t, workspacePath)
		archiveURL := fmt.Sprintf("/api/v1/projects/%s/workspace/archive", project.ID)

		fifoTarget := filepath.Join(workspacePath, "swap-fifo.txt")
		linkTarget := filepath.Join(workspacePath, "swap-link.txt")
		require.NoError(t, os.WriteFile(fifoTarget, []byte("before-fifo"), 0o644))
		require.NoError(t, os.WriteFile(linkTarget, []byte("before-link"), 0o644))

		swap := func(name string) {
			switch name {
			case "swap-fifo.txt":
				_ = os.Remove(fifoTarget)
				_ = syscall.Mkfifo(fifoTarget, 0o600)
			case "swap-link.txt":
				_ = os.Remove(linkTarget)
				_ = os.Symlink(".git/config", linkTarget)
			}
		}
		workspaceBeforeOpenHook.Store(&swap)
		t.Cleanup(func() { workspaceBeforeOpenHook.Store(nil) })

		runWithinTimeout(t, 2*time.Second, func() {
			rec := doRequest(t, srv, http.MethodGet, archiveURL, nil)
			require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
			entries := readZipEntries(t, rec.Body.Bytes())
			assert.Equal(t, "control", entries["README.md"])
			assert.NotContains(t, entries, "swap-fifo.txt")
			assert.NotContains(t, entries, "swap-link.txt")
			for name, content := range entries {
				assert.NotContains(t, content, dotGitReadSecret, "archive entry %q contains .git/config content", name)
			}
		})
	})

	t.Run("swapped_intermediate_dir_symlink", func(t *testing.T) {
		project, workspacePath := createTestHubManagedProject(t, srv, "Open Archive Swap Subdir")
		plantDotGitReadTree(t, workspacePath)
		archiveURL := fmt.Sprintf("/api/v1/projects/%s/workspace/archive", project.ID)

		require.NoError(t, os.MkdirAll(filepath.Join(workspacePath, "keepdir"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(workspacePath, "keepdir", "ok.txt"), []byte("nested-kept"), 0o644))
		subDir := filepath.Join(workspacePath, "swapdir")
		require.NoError(t, os.MkdirAll(subDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(subDir, "config"), []byte("regular-nested"), 0o644))

		swap := func(name string) {
			if name != "swapdir/config" {
				return
			}
			_ = os.RemoveAll(subDir)
			_ = os.Symlink(".git", subDir)
		}
		workspaceBeforeOpenHook.Store(&swap)
		t.Cleanup(func() { workspaceBeforeOpenHook.Store(nil) })

		runWithinTimeout(t, 2*time.Second, func() {
			rec := doRequest(t, srv, http.MethodGet, archiveURL, nil)
			require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
			entries := readZipEntries(t, rec.Body.Bytes())
			assert.Equal(t, "nested-kept", entries["keepdir/ok.txt"])
			assert.NotContains(t, entries, "swapdir/config")
			for name, content := range entries {
				assert.NotContains(t, content, dotGitReadSecret, "archive entry %q contains .git/config content", name)
			}
		})
	})
}

func TestWorkspaceWrite_SwappedFIFORefused(t *testing.T) {
	srv, _ := testServer(t)
	targets := setupWorkspaceAndSharedDirForOpenTest(t, srv, "Open Write Hardened")

	for _, tgt := range targets {
		t.Run("steady_fifo_"+tgt.label, func(t *testing.T) {
			fifoPath := filepath.Join(tgt.dir, "pipe")
			if err := syscall.Mkfifo(fifoPath, 0o600); err != nil {
				t.Skipf("mkfifo: %v", err)
			}
			runWithinTimeout(t, 2*time.Second, func() {
				rec := doRequest(t, srv, http.MethodPut, tgt.filesURL+"/pipe",
					ProjectWorkspaceWriteRequest{Content: "nope"})
				assert.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
			})
		})
	}

	t.Run("swapped_fifo_no_reader", func(t *testing.T) {
		project, workspacePath := createTestHubManagedProject(t, srv, "Open Write Swap NoReader")
		filesURL := fmt.Sprintf("/api/v1/projects/%s/workspace/files", project.ID)
		targetPath := filepath.Join(workspacePath, "target.txt")
		require.NoError(t, os.WriteFile(targetPath, []byte("orig"), 0o644))

		swap := func(name string) {
			if name != "target.txt" {
				return
			}
			_ = os.Remove(targetPath)
			_ = syscall.Mkfifo(targetPath, 0o600)
		}
		workspaceBeforeOpenHook.Store(&swap)
		t.Cleanup(func() { workspaceBeforeOpenHook.Store(nil) })

		runWithinTimeout(t, 2*time.Second, func() {
			rec := doRequest(t, srv, http.MethodPut, filesURL+"/target.txt",
				ProjectWorkspaceWriteRequest{Content: "new"})
			assert.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
		})
	})

	t.Run("swapped_fifo_with_reader", func(t *testing.T) {
		project, workspacePath := createTestHubManagedProject(t, srv, "Open Write Swap WithReader")
		filesURL := fmt.Sprintf("/api/v1/projects/%s/workspace/files", project.ID)
		targetPath := filepath.Join(workspacePath, "target.txt")
		require.NoError(t, os.WriteFile(targetPath, []byte("orig"), 0o644))

		var readerFile *os.File
		t.Cleanup(func() {
			if readerFile != nil {
				_ = readerFile.Close()
			}
		})

		swap := func(name string) {
			if name != "target.txt" {
				return
			}
			_ = os.Remove(targetPath)
			if err := syscall.Mkfifo(targetPath, 0o600); err != nil {
				return
			}
			// Open a non-blocking read end so O_WRONLY|O_NONBLOCK succeeds and
			// the post-open fstat check must reject the descriptor.
			f, err := os.OpenFile(targetPath, os.O_RDONLY|syscall.O_NONBLOCK, 0)
			if err == nil {
				readerFile = f
			}
		}
		workspaceBeforeOpenHook.Store(&swap)
		t.Cleanup(func() { workspaceBeforeOpenHook.Store(nil) })

		runWithinTimeout(t, 2*time.Second, func() {
			rec := doRequest(t, srv, http.MethodPut, filesURL+"/target.txt",
				ProjectWorkspaceWriteRequest{Content: "should-not-write"})
			assert.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
		})

		require.NotNil(t, readerFile)
		buf := make([]byte, 64)
		n, _ := readerFile.Read(buf)
		assert.Equal(t, 0, n, "unexpected bytes written to FIFO: %q", string(buf[:n]))
	})
}

func TestWorkspaceWrite_OpenErrnoMapsToBadRequest(t *testing.T) {
	t.Run("openAtInRoot_wraps_errno_in_path_error", func(t *testing.T) {
		dir := t.TempDir()
		root, err := os.OpenRoot(dir)
		require.NoError(t, err)
		defer func() { _ = root.Close() }()

		f, err := openAtInRoot(root, "missing.txt", unix.O_RDONLY, 0)
		if f != nil {
			_ = f.Close()
		}
		require.Error(t, err)
		var pathErr *fs.PathError
		assert.ErrorAs(t, err, &pathErr)
		assert.ErrorIs(t, err, fs.ErrNotExist)
	})

	t.Run("writeWorkspaceFileError_maps_eacces_path_error_to_400", func(t *testing.T) {
		rec := httptest.NewRecorder()
		writeWorkspaceFileError(rec, "workspace", "readonly.txt", &fs.PathError{
			Op:   "openat",
			Path: "readonly.txt",
			Err:  unix.EACCES,
		})
		assert.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	})

	t.Run("concurrently_removed_file_returns_400", func(t *testing.T) {
		srv, _ := testServer(t)
		project, workspacePath := createTestHubManagedProject(t, srv, "Open Write Concurrent Remove")
		filesURL := fmt.Sprintf("/api/v1/projects/%s/workspace/files", project.ID)
		targetPath := filepath.Join(workspacePath, "vanish.txt")
		require.NoError(t, os.WriteFile(targetPath, []byte("before"), 0o644))

		unlink := func(name string) {
			if name == "vanish.txt" {
				_ = os.Remove(targetPath)
			}
		}
		workspaceBeforeOpenHook.Store(&unlink)
		t.Cleanup(func() { workspaceBeforeOpenHook.Store(nil) })

		rec := doRequest(t, srv, http.MethodPut, filesURL+"/vanish.txt",
			ProjectWorkspaceWriteRequest{Content: "after"})
		assert.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	})

	t.Run("readonly_file_returns_400", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("running as root bypasses read-only file mode")
		}
		srv, _ := testServer(t)
		project, workspacePath := createTestHubManagedProject(t, srv, "Open Write Readonly")
		filesURL := fmt.Sprintf("/api/v1/projects/%s/workspace/files", project.ID)
		targetPath := filepath.Join(workspacePath, "readonly.txt")
		require.NoError(t, os.WriteFile(targetPath, []byte("before"), 0o444))
		t.Cleanup(func() { _ = os.Chmod(targetPath, 0o644) })

		rec := doRequest(t, srv, http.MethodPut, filesURL+"/readonly.txt",
			ProjectWorkspaceWriteRequest{Content: "after"})
		assert.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	})
}

func TestSnapshotWorkspaceDir_OpenHardening(t *testing.T) {
	t.Run("regular_files_and_steady_specials", func(t *testing.T) {
		srv, _ := testServer(t)
		project, workspacePath := createTestHubManagedProject(t, srv, "Snapshot Regular And Specials")
		src := filepath.Join(workspacePath, "import-src")
		require.NoError(t, os.MkdirAll(filepath.Join(src, "sub", "nested"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(src, "top.txt"), []byte("top-ok"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(src, "sub", "nested", "deep.txt"), []byte("deep-ok"), 0o644))
		require.NoError(t, os.Symlink("top.txt", filepath.Join(src, "link.txt")))
		if err := syscall.Mkfifo(filepath.Join(src, "pipe"), 0o600); err != nil {
			t.Skipf("mkfifo: %v", err)
		}

		runWithinTimeout(t, 2*time.Second, func() {
			dst, cleanup, err := srv.snapshotWorkspaceDir(context.Background(), project, "/import-src")
			require.NoError(t, err)
			defer cleanup()

			gotTop, err := os.ReadFile(filepath.Join(dst, "top.txt"))
			require.NoError(t, err)
			assert.Equal(t, "top-ok", string(gotTop))

			gotDeep, err := os.ReadFile(filepath.Join(dst, "sub", "nested", "deep.txt"))
			require.NoError(t, err)
			assert.Equal(t, "deep-ok", string(gotDeep))

			_, err = os.Lstat(filepath.Join(dst, "link.txt"))
			assert.ErrorIs(t, err, fs.ErrNotExist)
			_, err = os.Lstat(filepath.Join(dst, "pipe"))
			assert.ErrorIs(t, err, fs.ErrNotExist)
		})
	})

	t.Run("swapped_in_root_symlink_leaf_skipped", func(t *testing.T) {
		srv, _ := testServer(t)
		project, workspacePath := createTestHubManagedProject(t, srv, "Snapshot Swap Leaf Symlink")
		src := filepath.Join(workspacePath, "import-src")
		require.NoError(t, os.MkdirAll(filepath.Join(src, "sub"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(src, "keep.txt"), []byte("kept"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(src, "other.txt"), []byte("other-content"), 0o644))
		leafTop := filepath.Join(src, "swap-top.txt")
		leafNested := filepath.Join(src, "sub", "swap-nested.txt")
		require.NoError(t, os.WriteFile(leafTop, []byte("orig-top"), 0o644))
		require.NoError(t, os.WriteFile(leafNested, []byte("orig-nested"), 0o644))

		swap := func(name string) {
			switch name {
			case "swap-top.txt":
				_ = os.Remove(leafTop)
				_ = os.Symlink("other.txt", leafTop)
			case "sub/swap-nested.txt":
				_ = os.Remove(leafNested)
				_ = os.Symlink("../other.txt", leafNested)
			}
		}
		resourceImportBeforeOpenHook.Store(&swap)
		t.Cleanup(func() { resourceImportBeforeOpenHook.Store(nil) })

		dst, cleanup, err := srv.snapshotWorkspaceDir(context.Background(), project, "/import-src")
		require.NoError(t, err)
		defer cleanup()

		gotKeep, err := os.ReadFile(filepath.Join(dst, "keep.txt"))
		require.NoError(t, err)
		assert.Equal(t, "kept", string(gotKeep))
		_, err = os.Lstat(filepath.Join(dst, "swap-top.txt"))
		assert.ErrorIs(t, err, fs.ErrNotExist, "swapped single-component symlink must be skipped")
		_, err = os.Lstat(filepath.Join(dst, "sub", "swap-nested.txt"))
		assert.ErrorIs(t, err, fs.ErrNotExist, "swapped multi-component symlink must be skipped")
	})

	t.Run("swapped_in_root_symlink_intermediate_dir_skipped", func(t *testing.T) {
		srv, _ := testServer(t)
		project, workspacePath := createTestHubManagedProject(t, srv, "Snapshot Swap Dir Symlink")
		src := filepath.Join(workspacePath, "import-src")
		swapDir := filepath.Join(src, "swapdir")
		otherDir := filepath.Join(src, "otherdir")
		require.NoError(t, os.MkdirAll(swapDir, 0o755))
		require.NoError(t, os.MkdirAll(otherDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(src, "keep.txt"), []byte("kept"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(otherDir, "entry.txt"), []byte("other-dir-content"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(swapDir, "entry.txt"), []byte("orig"), 0o644))

		swap := func(name string) {
			if name != "swapdir/entry.txt" {
				return
			}
			_ = os.RemoveAll(swapDir)
			_ = os.Symlink("otherdir", swapDir)
		}
		resourceImportBeforeOpenHook.Store(&swap)
		t.Cleanup(func() { resourceImportBeforeOpenHook.Store(nil) })

		dst, cleanup, err := srv.snapshotWorkspaceDir(context.Background(), project, "/import-src")
		require.NoError(t, err)
		defer cleanup()

		gotKeep, err := os.ReadFile(filepath.Join(dst, "keep.txt"))
		require.NoError(t, err)
		assert.Equal(t, "kept", string(gotKeep))
		_, err = os.Lstat(filepath.Join(dst, "swapdir", "entry.txt"))
		assert.ErrorIs(t, err, fs.ErrNotExist, "swapped intermediate directory symlink must be skipped")
	})

	t.Run("swapped_fifo_does_not_block", func(t *testing.T) {
		srv, _ := testServer(t)
		project, workspacePath := createTestHubManagedProject(t, srv, "Snapshot Swap FIFO")
		src := filepath.Join(workspacePath, "import-src")
		require.NoError(t, os.MkdirAll(src, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(src, "keep.txt"), []byte("kept"), 0o644))
		fifoTarget := filepath.Join(src, "swap-fifo.txt")
		require.NoError(t, os.WriteFile(fifoTarget, []byte("orig"), 0o644))

		swap := func(name string) {
			if name != "swap-fifo.txt" {
				return
			}
			_ = os.Remove(fifoTarget)
			_ = syscall.Mkfifo(fifoTarget, 0o600)
		}
		resourceImportBeforeOpenHook.Store(&swap)
		t.Cleanup(func() { resourceImportBeforeOpenHook.Store(nil) })

		runWithinTimeout(t, 2*time.Second, func() {
			dst, cleanup, err := srv.snapshotWorkspaceDir(context.Background(), project, "/import-src")
			require.NoError(t, err)
			defer cleanup()

			gotKeep, err := os.ReadFile(filepath.Join(dst, "keep.txt"))
			require.NoError(t, err)
			assert.Equal(t, "kept", string(gotKeep))
			_, err = os.Lstat(filepath.Join(dst, "swap-fifo.txt"))
			assert.ErrorIs(t, err, fs.ErrNotExist)
		})
	})

	t.Run("swapped_unix_socket_skipped", func(t *testing.T) {
		srv, _ := testServer(t)
		project, workspacePath := createTestHubManagedProject(t, srv, "Snapshot Swap Socket")
		src := filepath.Join(workspacePath, "import-src")
		require.NoError(t, os.MkdirAll(src, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(src, "keep.txt"), []byte("kept"), 0o644))
		sockTarget := filepath.Join(src, "swap-sock")
		require.NoError(t, os.WriteFile(sockTarget, []byte("orig"), 0o644))

		var ln net.Listener
		t.Cleanup(func() {
			if ln != nil {
				_ = ln.Close()
			}
		})
		var sockErr error
		swap := func(name string) {
			if name != "swap-sock" {
				return
			}
			_ = os.Remove(sockTarget)
			ln, sockErr = net.Listen("unix", sockTarget)
		}
		resourceImportBeforeOpenHook.Store(&swap)
		t.Cleanup(func() { resourceImportBeforeOpenHook.Store(nil) })

		runWithinTimeout(t, 2*time.Second, func() {
			dst, cleanup, err := srv.snapshotWorkspaceDir(context.Background(), project, "/import-src")
			if sockErr != nil {
				t.Skipf("unix socket listen: %v", sockErr)
			}
			require.NoError(t, err)
			defer cleanup()

			gotKeep, err := os.ReadFile(filepath.Join(dst, "keep.txt"))
			require.NoError(t, err)
			assert.Equal(t, "kept", string(gotKeep))
			_, err = os.Lstat(filepath.Join(dst, "swap-sock"))
			assert.ErrorIs(t, err, fs.ErrNotExist)
		})
	})
}

func TestWriteRootFile_SwappedSymlinkAndSpecialFile(t *testing.T) {
	t.Run("swapped_in_root_symlink_refused_in_kernel", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "sibling"), []byte("keep-sibling\n"), 0o644))
		targetPath := filepath.Join(dir, "target")
		require.NoError(t, os.WriteFile(targetPath, []byte("orig\n"), 0o644))

		root, err := os.OpenRoot(dir)
		require.NoError(t, err)
		defer func() { _ = root.Close() }()

		swap := func(name string) {
			if name != "target" {
				return
			}
			_ = os.Remove(targetPath)
			_ = os.Symlink("sibling", targetPath)
		}
		rootFileBeforeOpenHook.Store(&swap)
		t.Cleanup(func() { rootFileBeforeOpenHook.Store(nil) })

		err = writeRootFile(root, "target", "new-val\n")
		require.Error(t, err)
		assert.ErrorIs(t, err, unix.ELOOP, "in-root symlink must be refused directly by openat(O_NOFOLLOW)")

		gotSibling, err := os.ReadFile(filepath.Join(dir, "sibling"))
		require.NoError(t, err)
		assert.Equal(t, "keep-sibling\n", string(gotSibling))

		fi, err := os.Lstat(targetPath)
		require.NoError(t, err)
		assert.NotZero(t, fi.Mode()&os.ModeSymlink)

		entries, err := os.ReadDir(dir)
		require.NoError(t, err)
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		assert.ElementsMatch(t, []string{"sibling", "target"}, names, "no temp file left behind")
	})

	t.Run("swapped_fifo_no_reader_does_not_block", func(t *testing.T) {
		dir := t.TempDir()
		targetPath := filepath.Join(dir, "target")
		require.NoError(t, os.WriteFile(targetPath, []byte("orig\n"), 0o644))

		root, err := os.OpenRoot(dir)
		require.NoError(t, err)
		defer func() { _ = root.Close() }()

		swap := func(name string) {
			if name != "target" {
				return
			}
			_ = os.Remove(targetPath)
			_ = syscall.Mkfifo(targetPath, 0o600)
		}
		rootFileBeforeOpenHook.Store(&swap)
		t.Cleanup(func() { rootFileBeforeOpenHook.Store(nil) })

		runWithinTimeout(t, 2*time.Second, func() {
			err := writeRootFile(root, "target", "new-val\n")
			require.Error(t, err)
		})

		entries, err := os.ReadDir(dir)
		require.NoError(t, err)
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		assert.ElementsMatch(t, []string{"target"}, names, "no temp file left behind")
	})

	t.Run("swapped_fifo_with_reader_refused_without_writing", func(t *testing.T) {
		dir := t.TempDir()
		targetPath := filepath.Join(dir, "target")
		require.NoError(t, os.WriteFile(targetPath, []byte("orig\n"), 0o644))

		root, err := os.OpenRoot(dir)
		require.NoError(t, err)
		defer func() { _ = root.Close() }()

		var readerFile *os.File
		t.Cleanup(func() {
			if readerFile != nil {
				_ = readerFile.Close()
			}
		})

		swap := func(name string) {
			if name != "target" {
				return
			}
			_ = os.Remove(targetPath)
			if err := syscall.Mkfifo(targetPath, 0o600); err != nil {
				return
			}
			f, err := os.OpenFile(targetPath, os.O_RDONLY|syscall.O_NONBLOCK, 0)
			if err == nil {
				readerFile = f
			}
		}
		rootFileBeforeOpenHook.Store(&swap)
		t.Cleanup(func() { rootFileBeforeOpenHook.Store(nil) })

		runWithinTimeout(t, 2*time.Second, func() {
			err := writeRootFile(root, "target", "new-val\n")
			require.Error(t, err)
		})

		require.NotNil(t, readerFile)
		buf := make([]byte, 64)
		n, _ := readerFile.Read(buf)
		assert.Equal(t, 0, n, "unexpected bytes written to FIFO")
	})

	t.Run("regular_create_and_replace", func(t *testing.T) {
		dir := t.TempDir()
		root, err := os.OpenRoot(dir)
		require.NoError(t, err)
		defer func() { _ = root.Close() }()

		require.NoError(t, writeRootFile(root, "item", "first\n"))
		got, err := os.ReadFile(filepath.Join(dir, "item"))
		require.NoError(t, err)
		assert.Equal(t, "first\n", string(got))

		require.NoError(t, writeRootFile(root, "item", "second\n"))
		got, err = os.ReadFile(filepath.Join(dir, "item"))
		require.NoError(t, err)
		assert.Equal(t, "second\n", string(got))
	})

	t.Run("multi_component_create_and_replace", func(t *testing.T) {
		dir := t.TempDir()
		nestedDir := filepath.Join(dir, "sub", "nested")
		require.NoError(t, os.MkdirAll(nestedDir, 0o755))

		root, err := os.OpenRoot(dir)
		require.NoError(t, err)
		defer func() { _ = root.Close() }()

		require.NoError(t, writeRootFile(root, "sub/nested/item", "first\n"))
		got, err := os.ReadFile(filepath.Join(nestedDir, "item"))
		require.NoError(t, err)
		assert.Equal(t, "first\n", string(got))

		require.NoError(t, writeRootFile(root, "sub/nested/item", "second\n"))
		got, err = os.ReadFile(filepath.Join(nestedDir, "item"))
		require.NoError(t, err)
		assert.Equal(t, "second\n", string(got))

		entries, err := os.ReadDir(nestedDir)
		require.NoError(t, err)
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		assert.ElementsMatch(t, []string{"item"}, names, "no temp file left behind in nested directory")
	})

	t.Run("multi_component_symlink_dir_refused", func(t *testing.T) {
		dir := t.TempDir()
		subDir := filepath.Join(dir, "sub")
		require.NoError(t, os.MkdirAll(subDir, 0o755))
		require.NoError(t, os.Symlink("sub", filepath.Join(dir, "symdir")))

		root, err := os.OpenRoot(dir)
		require.NoError(t, err)
		defer func() { _ = root.Close() }()

		err = writeRootFile(root, "symdir/item", "val\n")
		require.Error(t, err)
		assert.True(t, errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR), "expected ELOOP or ENOTDIR, got: %v", err)

		entries, err := os.ReadDir(subDir)
		require.NoError(t, err)
		assert.Empty(t, entries, "no temp file created through symlinked directory")
	})

	t.Run("invalid_leaf_paths_refused", func(t *testing.T) {
		dir := t.TempDir()
		subDir := filepath.Join(dir, "sub")
		require.NoError(t, os.MkdirAll(subDir, 0o755))

		root, err := os.OpenRoot(dir)
		require.NoError(t, err)
		defer func() { _ = root.Close() }()

		for _, bad := range []string{"", ".", "..", "sub/", "sub/.", "sub/.."} {
			err := writeRootFile(root, bad, "val\n")
			assert.Error(t, err, "path %q should be refused", bad)
		}

		entries, err := os.ReadDir(subDir)
		require.NoError(t, err)
		assert.Empty(t, entries, "no temp file left behind in sub")
	})
}
