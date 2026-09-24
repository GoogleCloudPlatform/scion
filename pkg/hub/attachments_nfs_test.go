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

//go:build !no_sqlite

package hub

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Chat attachments are not available for NFS-backed shared dirs in this
// release: sharedDirHostPath returns ("", false) immediately for an
// NFS-backed shared dir, with no fallback, so neither ingest
// (storeAgentAttachment) nor staging (attachmentStaging.stage) ever runs
// against the NFS export (see the comment on sharedDirHostPath).

// nfsAttachmentServer mirrors agentAttachmentServer, but with
// server.shared_dir_storage: nfs configured in global settings -- which
// must be written, and HOME set, BEFORE testServer(t) runs (see
// setNFSSharedDirStorageGlobalSettings's own docstring).
func nfsAttachmentServer(t *testing.T) (srv *Server, project *store.Project, hostBase string) {
	t.Helper()

	hostBase = setNFSSharedDirStorageGlobalSettings(t)
	srv, s := testServer(t)

	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	wcs := NewWebChatStore(db, "sqlite3")
	require.NoError(t, wcs.Init())
	srv.SetWebChatStore(wcs)

	as, err := NewLocalDiskAttachmentStore(t.TempDir())
	require.NoError(t, err)
	srv.SetAttachmentStore(as)

	project = &store.Project{
		ID:         api.NewUUID(),
		Name:       "attach-nfs-project",
		Slug:       "attach-nfs-project",
		SharedDirs: []api.SharedDir{{Name: attachmentSharedDirName}},
	}
	require.NoError(t, s.CreateProject(context.Background(), project))

	return srv, project, hostBase
}

// TestSharedDirHostPath_NFSBackend_Refused: an NFS-backed shared dir must
// never be returned as an attachment host path, even though it resolves
// successfully and exists on disk.
func TestSharedDirHostPath_NFSBackend_Refused(t *testing.T) {
	srv, project, hostBase := nfsAttachmentServer(t)

	leaf := filepath.Join(hostBase, "projects", project.ID, "shared-dirs", attachmentSharedDirName)
	require.NoError(t, os.MkdirAll(leaf, 0o2775))

	hostPath, inWorkspace := srv.sharedDirHostPath(context.Background(), project.ID, attachmentSharedDirName)
	assert.Empty(t, hostPath, "an NFS-backed shared dir must never be used for attachments (not yet confined to os.Root)")
	assert.False(t, inWorkspace)
}

// TestSharedDirHostPath_NFSBackend_NoFallbackToStaleLocalDir: a stale local
// project-configs directory -- e.g. left over from before the project moved
// to shared_dir_storage: nfs -- must never be used as an attachment staging
// target once the project is NFS-backed. sharedDirHostPath returns
// immediately on an NFS backend, before ever reaching the conventional
// local-layout fallback candidate, even though that candidate exists on
// disk here.
func TestSharedDirHostPath_NFSBackend_NoFallbackToStaleLocalDir(t *testing.T) {
	srv, project, hostBase := nfsAttachmentServer(t)

	leaf := filepath.Join(hostBase, "projects", project.ID, "shared-dirs", attachmentSharedDirName)
	require.NoError(t, os.MkdirAll(leaf, 0o2775))

	// A stale local project-configs directory, computed the same way
	// config.SharedDirHostPath does, with a marker file inside it.
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	staleLocalDir := config.SharedDirHostPath(home, project.Slug, project.ID, attachmentSharedDirName)
	require.NoError(t, os.MkdirAll(staleLocalDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(staleLocalDir, "marker.txt"), []byte("stale"), 0o644))

	hostPath, inWorkspace := srv.sharedDirHostPath(context.Background(), project.ID, attachmentSharedDirName)
	assert.Empty(t, hostPath, "an NFS-backed project must never fall back to the stale local project-configs dir")
	assert.False(t, inWorkspace)

	// The staging half, end to end: no attachment ever lands in the stale dir.
	staging := srv.resolveAttachmentStaging(context.Background(), project.ID)
	require.Nil(t, staging, "staging must be unavailable, not silently backed by the stale local dir")

	entries, err := os.ReadDir(staleLocalDir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "the stale local dir must be untouched, only the marker file present")
	assert.Equal(t, "marker.txt", entries[0].Name())
}

