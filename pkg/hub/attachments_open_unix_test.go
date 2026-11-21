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
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestIngestAgentAttachments_OpenRegularPolicy(t *testing.T) {
	t.Run("regular_single_and_multi_component", func(t *testing.T) {
		srv, _, project, sharedDir := agentAttachmentServer(t)
		ctx := context.Background()

		require.NoError(t, os.WriteFile(filepath.Join(sharedDir, "top.txt"), []byte("top"), 0o644))
		nestedDir := filepath.Join(sharedDir, ".attachments", "sender", "msg1", "deep")
		require.NoError(t, os.MkdirAll(nestedDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(nestedDir, "notes.md"), []byte("nested"), 0o644))

		refs, warnings := srv.ingestAgentAttachments(ctx, project.ID, "agent-1", []string{
			"/scion-volumes/" + attachmentSharedDirName + "/top.txt",
			"/scion-volumes/" + attachmentSharedDirName + "/.attachments/sender/msg1/deep/notes.md",
		})
		require.Empty(t, warnings)
		require.Len(t, refs, 2)
		assert.Equal(t, "top.txt", refs[0].Name)
		assert.Equal(t, "notes.md", refs[1].Name)

		reader, _, err := srv.attachmentStore.Get(ctx, project.ID, refs[1].ID)
		require.NoError(t, err)
		defer func() { _ = reader.Close() }()
		var buf bytes.Buffer
		_, err = buf.ReadFrom(reader)
		require.NoError(t, err)
		assert.Equal(t, "nested", buf.String())
	})

	t.Run("in_root_leaf_symlink_refused", func(t *testing.T) {
		srv, _, project, sharedDir := agentAttachmentServer(t)
		dir := filepath.Join(sharedDir, ".attachments", "sender", "msg1")
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "real.txt"), []byte("target"), 0o644))
		require.NoError(t, os.Symlink("real.txt", filepath.Join(dir, "link.txt")))

		refs, warnings := srv.ingestAgentAttachments(context.Background(), project.ID, "agent-1", []string{
			"/scion-volumes/" + attachmentSharedDirName + "/.attachments/sender/msg1/link.txt",
		})
		assert.Empty(t, refs)
		require.Len(t, warnings, 1)
		assert.Equal(t, string(errAttachmentNotRegular), warnings[0].Reason)
	})

	t.Run("in_root_intermediate_dir_symlink_refused", func(t *testing.T) {
		srv, _, project, sharedDir := agentAttachmentServer(t)
		realDir := filepath.Join(sharedDir, "real")
		require.NoError(t, os.MkdirAll(realDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(realDir, "notes.md"), []byte("via-link"), 0o644))

		parent := filepath.Join(sharedDir, ".attachments", "sender")
		require.NoError(t, os.MkdirAll(parent, 0o755))
		require.NoError(t, os.Symlink("../../real", filepath.Join(parent, "msg1")))

		refs, warnings := srv.ingestAgentAttachments(context.Background(), project.ID, "agent-1", []string{
			"/scion-volumes/" + attachmentSharedDirName + "/.attachments/sender/msg1/notes.md",
		})
		assert.Empty(t, refs, "intermediate in-root symlink must be refused")
		require.Len(t, warnings, 1)
		assert.Equal(t, string(errAttachmentNotRegular), warnings[0].Reason)
	})

	t.Run("fifo_does_not_block", func(t *testing.T) {
		srv, _, project, sharedDir := agentAttachmentServer(t)
		dir := filepath.Join(sharedDir, ".attachments", "sender", "msg1")
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, unix.Mkfifo(filepath.Join(dir, "pipe.txt"), 0o644))

		runWithinTimeout(t, 2*time.Second, func() {
			refs, warnings := srv.ingestAgentAttachments(context.Background(), project.ID, "agent-1", []string{
				"/scion-volumes/" + attachmentSharedDirName + "/.attachments/sender/msg1/pipe.txt",
			})
			assert.Empty(t, refs)
			require.Len(t, warnings, 1)
			assert.Equal(t, string(errAttachmentNotRegular), warnings[0].Reason)
		})
	})

	t.Run("swapped_leaf_symlink_and_fifo_refused", func(t *testing.T) {
		for _, kind := range []string{"symlink", "fifo"} {
			t.Run(kind, func(t *testing.T) {
				srv, _, project, sharedDir := agentAttachmentServer(t)
				dir := filepath.Join(sharedDir, ".attachments", "sender", "msg1")
				require.NoError(t, os.MkdirAll(dir, 0o755))
				target := filepath.Join(dir, "notes.txt")
				require.NoError(t, os.WriteFile(target, []byte("initial"), 0o644))
				require.NoError(t, os.WriteFile(filepath.Join(dir, "other.txt"), []byte("other"), 0o644))

				swapped := false
				hook := func(name string) {
					if swapped || name != ".attachments/sender/msg1/notes.txt" {
						return
					}
					swapped = true
					_ = os.Remove(target)
					if kind == "symlink" {
						_ = os.Symlink("other.txt", target)
					} else {
						_ = unix.Mkfifo(target, 0o644)
					}
				}
				workspaceBeforeOpenHook.Store(&hook)
				t.Cleanup(func() { workspaceBeforeOpenHook.Store(nil) })

				runWithinTimeout(t, 2*time.Second, func() {
					refs, warnings := srv.ingestAgentAttachments(context.Background(), project.ID, "agent-1", []string{
						"/scion-volumes/" + attachmentSharedDirName + "/.attachments/sender/msg1/notes.txt",
					})
					assert.Empty(t, refs)
					require.Len(t, warnings, 1)
					assert.Equal(t, string(errAttachmentNotRegular), warnings[0].Reason)
				})
				assert.True(t, swapped, "expected open hook to run")
			})
		}
	})

	t.Run("error_mapping_unchanged", func(t *testing.T) {
		srv, _, project, sharedDir := agentAttachmentServer(t)
		dir := filepath.Join(sharedDir, ".attachments", "sender", "msg1")
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "subdir"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "page.html"), []byte("<p>hi</p>"), 0o644))

		paths := []string{
			"/scion-volumes/" + attachmentSharedDirName + "/.attachments/sender/msg1/missing.txt",
			"/scion-volumes/" + attachmentSharedDirName + "/.attachments/sender/msg1/subdir",
			"/scion-volumes/" + attachmentSharedDirName + "/.attachments/sender/msg1/page.html",
		}
		wantReasons := []string{
			attachmentWarnNotFound,
			string(errAttachmentNotRegular),
			attachmentWarnBadFilename,
		}
		if os.Geteuid() != 0 {
			noRead := filepath.Join(dir, "noread.txt")
			require.NoError(t, os.WriteFile(noRead, []byte("secret"), 0o000))
			t.Cleanup(func() { _ = os.Chmod(noRead, 0o644) })
			paths = append(paths, "/scion-volumes/"+attachmentSharedDirName+"/.attachments/sender/msg1/noread.txt")
			wantReasons = append(wantReasons, attachmentWarnPermission)
		}

		refs, warnings := srv.ingestAgentAttachments(context.Background(), project.ID, "agent-1", paths)
		assert.Empty(t, refs)
		require.Len(t, warnings, len(wantReasons))
		for i, want := range wantReasons {
			assert.Equal(t, want, warnings[i].Reason, "path=%s", paths[i])
		}
	})
}

