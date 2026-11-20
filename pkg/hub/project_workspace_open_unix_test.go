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
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