// TestResolveAttachmentStaging_NFSBackend_Refused is the staging half: with
// server.shared_dir_storage: nfs configured, resolveAttachmentStaging must
// report unavailable, never returning a staging target backed by the
// unconfined path.
func TestResolveAttachmentStaging_NFSBackend_Refused(t *testing.T) {
	srv, project, hostBase := nfsAttachmentServer(t)

	leaf := filepath.Join(hostBase, "projects", project.ID, "shared-dirs", attachmentSharedDirName)
	require.NoError(t, os.MkdirAll(leaf, 0o2775))

	assert.Nil(t, srv.resolveAttachmentStaging(context.Background(), project.ID))
}

// TestIngestAgentAttachments_NFSBackend_RefusedNoLeak: a directory symlink
// planted inside an NFS-backed leaf, pointing at a victim outside the
// export -- if ingest read through that symlink, publishing a path through
// it would read (and then publish into the chat) the victim's file. The NFS
// refusal must stop this before any filesystem access through the symlink
// happens at all.
func TestIngestAgentAttachments_NFSBackend_RefusedNoLeak(t *testing.T) {
	srv, project, hostBase := nfsAttachmentServer(t)
	outside := newOutsideTree(t)

	leaf := filepath.Join(hostBase, "projects", project.ID, "shared-dirs", attachmentSharedDirName)
	require.NoError(t, os.MkdirAll(leaf, 0o2775))
	require.NoError(t, os.Symlink(outside.dir, filepath.Join(leaf, "escape")))

	before := outside.snapshot(t)
	refs := srv.ingestAgentAttachments(context.Background(), project.ID, "agent-1", []string{
		"/scion-volumes/" + attachmentSharedDirName + "/escape/secret.txt",
	})
	assert.Empty(t, refs, "NFS-backed attachment ingest must be refused outright")
	outside.assertIntact(t, before)
}

// TestSharedDirHostPath_NFSResolutionError_SymlinkedPid_NoFallback: when the
// project's shared_dir_storage is nfs-configured but resolution itself
// errors -- here, a symlinked <pid> component with an existing victim leaf,
// caught by resolveNFSSharedDirPath's own symlink-escape backstop --
// sharedDirHostPath must fail closed and never fall back to a stale local
// project-configs directory, even though that directory exists on disk.
func TestSharedDirHostPath_NFSResolutionError_SymlinkedPid_NoFallback(t *testing.T) {
	srv, project, hostBase := nfsAttachmentServer(t)

	victim := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(victim, "shared-dirs", attachmentSharedDirName), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(hostBase, "projects"), 0o755))
	require.NoError(t, os.Symlink(victim, filepath.Join(hostBase, "projects", project.ID)))

	home, err := os.UserHomeDir()
	require.NoError(t, err)
	staleLocalDir := config.SharedDirHostPath(home, project.Slug, project.ID, attachmentSharedDirName)
	require.NoError(t, os.MkdirAll(staleLocalDir, 0o755))

	hostPath, inWorkspace := srv.sharedDirHostPath(context.Background(), project.ID, attachmentSharedDirName)
	assert.Empty(t, hostPath, "an NFS resolution error must fail closed, never fall back to a local directory")
	assert.False(t, inWorkspace)

	staging := srv.resolveAttachmentStaging(context.Background(), project.ID)
	require.Nil(t, staging, "staging must be unavailable when NFS resolution errors")
}

// TestSharedDirHostPath_NFSResolutionError_InvalidBlock_NoFallback: the same
// fail-closed requirement, this time triggered by an nfs block that fails
// V1SharedDirStorageConfig.Validate() (a missing mount_root).
func TestSharedDirHostPath_NFSResolutionError_InvalidBlock_NoFallback(t *testing.T) {
	srv, project, _ := nfsAttachmentServer(t)

	home, err := os.UserHomeDir()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(home, ".scion", "settings.yaml"),
		[]byte("schema_version: \"1\"\nserver:\n  shared_dir_storage:\n    backend: nfs\n    nfs:\n      shares:\n        - id: nfs-export\n"), 0o644))

	staleLocalDir := config.SharedDirHostPath(home, project.Slug, project.ID, attachmentSharedDirName)
	require.NoError(t, os.MkdirAll(staleLocalDir, 0o755))

	hostPath, inWorkspace := srv.sharedDirHostPath(context.Background(), project.ID, attachmentSharedDirName)
	assert.Empty(t, hostPath, "an invalid nfs block must fail closed, never fall back to a local directory")
	assert.False(t, inWorkspace)
}