func TestAttachmentStaging_OpenPolicy(t *testing.T) {
	t.Run("regular_stage_and_reuse", func(t *testing.T) {
		src := writeTempFile(t, "notes.txt", "hello agent")
		st := newLocalTestStaging(t.TempDir(), false)

		first, err := st.stage(src, "att-1", "notes.txt")
		require.NoError(t, err)
		second, err := st.stage(src, "att-1", "notes.txt")
		require.NoError(t, err)
		assert.Equal(t, first, second)
	})

	t.Run("write_in_root_intermediate_symlink_refused", func(t *testing.T) {
		src := writeTempFile(t, "notes.txt", "hello agent")
		base := t.TempDir()
		st := newLocalTestStaging(base, false)

		targetDir := filepath.Join(base, "other")
		require.NoError(t, os.MkdirAll(targetDir, 0o755))
		require.NoError(t, os.MkdirAll(st.hostDir(), 0o755))
		require.NoError(t, os.Symlink("../../other", filepath.Join(st.hostDir(), "att-1")))

		_, err := st.stage(src, "att-1", "notes.txt")
		require.Error(t, err, "staging write must refuse an intermediate in-root symlink")
		entries, err := os.ReadDir(targetDir)
		require.NoError(t, err)
		assert.Empty(t, entries, "symlink target directory must remain untouched")
	})

	t.Run("verify_in_root_intermediate_symlink_refused", func(t *testing.T) {
		src := writeTempFile(t, "notes.txt", "hello agent")
		base := t.TempDir()
		st := newLocalTestStaging(base, false)

		targetDir := filepath.Join(base, "other")
		require.NoError(t, os.MkdirAll(targetDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(targetDir, "notes.txt"), []byte("hello agent"), 0o644))
		require.NoError(t, os.MkdirAll(st.hostDir(), 0o755))
		require.NoError(t, os.Symlink("../../other", filepath.Join(st.hostDir(), "att-1")))

		_, err := st.stage(src, "att-1", "notes.txt")
		require.ErrorIs(t, err, errStagedCopyMismatch, "staging verify must refuse an intermediate in-root symlink")
	})

	t.Run("verify_in_root_leaf_symlink_refused", func(t *testing.T) {
		src := writeTempFile(t, "notes.txt", "hello agent")
		base := t.TempDir()
		st := newLocalTestStaging(base, false)

		attDir := filepath.Join(st.hostDir(), "att-1")
		require.NoError(t, os.MkdirAll(attDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(attDir, "real.txt"), []byte("hello agent"), 0o644))
		require.NoError(t, os.Symlink("real.txt", filepath.Join(attDir, "notes.txt")))

		_, err := st.stage(src, "att-1", "notes.txt")
		require.ErrorIs(t, err, errStagedCopyMismatch)
	})

	t.Run("verify_swapped_leaf_symlink_and_fifo", func(t *testing.T) {
		for _, kind := range []string{"symlink", "fifo"} {
			t.Run(kind, func(t *testing.T) {
				src := writeTempFile(t, "notes.txt", "hello agent")
				base := t.TempDir()
				st := newLocalTestStaging(base, false)

				attDir := filepath.Join(st.hostDir(), "att-1")
				require.NoError(t, os.MkdirAll(attDir, 0o755))
				dest := filepath.Join(attDir, "notes.txt")
				require.NoError(t, os.WriteFile(dest, []byte("hello agent"), 0o644))
				require.NoError(t, os.WriteFile(filepath.Join(attDir, "other.txt"), []byte("hello agent"), 0o644))

				swapped := false
				hook := func(name string) {
					if swapped || name != ".attachments/_webchat/att-1/notes.txt" {
						return
					}
					swapped = true
					_ = os.Remove(dest)
					if kind == "symlink" {
						_ = os.Symlink("other.txt", dest)
					} else {
						_ = unix.Mkfifo(dest, 0o644)
					}
				}
				workspaceBeforeOpenHook.Store(&hook)
				t.Cleanup(func() { workspaceBeforeOpenHook.Store(nil) })

				runWithinTimeout(t, 2*time.Second, func() {
					_, err := st.stage(src, "att-1", "notes.txt")
					require.ErrorIs(t, err, errStagedCopyMismatch)
				})
				assert.True(t, swapped, "expected open hook to run")
			})
		}
	})

	t.Run("verify_swapped_unix_socket", func(t *testing.T) {
		src := writeTempFile(t, "n.txt", "hello agent")
		base, err := os.MkdirTemp("/tmp", "att-sock-*")
		require.NoError(t, err)
		t.Cleanup(func() { _ = os.RemoveAll(base) })
		st := newLocalTestStaging(base, false)

		attDir := filepath.Join(st.hostDir(), "s")
		require.NoError(t, os.MkdirAll(attDir, 0o755))
		dest := filepath.Join(attDir, "n.txt")
		require.NoError(t, os.WriteFile(dest, []byte("hello agent"), 0o644))

		var ln net.Listener
		t.Cleanup(func() {
			if ln != nil {
				_ = ln.Close()
			}
		})
		swapped := false
		hook := func(name string) {
			if swapped || name != ".attachments/_webchat/s/n.txt" {
				return
			}
			swapped = true
			_ = os.Remove(dest)
			ln, _ = net.Listen("unix", dest)
		}
		workspaceBeforeOpenHook.Store(&hook)
		t.Cleanup(func() { workspaceBeforeOpenHook.Store(nil) })

		runWithinTimeout(t, 2*time.Second, func() {
			_, stageErr := st.stage(src, "s", "n.txt")
			require.ErrorIs(t, stageErr, errStagedCopyMismatch)
		})
		assert.True(t, swapped, "expected open hook to run")
	})

	t.Run("write_swapped_intermediate_symlink_refused", func(t *testing.T) {
		src := writeTempFile(t, "notes.txt", "hello agent")
		base := t.TempDir()
		st := newLocalTestStaging(base, false)

		targetDir := filepath.Join(base, "other")
		require.NoError(t, os.MkdirAll(targetDir, 0o755))
		attDir := filepath.Join(st.hostDir(), "att-swap")

		swapped := false
		hook := func(name string) {
			if swapped {
				return
			}
			swapped = true
			_ = os.RemoveAll(attDir)
			_ = os.Symlink("../../other", attDir)
		}
		workspaceBeforeOpenHook.Store(&hook)
		t.Cleanup(func() { workspaceBeforeOpenHook.Store(nil) })

		_, err := st.stage(src, "att-swap", "notes.txt")
		require.Error(t, err)
		assert.True(t, swapped, "expected open hook to run")
		entries, err := os.ReadDir(targetDir)
		require.NoError(t, err)
		assert.Empty(t, entries, "symlink target directory must remain untouched")
	})
}