// TestSharedDirHostPath_NFSBackend_SkipLoggedOncePerProject: the Info log
// noting that chat attachments are not staged into an NFS-backed shared dir
// must be emitted once per project, not once per call -- a second call for
// the same project must not add a second log line.
func TestSharedDirHostPath_NFSBackend_SkipLoggedOncePerProject(t *testing.T) {
	buf := captureSlog(t)
	srv, project, hostBase := nfsAttachmentServer(t)

	leaf := filepath.Join(hostBase, "projects", project.ID, "shared-dirs", attachmentSharedDirName)
	require.NoError(t, os.MkdirAll(leaf, 0o2775))

	_, _ = srv.sharedDirHostPath(context.Background(), project.ID, attachmentSharedDirName)
	_, _ = srv.sharedDirHostPath(context.Background(), project.ID, attachmentSharedDirName)

	count := strings.Count(buf.String(), "chat attachments are not staged into NFS-backed shared dirs")
	assert.Equal(t, 1, count, "the skip log must be emitted exactly once across two calls for the same project")
}

// TestSharedDirHostPath_NFSBackend_ResolveErrorWarnLoggedIndependentlyOfSkipLog:
// the Info skip log (successful NFS resolution) and the Warn resolution-error
// log dedup independently, keyed separately, so a project that later starts
// failing to resolve still gets its Warn even though its Info line already
// fired once.
func TestSharedDirHostPath_NFSBackend_ResolveErrorWarnLoggedIndependentlyOfSkipLog(t *testing.T) {
	buf := captureSlog(t)
	srv, project, hostBase := nfsAttachmentServer(t)

	leaf := filepath.Join(hostBase, "projects", project.ID, "shared-dirs", attachmentSharedDirName)
	require.NoError(t, os.MkdirAll(leaf, 0o2775))

	// First call: NFS resolution succeeds, the Info skip log fires once.
	_, _ = srv.sharedDirHostPath(context.Background(), project.ID, attachmentSharedDirName)
	infoCount := strings.Count(buf.String(), "chat attachments are not staged into NFS-backed shared dirs")
	require.Equal(t, 1, infoCount, "sanity: the Info skip log must have fired once")

	// Now the same project's NFS-configured resolution starts failing
	// (invalid block, mount_root missing) -- the Warn must still fire, not
	// be suppressed by the Info log's own dedup key.
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(home, ".scion", "settings.yaml"),
		[]byte("schema_version: \"1\"\nserver:\n  shared_dir_storage:\n    backend: nfs\n    nfs:\n      shares:\n        - id: nfs-export\n"), 0o644))

	_, _ = srv.sharedDirHostPath(context.Background(), project.ID, attachmentSharedDirName)
	warnCount := strings.Count(buf.String(), "NFS-configured shared dir resolution failed")
	assert.Equal(t, 1, warnCount, "the resolution-error Warn must fire despite the project's Info log already having fired")

	// A second call on the same still-failing path must not add a second
	// Warn line: the resolution-error log dedups per project too, exactly
	// like the Info log does.
	_, _ = srv.sharedDirHostPath(context.Background(), project.ID, attachmentSharedDirName)
	warnCount = strings.Count(buf.String(), "NFS-configured shared dir resolution failed")
	assert.Equal(t, 1, warnCount, "the resolution-error Warn must still be deduped across repeated calls")
}

// TestAttachmentStaging_NFSBackend_NoOutsideWrite is the staging half of the
// same requirement: staging must not be attempted at all for an NFS-backed
// project (resolveAttachmentStaging returns nil, so stage() is never
// called), which this confirms end to end by checking nothing is ever
// written under the NFS leaf's .attachments directory.
func TestAttachmentStaging_NFSBackend_NoOutsideWrite(t *testing.T) {
	srv, project, hostBase := nfsAttachmentServer(t)

	leaf := filepath.Join(hostBase, "projects", project.ID, "shared-dirs", attachmentSharedDirName)
	require.NoError(t, os.MkdirAll(leaf, 0o2775))

	staging := srv.resolveAttachmentStaging(context.Background(), project.ID)
	require.Nil(t, staging, "staging must be unavailable for an NFS-backed shared dir")

	_, statErr := os.Stat(filepath.Join(leaf, ".attachments"))
	assert.True(t, os.IsNotExist(statErr), "nothing should ever be staged under the NFS leaf")
}
